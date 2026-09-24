package kimi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The image sample is Kimi Code over ACP in the harness-test container: a
// prompt with a PNG attached, then a mock ReadMediaFile call on a PNG in the
// repository, whose result is the image between two text parts.
const (
	imageHome = "testdata/image"
	imageCWD  = "/tmp/harness-test-kimi-2139596651/test-repo"
	imageID   = "session_bfa348ba-9b90-40eb-8cd0-41adf0589271"
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
	smp := transcripttest.Sample{Home: imageHome, CWD: imageCWD, ID: imageID}
	transcripttest.RoundTrip(t, Codec, smp)
	transcripttest.Imported(t, Codec, smp)
}

// The attached image is an image_url part with a data: URL: an image block
// of the prompt, for the person and the model.
func TestImagePrompt(t *testing.T) {
	s := readImage(t)
	for _, view := range [][]transcript.Entry{s.Linearize(), s.Context()} {
		var prompt *transcript.Entry
		for i, e := range view {
			if e.Role == transcript.RoleUser && e.Text() == "What is in this image?" {
				prompt = &view[i]
			}
		}
		if prompt == nil || len(prompt.Content) != 2 {
			t.Fatalf("prompt = %+v, want the text and the image", prompt)
		}
		img := prompt.Content[1]
		if img.Kind != transcript.BlockImage || img.MediaType != "image/png" || !bytes.HasPrefix(img.Data, pngMagic) {
			t.Errorf("image = %s %s %.8q, want a PNG", img.Kind, img.MediaType, img.Data)
		}
	}
}

// ReadMediaFile's image follows the result's text, with the call's id.
func TestToolImage(t *testing.T) {
	for _, e := range readImage(t).Context() {
		if e.Role != transcript.RoleTool {
			continue
		}
		if len(e.Content) != 2 {
			t.Fatalf("tool entry = %+v, want the result and its image", e.Content)
		}
		res, img := e.Content[0], e.Content[1]
		if !strings.HasPrefix(res.Text, "<image path=") || !strings.HasSuffix(res.Text, "</image>") {
			t.Errorf("result text = %q", res.Text)
		}
		if img.Kind != transcript.BlockImage || img.ToolID != "call_mock_1" || !bytes.HasPrefix(img.Data, pngMagic) {
			t.Errorf("image = %s %q, want the PNG call_mock_1 returned", img.Kind, img.ToolID)
		}
		return
	}
	t.Fatal("no tool entry")
}

// Written for kimi from another agent, an image is an image_url part with a
// data: URL, in the prompt or among a tool result's parts; a document kimi
// has no part for is left out.
func TestWriteMedia(t *testing.T) {
	in := &transcript.Session{Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF")},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c", Name: "ReadMediaFile", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c", Text: "read", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c", MediaType: "image/png", Data: pngMagic},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a picture"}}},
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
	ctx := s.Context()
	if len(ctx) != 4 {
		t.Fatalf("context = %d entries", len(ctx))
	}
	want := [][]transcript.Block{
		{{Kind: transcript.BlockText, Text: "look"}, {Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic}},
		nil,
		{{Kind: transcript.BlockToolResult, ToolID: "c", Text: "read"}, {Kind: transcript.BlockImage, ToolID: "c", MediaType: "image/png", Data: pngMagic}},
	}
	for i, w := range want {
		if w == nil {
			continue
		}
		got := ctx[i].Content
		if len(got) != len(w) {
			t.Fatalf("entry %d = %+v, want %+v", i, got, w)
		}
		for j := range w {
			x, y := got[j], w[j]
			if x.Kind != y.Kind || x.Text != y.Text || x.ToolID != y.ToolID || x.MediaType != y.MediaType || !bytes.Equal(x.Data, y.Data) {
				t.Errorf("entry %d block %d = %+v, want %+v", i, j, x, y)
			}
		}
	}
}
