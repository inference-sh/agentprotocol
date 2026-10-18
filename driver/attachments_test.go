package driver

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ap "github.com/inference-sh/agentprotocol"
)

// attachments is an input with a local image, an image only reachable by
// URL, and a document, each of which every driver has to pass on.
func attachments(t *testing.T) (Input, string, string) {
	t.Helper()
	dir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\nnot really")
	img := filepath.Join(dir, "shot.png")
	doc := filepath.Join(dir, "notes.pdf")
	os.WriteFile(img, png, 0o644)
	os.WriteFile(doc, []byte("%PDF"), 0o644)
	return Input{Text: "look", Files: []ap.FileRef{
		{URI: "file://" + img, Filename: "shot.png", ContentType: "image/png"},
		{URI: "https://cloud.example/x.jpg", Filename: "x.jpg", ContentType: "image/jpeg"},
		{URI: "file://" + doc, Filename: "notes.pdf", ContentType: "application/pdf"},
	}}, base64.StdEncoding.EncodeToString(png), dir
}

func TestImageOfReadsLocalAndDataImagesOnly(t *testing.T) {
	in, data, dir := attachments(t)
	if img, ok := imageOf(in.Files[0]); !ok || img.Data != data || img.MimeType != "image/png" {
		t.Errorf("local image: %+v %v", img, ok)
	}
	if _, ok := imageOf(in.Files[1]); ok {
		t.Error("a URL was inlined; drivers read local bytes only")
	}
	if _, ok := imageOf(in.Files[2]); ok {
		t.Error("a PDF was inlined as an image")
	}
	if img, ok := imageOf(ap.FileRef{URI: "data:image/gif;base64,R0lG"}); !ok || img.MimeType != "image/gif" || img.Data != "R0lG" {
		t.Errorf("data URI: %+v %v", img, ok)
	}
	big := filepath.Join(dir, "big.png")
	os.WriteFile(big, make([]byte, maxImageBytes+1), 0o644)
	if _, ok := imageOf(ap.FileRef{URI: "file://" + big, ContentType: "image/png"}); ok {
		t.Error("an image over maxImageBytes was inlined")
	}
	if _, ok := imageOf(ap.FileRef{URI: "file://other-host/x.png", ContentType: "image/png"}); ok {
		t.Error("a file:// URI on another host was read locally")
	}
}

// The rest are named in the text by local path, where they have one.
func wantNamed(t *testing.T, text, dir string) {
	t.Helper()
	for _, want := range []string{"look", "x.jpg (https://cloud.example/x.jpg)", "notes.pdf (" + filepath.Join(dir, "notes.pdf") + ")"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q is missing %q", text, want)
		}
	}
	if strings.Contains(text, "shot.png") {
		t.Errorf("text %q names the image that went as an image", text)
	}
}

func TestClaudeSendsLocalImagesAsImageBlocks(t *testing.T) {
	in, data, dir := attachments(t)
	blocks := claudeBlocks(in)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if s := blocks[1].Source; s == nil || s.Type != "base64" || s.MediaType != "image/png" || s.Data != data {
		t.Errorf("image source = %+v", blocks[1].Source)
	}
	wantNamed(t, blocks[0].Text, dir)
}

func TestPiSendsLocalImagesWithThePrompt(t *testing.T) {
	in, data, dir := attachments(t)
	text, images := piInput(in)
	if len(images) != 1 || images[0].Type != "image" || images[0].MimeType != "image/png" || images[0].Data != data {
		t.Fatalf("images = %+v", images)
	}
	wantNamed(t, text, dir)
}

func TestACPSendsImageBlocksOnlyWhenTheAgentTakesThem(t *testing.T) {
	in, data, dir := attachments(t)
	blocks := promptBlocks(in, true)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image" || blocks[1].Data != data || blocks[1].MimeType != "image/png" || blocks[1].URI != in.Files[0].URI {
		t.Fatalf("with images: %+v", blocks)
	}
	wantNamed(t, blocks[0].Text, dir)

	blocks = promptBlocks(in, false)
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("without images: %+v", blocks)
	}
	if want := "shot.png (" + filepath.Join(dir, "shot.png") + ")"; !strings.Contains(blocks[0].Text, want) {
		t.Errorf("text %q does not name the image the agent was not sent, %q", blocks[0].Text, want)
	}
}

func TestCodexSendsImagesByURLAndPath(t *testing.T) {
	in, _, dir := attachments(t)
	out := userInput(in)
	if len(out) != 3 {
		t.Fatalf("inputs = %+v", out)
	}
	text, ok := out[0].AsTextUserInput()
	if !ok {
		t.Fatalf("first input is not text: %+v", out[0])
	}
	if l, ok := out[1].AsLocalImageUserInput(); !ok || l.Path != filepath.Join(dir, "shot.png") {
		t.Errorf("local image = %+v", out[1])
	}
	if u, ok := out[2].AsImageUserInput(); !ok || u.URL == nil || *u.URL != "https://cloud.example/x.jpg" {
		t.Errorf("URL image = %+v", out[2])
	}
	if !strings.Contains(text.Text, "notes.pdf ("+filepath.Join(dir, "notes.pdf")+")") || strings.Contains(text.Text, "x.jpg") {
		t.Errorf("text = %q", text.Text)
	}
}
