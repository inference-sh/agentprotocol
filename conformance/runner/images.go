package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/harness"
)

// probeImages sends a prompt with a PNG attached, as a file outside the
// repository the way a host hands the driver a downloaded attachment, and
// reads the requests the model was sent.
//
// The backend's Capabilities.Images is the claim being checked. Claimed, the
// image has to reach the model as image data; not claimed, its path has to
// be in the request, so the agent can open it. An ACP agent the registry
// does not claim images for is then sent the image anyway: if it takes it,
// that is a finding, and harness.Harness.ACPImages should be set.
//
// Agents re-encode images: droid sends an 8x8 PNG as a different PNG, omp
// scales it to 200x200. So an image counts as arrived when a request carries
// an image with this one's pixels, at any size and in any encoding.
func (r *TestRunner) probeImages() {
	if r.harness.DriverKind() == "" || r.server == nil {
		return
	}
	fmt.Println("[probe] images (an attached image reaching the model)")
	name := r.harness.Name
	claimed := r.sessionBackend(false, false)(func(string) {}).Capabilities().Images

	dir, err := os.MkdirTemp("", "attachments-")
	if err != nil {
		r.fail("images.write", "images: "+err.Error())
		return
	}
	defer os.RemoveAll(dir)
	raw := probePNG()
	path := filepath.Join(dir, "probe-image.png")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		r.fail("images.write", "images: "+err.Error())
		return
	}
	in := driver.Input{Text: promptText, Files: []ap.FileRef{{
		URI: "file://" + path, Filename: "probe-image.png", ContentType: "image/png", Size: int64(len(raw)),
	}}}

	got, ok := r.imageTurn(in, raw, path)
	if !ok {
		return
	}
	switch {
	case claimed && got.image != "":
		r.pass("images.sent", fmt.Sprintf("images: %s sent the attached image to the model as image data (%s)", name, got.image))
	case claimed:
		r.fail("images.sent", fmt.Sprintf("images: %s's backend reports Capabilities.Images and the image never reached the model (its path did: %t)", name, got.named))
	case got.named:
		r.pass("images.named", fmt.Sprintf("images: %s was sent the image's path, not the image", name))
	default:
		r.fail("images.named", fmt.Sprintf("images: %s was sent neither the image nor its path", name))
	}
	if claimed || r.harness.DriverKind() != harness.DriverACP {
		return
	}

	r.acpImages = true
	defer func() { r.acpImages = false }()
	got, ok = r.imageTurn(in, raw, path)
	if !ok {
		return
	}
	if got.image != "" {
		r.finding("images.unclaimed", fmt.Sprintf("images: %s took the image as an image block (%s); the registry does not set ACPImages", name, got.image))
		return
	}
	r.pass("images.declined", fmt.Sprintf("images: sent an image block anyway, %s did not pass it to the model", name))
}

// imageResult is what the model was sent of an attached image.
type imageResult struct {
	image string // how the image arrived, or "" when it did not
	named bool   // its path was in a request
}

// imageTurn runs one turn in its own session with in and reads the requests.
func (r *TestRunner) imageTurn(in driver.Input, raw []byte, path string) (imageResult, bool) {
	r.server.ClearLog()
	d := r.newSession(false, false)
	if err := d.Start(); err != nil {
		r.skip("images.start", "images: "+r.sessionLabel()+" start: "+err.Error())
		return imageResult{}, false
	}
	defer d.Close()
	time.Sleep(2 * time.Second)
	answered := r.server.AnswersServed()
	if err := d.SendInput(in); err != nil {
		r.skip("images.prompt", "images: "+r.sessionLabel()+" prompt: "+err.Error())
		return imageResult{}, false
	}
	d.WaitAnswered(r.server.AnswersServed, answered, 60*time.Second)
	d.WaitIdle(2 * time.Second)

	var res imageResult
	for _, e := range r.server.Log() {
		body := string(e.Body)
		res.named = res.named || strings.Contains(body, path)
		if res.image == "" {
			res.image = findProbeImage(body, raw)
		}
	}
	return res, true
}

// base64Run is a run of base64 long enough to be an image.
var base64Run = regexp.MustCompile(`[A-Za-z0-9+/]{64,}={0,2}`)

// findProbeImage looks through a request body for the probe image and says
// how it arrived: as sent, or re-encoded at some size.
func findProbeImage(body string, raw []byte) string {
	if strings.Contains(body, base64.StdEncoding.EncodeToString(raw)) {
		return "as attached"
	}
	for _, m := range base64Run.FindAllString(body, -1) {
		b, err := base64.StdEncoding.DecodeString(m)
		if err != nil {
			continue
		}
		img, format, err := image.Decode(bytes.NewReader(b))
		if err != nil || !isProbeImage(img) {
			continue
		}
		size := img.Bounds().Size()
		return fmt.Sprintf("re-encoded as %s, %dx%d", format, size.X, size.Y)
	}
	return ""
}

// probeSide is the probe image's width and height in cells.
const probeSide = 8

// probeColor is the probe image's colour at cell (x, y).
func probeColor(x, y int) color.RGBA {
	return color.RGBA{uint8(31 * x), uint8(29 * y), 197, 255}
}

// probePNG is a small image no request carries by chance.
func probePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, probeSide, probeSide))
	for x := 0; x < probeSide; x++ {
		for y := 0; y < probeSide; y++ {
			img.Set(x, y, probeColor(x, y))
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// isProbeImage reports whether img is the probe image at any scale: the
// centre of each cell has that cell's colour, within what a lossy encoding
// changes.
func isProbeImage(img image.Image) bool {
	r := img.Bounds()
	if r.Dx() < probeSide || r.Dy() < probeSide {
		return false
	}
	for x := 0; x < probeSide; x++ {
		for y := 0; y < probeSide; y++ {
			px := r.Min.X + (2*x+1)*r.Dx()/(2*probeSide)
			py := r.Min.Y + (2*y+1)*r.Dy()/(2*probeSide)
			got := color.RGBAModel.Convert(img.At(px, py)).(color.RGBA)
			want := probeColor(x, y)
			if far(got.R, want.R) || far(got.G, want.G) || far(got.B, want.B) {
				return false
			}
		}
	}
	return true
}

func far(a, b uint8) bool {
	d := int(a) - int(b)
	return d > 16 || d < -16
}

// SendInput is SendPrompt with attachments.
func (d *SessionDriver) SendInput(in driver.Input) error {
	d.mu.Lock()
	d.turnEnded = false
	d.mu.Unlock()
	return d.sess.Prompt(context.Background(), in)
}
