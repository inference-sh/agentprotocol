package runner

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"strings"
	"testing"
)

// The probe image is found as attached, re-encoded, and scaled, and not in a
// request that carries some other image.
func TestFindProbeImage(t *testing.T) {
	raw := probePNG()
	if got := findProbeImage(`{"data":"`+base64.StdEncoding.EncodeToString(raw)+`"}`, raw); got != "as attached" {
		t.Errorf("as attached: got %q", got)
	}

	src, _, _ := image.Decode(bytes.NewReader(raw))
	big := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for x := 0; x < 200; x++ {
		for y := 0; y < 200; y++ {
			big.Set(x, y, src.At(x*8/200, y*8/200))
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, big, &jpeg.Options{Quality: 90})
	got := findProbeImage(`"image_url":"data:image/jpeg;base64,`+base64.StdEncoding.EncodeToString(b.Bytes())+`"`, raw)
	if !strings.HasPrefix(got, "re-encoded as jpeg, 200x200") {
		t.Errorf("scaled jpeg: got %q", got)
	}

	other := image.NewRGBA(image.Rect(0, 0, 8, 8))
	b.Reset()
	_ = jpeg.Encode(&b, other, nil)
	if got := findProbeImage(base64.StdEncoding.EncodeToString(b.Bytes()), raw); got != "" {
		t.Errorf("another image matched: %q", got)
	}
}
