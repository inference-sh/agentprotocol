package qwen

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The image samples are qwen-code 0.24.4 in the harness-test container:
// toolImageID an ACP run whose second prompt got a mock read_file call on a
// PNG, which the tool returns as an image; promptImageID a headless run of
// "What is in @pic.png ?", which reads the image into the prompt; linkID an
// ACP run whose prompt carried a resource_link to it.
const (
	imageHome     = "testdata/image"
	toolImageCWD  = "/tmp/harness-test-qwen-1251420616/test-repo"
	toolImageID   = "e2eb4abd-df2d-4afb-a1f8-bd8743683234"
	promptCWD     = "/tmp/harness-test-qwen-3348733556/test-repo"
	promptImageID = "6609e585-2f50-42e8-92d1-aba4ab9c1a27"
	linkID        = "5a128734-8f8c-4314-ae4a-187a126a2e01"
)

var jpegMagic = []byte("\xff\xd8\xff")

func readImage(t *testing.T, id string) *transcript.Session {
	t.Helper()
	st, err := Codec.Open(imageHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImageSamplesRoundTrip(t *testing.T) {
	for _, smp := range []transcripttest.Sample{
		{Home: imageHome, CWD: toolImageCWD, ID: toolImageID},
		{Home: imageHome, CWD: promptCWD, ID: promptImageID},
		{Home: imageHome, CWD: promptCWD, ID: linkID},
	} {
		transcripttest.RoundTrip(t, Codec, smp)
		transcripttest.Imported(t, Codec, smp)
	}
}

// read_file returns an image as an inlineData part nested in its
// functionResponse; it follows the result, with the call's id.
func TestToolImage(t *testing.T) {
	for _, e := range readImage(t, toolImageID).Messages() {
		if e.Role != transcript.RoleTool {
			continue
		}
		if len(e.Content) != 2 {
			t.Fatalf("tool entry = %+v, want the result and its image", e.Content)
		}
		res, img := e.Content[0], e.Content[1]
		if res.Kind != transcript.BlockToolResult || !strings.HasPrefix(res.Text, "Image overview") {
			t.Errorf("result = %+v", res)
		}
		if img.Kind != transcript.BlockImage || img.ToolID != "call_mock_1" || img.MediaType != "image/jpeg" || !bytes.HasPrefix(img.Data, jpegMagic) {
			t.Errorf("image = %s %q %s %.4q, want a JPEG for call_mock_1", img.Kind, img.ToolID, img.MediaType, img.Data)
		}
		return
	}
	t.Fatal("no tool entry")
}

// An image read into a prompt is an image block of the prompt, for the
// person and the model alike.
func TestPromptImage(t *testing.T) {
	s := readImage(t, promptImageID)
	prompt := s.Linearize()[0]
	var img *transcript.Block
	for i, b := range prompt.Content {
		if b.Kind == transcript.BlockImage {
			img = &prompt.Content[i]
		}
		if b.Kind == transcript.BlockText && b.Text == "" {
			t.Error("prompt has an empty text block")
		}
	}
	if img == nil || img.MediaType != "image/jpeg" || !bytes.HasPrefix(img.Data, jpegMagic) {
		t.Fatalf("prompt = %+v, want a JPEG image block", prompt.Content)
	}
	if ctx := s.Context(); len(ctx) == 0 || len(ctx[0].Content) != len(prompt.Content) {
		t.Errorf("context's prompt differs from the person's")
	}
}

// A resource link an ACP prompt carried is shown with the prompt; the model
// is given the prompt's text only, as Qwen resumes it.
func TestPromptResourceLink(t *testing.T) {
	s := readImage(t, linkID)
	var shown, sent *transcript.Entry
	for _, e := range s.Linearize() {
		if e.Role == transcript.RoleUser && e.Text() == "Describe this file." {
			shown = &e
		}
	}
	for _, e := range s.Context() {
		if e.Role == transcript.RoleUser && e.Text() == "Describe this file." {
			sent = &e
		}
	}
	if shown == nil || sent == nil {
		t.Fatal("prompt not found")
	}
	want := transcript.Block{Kind: transcript.BlockImage, MediaType: "image/png", URI: "file://" + promptCWD + "/pic.png", Name: "pic.png"}
	if n := len(shown.Content); n != 2 || !sameBlock(shown.Content[1], want) {
		t.Errorf("shown = %+v, want the text then %+v", shown.Content, want)
	}
	if len(sent.Content) != 1 {
		t.Errorf("sent = %+v, want the text alone", sent.Content)
	}
}

// Written for Qwen from another agent, images and files are inlineData or
// fileData parts, and a tool's are nested in its functionResponse.
func TestWriteMedia(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	in := &transcript.Session{ID: transcript.NewUUID(), Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{ID: transcript.NewUUID(), Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", URI: "/tmp/a.pdf", Name: "a.pdf"},
		}},
		{ID: transcript.NewUUID(), Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c", Name: "read_file", Input: []byte(`{}`)}}},
		{ID: transcript.NewUUID(), Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c", Name: "read_file", Text: "read", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c", MediaType: "image/png", Data: png},
		}},
	}}
	for i := 1; i < len(in.Entries); i++ {
		in.Entries[i].ParentID = in.Entries[i-1].ID
	}
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
	for i, want := range [][]transcript.Block{in.Entries[0].Content, in.Entries[2].Content} {
		got := msgs[2*i].Content
		if len(got) != len(want) {
			t.Fatalf("entry %d = %+v, want %+v", 2*i, got, want)
		}
		for j := range want {
			if !sameBlock(got[j], want[j]) {
				t.Errorf("entry %d block %d = %+v, want %+v", 2*i, j, got[j], want[j])
			}
		}
	}
	if !strings.Contains(string(msgs[2].Raw), `"functionResponse":{"id":"c","name":"read_file","response":{"output":"read"},"parts":[{"inlineData"`) {
		t.Errorf("tool row = %s, want the image nested in the response", msgs[2].Raw)
	}
}

func sameBlock(x, y transcript.Block) bool {
	return x.Kind == y.Kind && x.Text == y.Text && x.ToolID == y.ToolID && x.MediaType == y.MediaType && x.Name == y.Name && x.URI == y.URI && bytes.Equal(x.Data, y.Data)
}
