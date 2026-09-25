package gemini

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
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
			{Kind: transcript.BlockFile, MediaType: "application/pdf", URI: "gs://b/a.pdf"},
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

// TestImportedFileHasNoDisplayName moves claude's image run, whose second
// prompt attaches doc.pdf, into gemini. The file is an inlineData part
// without the file's name: @google/genai throws "displayName parameter is
// not supported in Gemini API" on a part carrying displayName outside
// Vertex AI, which failed the turn after the import.
func TestImportedFileHasNoDisplayName(t *testing.T) {
	src, err := claude.Codec.Open("../claude/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Read(t.Context(), "07f0f8b5-1255-4c04-b97b-e1f209e315ac")
	if err != nil {
		t.Fatal(err)
	}
	s.ID = ""
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".gemini", "tmp", "*", "chats", "*.jsonl"))
	if len(paths) != 1 {
		t.Fatalf("session files %v", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var media []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if m, ok := e.(map[string]any); ok && (k == "inlineData" || k == "fileData") {
					media = append(media, m)
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
		walk(v)
	}
	pdf := false
	for _, m := range media {
		if _, ok := m["displayName"]; ok {
			t.Errorf("media part with displayName: %v", m["mimeType"])
		}
		pdf = pdf || m["mimeType"] == "application/pdf"
	}
	if !pdf {
		t.Error("no PDF part written")
	}
}
