package droid

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The image sample is droid 0.226 over ACP in the harness-test container: a
// prompt with a PNG attached, then a mock Read call on a PNG in the
// repository, whose result carries the image.
const (
	imageHome = "testdata/image"
	imageCWD  = "/tmp/harness-test-droid-3831131475/test-repo"
	imageID   = "784f13d1-ba50-4bea-bf86-c4f813e39f4f"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func readImage(t *testing.T) *transcript.Session {
	t.Helper()
	st, err := Codec.Open(imageHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), imageID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImageSampleRoundTrip(t *testing.T) {
	transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: imageHome, CWD: imageCWD, ID: imageID})
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: imageHome, CWD: imageCWD, ID: imageID})
}

// The attached image is an image block of the prompt, and the note droid
// adds with its local path is the model's, not the person's.
func TestImagePrompt(t *testing.T) {
	s := readImage(t)
	var prompt *transcript.Entry
	for _, e := range s.Linearize() {
		if e.Role == transcript.RoleUser {
			prompt = &e
			break
		}
	}
	if prompt == nil {
		t.Fatal("no prompt")
	}
	if len(prompt.Content) != 2 {
		t.Fatalf("prompt = %+v, want the image and the text", prompt.Content)
	}
	img := prompt.Content[0]
	if img.Kind != transcript.BlockImage || img.MediaType != "image/png" || !bytes.HasPrefix(img.Data, pngMagic) {
		t.Errorf("first block = %s %s %.8q, want a PNG image", img.Kind, img.MediaType, img.Data)
	}
	if prompt.Content[1].Text != "What is in this image?" {
		t.Errorf("text = %q", prompt.Content[1].Text)
	}
	if len(prompt.ModelContent) != 3 || !strings.Contains(prompt.ModelContent[1].Text, "Attached image local file paths") {
		t.Errorf("model content = %+v, want the image, droid's path note and the text", prompt.ModelContent)
	}
}

// An image the Read tool returned follows its result, with the call's id.
func TestToolImage(t *testing.T) {
	s := readImage(t)
	for _, e := range s.Messages() {
		if e.Role != transcript.RoleTool {
			continue
		}
		if len(e.Content) != 2 {
			t.Fatalf("tool entry = %+v, want the result and its image", e.Content)
		}
		res, img := e.Content[0], e.Content[1]
		if res.Kind != transcript.BlockToolResult || !strings.HasPrefix(res.Text, "Image file: pic.png") {
			t.Errorf("result = %+v", res)
		}
		if img.Kind != transcript.BlockImage || img.ToolID != res.ToolID || img.MediaType != "image/png" || !bytes.HasPrefix(img.Data, pngMagic) {
			t.Errorf("image = %s %q %s, want a PNG for call %q", img.Kind, img.ToolID, img.MediaType, res.ToolID)
		}
		return
	}
	t.Fatal("no tool entry")
}

// Written for droid from another agent, images and documents are droid's
// image and document blocks, a tool's inside its tool_result; what droid
// cannot hold (an image by path, a file of another type) is left out.
func TestWriteMedia(t *testing.T) {
	pdf := []byte("%PDF-1.4 x")
	in := &transcript.Session{ID: "s", Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{ID: "u", Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic},
			{Kind: transcript.BlockImage, MediaType: "image/png", URI: "/tmp/a.png"},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: pdf, Name: "a.pdf"},
			{Kind: transcript.BlockFile, MediaType: "text/markdown", Data: []byte("# hi"), Name: "a.md"},
			{Kind: transcript.BlockFile, MediaType: "application/zip", Data: []byte("PK")},
		}},
		{ID: "a", Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c", Name: "Read", Input: []byte(`{}`)}}},
		{ID: "t", Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c", Text: "read", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c", MediaType: "image/png", Data: pngMagic},
		}},
	}}
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	got := msgs[0].Content
	want := []transcript.Block{
		{Kind: transcript.BlockText, Text: "look"},
		{Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic},
		{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: pdf, Name: "a.pdf"},
		{Kind: transcript.BlockFile, MediaType: "text/markdown", Data: []byte("# hi"), Name: "a.md"},
	}
	if !sameBlocks(got, want) {
		t.Errorf("prompt = %+v\nwant %+v", got, want)
	}
	tool := msgs[2].Content
	if len(tool) != 2 || tool[0].Text != "read" || tool[1].Kind != transcript.BlockImage || tool[1].ToolID != "c" || !bytes.Equal(tool[1].Data, pngMagic) {
		t.Errorf("tool = %+v, want the result then its image", tool)
	}
}

func sameBlocks(a, b []transcript.Block) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.Text != y.Text || x.ToolID != y.ToolID || x.MediaType != y.MediaType || x.Name != y.Name || x.URI != y.URI || !bytes.Equal(x.Data, y.Data) {
			return false
		}
	}
	return true
}
