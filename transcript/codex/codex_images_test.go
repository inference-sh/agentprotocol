package codex_test

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/codex"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The images sample is a codex exec run of codex 0.156.1 in the
// harness-test container: a prompt with a PNG attached through -i, answered
// by a mock view_image call on another PNG.
const (
	imagesCWD = "/tmp/harness-test-codex-2901799383/test-repo"
	imagesID  = "01a0d2e7-4559-78b3-a835-f49e78b998c0"
)

var images = transcripttest.Sample{Home: "testdata/home", CWD: imagesCWD, ID: imagesID}

func kinds(bs []transcript.Block) []transcript.BlockKind {
	var out []transcript.BlockKind
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

func isPNG(b transcript.Block) bool {
	return b.Kind == transcript.BlockImage && b.MediaType == "image/png" && bytes.HasPrefix(b.Data, []byte("\x89PNG\r\n\x1a\n"))
}

func TestImages(t *testing.T) {
	s := transcripttest.RoundTrip(t, codex.Codec, images)
	var prompt, view *transcript.Entry
	for _, e := range s.Linearize() {
		switch {
		case e.Role == transcript.RoleUser && e.Audience == transcript.AudienceAll:
			prompt = &e
		case e.Role == transcript.RoleTool:
			view = &e
		}
	}
	if prompt == nil || view == nil {
		t.Fatal("no prompt or no tool output")
	}
	// Codex frames an attached image in text naming it.
	if got := kinds(prompt.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage, transcript.BlockText, transcript.BlockText}) ||
		!isPNG(prompt.Content[1]) || prompt.Content[0].Text != `<image name=[Image #1] path="pic.png">` {
		t.Errorf("prompt = %v", prompt.Content)
	}
	if got := kinds(view.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) ||
		!isPNG(view.Content[1]) || view.Content[1].ToolID != "call_mock_1" {
		t.Errorf("view_image output = %v", view.Content)
	}
}

func TestImagesImported(t *testing.T) {
	transcripttest.Imported(t, codex.Codec, images)
}

// TestForeignImages: another agent's images are written as input_image
// items with data: URLs, in the user message and in the tool's output;
// what Codex has no item for is left out.
func TestForeignImages(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	s := &transcript.Session{Agent: "test", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
			{Kind: transcript.BlockImage, URI: "https://example.com/a.png"},
			{Kind: transcript.BlockImage, URI: "/tmp/p/local.png"},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF-")},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "view", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c1", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: png},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a picture"}}},
	}}
	home := t.TempDir()
	st, err := codex.Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := back.Messages()
	if len(msgs) != 4 {
		t.Fatalf("%d messages", len(msgs))
	}
	if c := msgs[0].Content; !slices.Equal(kinds(c), []transcript.BlockKind{transcript.BlockText, transcript.BlockImage, transcript.BlockImage}) ||
		!bytes.Equal(c[1].Data, png) || c[1].MediaType != "image/png" || c[2].URI != "https://example.com/a.png" {
		t.Errorf("prompt reads back as %+v", c)
	}
	if c := msgs[2].Content; !slices.Equal(kinds(c), []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) ||
		c[1].ToolID != "c1" || !bytes.Equal(c[1].Data, png) {
		t.Errorf("tool output reads back as %+v", c)
	}
	files, _ := transcript.Glob(home + "/.codex/sessions/*/*/*/*.jsonl")
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if !strings.Contains(string(raw), `"output":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgpmYWtl"}]`) {
		t.Errorf("view output is not written as content items:\n%s", raw)
	}
}
