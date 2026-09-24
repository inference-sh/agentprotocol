package gemini

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The image sample is gemini-cli over ACP in the harness-test container: a
// prompt with a PNG attached, then a prompt answered with two mock read_file
// calls on a PNG in the repository. The model, gemini-3.8-flash, takes no
// media in a function response, so each call's image follows its response
// as a part of its own.
const (
	imageHome = "testdata/image"
	imageCWD  = "/tmp/harness-test-gemini-2059580919/test-repo"
	imageID   = "0c7c796e-c420-45f5-9738-848c3af96910"
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

// The attached image is an inlineData part of the prompt: an image block,
// with the hook's context the model's alone.
func TestImagePrompt(t *testing.T) {
	prompt := readImage(t).Linearize()[0]
	if len(prompt.Content) != 2 || prompt.Content[0].Text != "What is in this image?" {
		t.Fatalf("prompt = %+v, want the text and the image", prompt.Content)
	}
	img := prompt.Content[1]
	if img.Kind != transcript.BlockImage || img.MediaType != "image/png" || !bytes.HasPrefix(img.Data, pngMagic) {
		t.Errorf("image = %s %s %.8q, want a PNG", img.Kind, img.MediaType, img.Data)
	}
	if len(prompt.ModelContent) != 3 {
		t.Errorf("model content = %+v, want the hook context too", prompt.ModelContent)
	}
}

// Each call's image follows its result, with the call's id, and the record
// is still the tool's.
func TestToolImages(t *testing.T) {
	for _, e := range readImage(t).Messages() {
		if e.Role != transcript.RoleTool {
			continue
		}
		if len(e.Content) != 4 {
			t.Fatalf("tool entry = %+v, want two results, each with its image", e.Content)
		}
		for i := 0; i < 4; i += 2 {
			res, img := e.Content[i], e.Content[i+1]
			if res.Kind != transcript.BlockToolResult || !strings.HasPrefix(res.ToolID, "read_file__read_file_") {
				t.Errorf("block %d = %+v, want a read_file result", i, res)
			}
			if img.Kind != transcript.BlockImage || img.ToolID != res.ToolID || !bytes.HasPrefix(img.Data, pngMagic) {
				t.Errorf("block %d = %s %q, want the PNG call %q returned", i+1, img.Kind, img.ToolID, res.ToolID)
			}
		}
		return
	}
	t.Fatal("no tool entry")
}

// The nested sample is the same run on gemini-3-flash-preview, a model
// that takes media in a function response: each call's image is nested in
// its response, and reads the same way.
const (
	nestedHome = "testdata/nested"
	nestedCWD  = "/tmp/harness-test-gemini-2847728782/test-repo"
	nestedID   = "6b3defca-d994-4acb-bc96-23bfcdeca161"
)

func TestNestedToolImages(t *testing.T) {
	smp := transcripttest.Sample{Home: nestedHome, CWD: nestedCWD, ID: nestedID}
	transcripttest.RoundTrip(t, Codec, smp)
	st, err := Codec.Open(nestedHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), nestedID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Messages() {
		if e.Role != transcript.RoleTool || len(e.Content) == 0 || e.Content[0].Name != "read_file" {
			continue
		}
		if len(e.Content) != 4 {
			t.Fatalf("tool entry = %+v, want two results, each with its image", e.Content)
		}
		for i := 0; i < 4; i += 2 {
			res, img := e.Content[i], e.Content[i+1]
			if img.Kind != transcript.BlockImage || img.ToolID != res.ToolID || !bytes.HasPrefix(img.Data, pngMagic) {
				t.Errorf("block %d = %s %q, want the PNG call %q returned", i+1, img.Kind, img.ToolID, res.ToolID)
			}
		}
		return
	}
	t.Fatal("no read_file entry")
}

// Written for gemini from another agent, images and files are inlineData
// or fileData parts, a tool's right after its response.
func TestWriteMedia(t *testing.T) {
	in := &transcript.Session{ID: transcript.NewUUID(), Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{ID: transcript.NewUUID(), Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", URI: "gs://b/a.pdf", Name: "a.pdf"},
		}},
		{ID: transcript.NewUUID(), Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c", Name: "read_file", Input: []byte(`{}`)}}},
		{ID: transcript.NewUUID(), Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c", Name: "read_file", Text: "read", Status: transcript.StatusOK},
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
	if msgs[2].Role != transcript.RoleTool {
		t.Errorf("tool entry read back as %s", msgs[2].Role)
	}
}

func sameBlock(x, y transcript.Block) bool {
	return x.Kind == y.Kind && x.Text == y.Text && x.ToolID == y.ToolID && x.MediaType == y.MediaType && x.Name == y.Name && x.URI == y.URI && bytes.Equal(x.Data, y.Data)
}
