package copilot

import (
	"bytes"
	"encoding/base64"
	"slices"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// An ACP run of Copilot CLI 1.0.88 in the harness-test container: a prompt
// with a PNG, a prompt with an embedded text resource and a resource link,
// and a prompt whose answer calls view on a PNG in the repository; then
// loaded and resumed with one more prompt.
const (
	imagesCWD = "/tmp/harness-test-copilot-image/test-repo"
	imagesID  = "d96d5f9f-c78a-4ae3-8c8f-daef56c1200c"
	// The two PNGs the run sent, as base64.
	redPNG  = "iVBORw0KGgoAAAANSUhEUgAAAAQAAAAECAIAAAAmkwkpAAAAEElEQVR4nGP4z8AARwzEcQCukw/x0F8jngAAAABJRU5ErkJggg=="
	bluePNG = "iVBORw0KGgoAAAANSUhEUgAAAAMAAAADCAIAAADZSiLoAAAAD0lEQVR4nGNgYPgPQ5gsAH2gCPgfW8MjAAAAAElFTkSuQmCC"
)

func png(t *testing.T, b64 string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kinds(bs []transcript.Block) []transcript.BlockKind {
	var out []transcript.BlockKind
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

// TestImages reads the prompt's image from the session.binary_asset its
// attachment names, the embedded resource as the file Copilot saved it to
// (typed by its extension, as Copilot records no type for it),
// and the image view returned after the tool's result; and gives the model
// each prompt as Copilot sent it on resume.
func TestImages(t *testing.T) {
	s := read(t, "testdata/home", imagesID)
	var users, tools []transcript.Entry
	for _, e := range s.Messages() {
		switch e.Role {
		case transcript.RoleUser:
			users = append(users, e)
		case transcript.RoleTool:
			tools = append(tools, e)
		}
	}
	if len(users) != 4 || len(tools) != 1 {
		t.Fatalf("%d prompts, %d tool results", len(users), len(tools))
	}

	img := users[0]
	if got := kinds(img.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) {
		t.Fatalf("image prompt: %v", got)
	}
	if b := img.Content[1]; b.MediaType != "image/png" || !bytes.Equal(b.Data, png(t, redPNG)) {
		t.Errorf("image: %s %x", b.MediaType, b.Data)
	}
	if img.Text() != "what is in this image" {
		t.Errorf("shown: %q", img.Text())
	}
	mc := img.ModelContent
	if len(mc) != 3 || mc[0].Text != "<current_datetime>2026-09-24T10:09:42.898+00:00</current_datetime>\n\nwhat is in this image" ||
		mc[1].Text != "Image file at path /tmp/copilot-image-a75f7d.png" || mc[2].Kind != transcript.BlockImage {
		t.Errorf("model content: %+v", mc)
	}

	res := users[1]
	if len(res.Content) != 2 || res.Content[1].Kind != transcript.BlockFile ||
		res.Content[1].URI != "/tmp/acp-resource-4b85201a-9c36-4b8b-83bf-1d82d0e73d9e.txt" || res.Content[1].Name != "notes.txt" ||
		res.Content[1].MediaType != "text/plain" {
		t.Errorf("resource prompt: %+v", res.Content)
	}
	const sent = "<current_datetime>2026-09-24T10:09:44.060+00:00</current_datetime>\n\nread the attached notes\n[Resource link: file:///tmp/other.md]\n\n\n\n<tagged_files>\n* /tmp/acp-resource-4b85201a-9c36-4b8b-83bf-1d82d0e73d9e.txt (1 lines)\n</tagged_files>"
	if len(res.ModelContent) != 1 || res.ModelContent[0].Text != sent {
		t.Errorf("resource prompt, model content: %+v", res.ModelContent)
	}

	tool := tools[0]
	if got := kinds(tool.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) {
		t.Fatalf("tool result: %v", got)
	}
	if b := tool.Content[1]; b.ToolID != "call_mock_1" || !bytes.Equal(b.Data, png(t, bluePNG)) {
		t.Errorf("tool image: %+v", b)
	}

	transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: imagesCWD, ID: imagesID})
	transcripttest.Imported(t, Writer, transcripttest.Sample{Home: "testdata/home", CWD: imagesCWD, ID: imagesID})
}

// TestWriteImages writes a prompt image as a blob attachment and a tool's
// image as an inline binary result, the forms Copilot loads from another
// agent's session (checked in the container: on resume the model was given
// the prompt's image).
func TestWriteImages(t *testing.T) {
	red := png(t, redPNG)
	home := t.TempDir()
	st, err := Writer.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "look"}, {Kind: transcript.BlockImage, MediaType: "image/png", Data: red}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "view", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "viewed"}, {Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: red}}},
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "and this"}, {Kind: transcript.BlockFile, URI: "/tmp/p/notes.txt"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if b := msgs[0].Content; len(b) != 2 || b[1].Kind != transcript.BlockImage || !bytes.Equal(b[1].Data, red) {
		t.Errorf("prompt: %+v", b)
	}
	if b := msgs[2].Content; len(b) != 2 || b[1].ToolID != "c1" || !bytes.Equal(b[1].Data, red) {
		t.Errorf("tool result: %+v", b)
	}
	if b := msgs[3].Content; len(b) != 2 || b[1].Kind != transcript.BlockFile || b[1].URI != "/tmp/p/notes.txt" || b[1].Name != "notes.txt" {
		t.Errorf("file prompt: %+v", b)
	}
}
