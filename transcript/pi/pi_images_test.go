package pi

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The image samples are RPC runs of pi and Oh My Pi in the harness-test
// container. pi: a prompt with a PNG, answered by a mock read of a PNG, and
// a second prompt. omp: a prompt with a PNG large enough that omp stores it
// as a blob (converted to WebP), answered by a mock read of a small PNG,
// then a prompt with an @pic.png mention.
var (
	piImages  = transcripttest.Sample{Home: "testdata/home", CWD: "/tmp/harness-test-pi-img/proj", ID: "01a0d2e8-3410-7438-bf36-12bd02d586bc"}
	ompImages = transcripttest.Sample{Home: "testdata/home", CWD: "/tmp/harness-test-omp-img/proj", ID: "01a0d2e8-5d5a-731c-be28-3811486b3f17"}
)

const ompBlob = "f69b1eb497ada7c62426a58db0ff5adbd7408b6d64a73b95de66b24988a5248a"

func kinds(bs []transcript.Block) []transcript.BlockKind {
	var out []transcript.BlockKind
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

func image(b transcript.Block, mediaType, magic string) bool {
	return b.Kind == transcript.BlockImage && b.MediaType == mediaType && b.URI == "" && bytes.HasPrefix(b.Data, []byte(magic))
}

const (
	pngMagic  = "\x89PNG\r\n\x1a\n"
	webpMagic = "RIFF"
)

func TestImagesPi(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, piImages)
	msgs := s.Messages()
	var prompt, read transcript.Entry
	for _, e := range msgs {
		switch {
		case e.Role == transcript.RoleUser && prompt.ID == "":
			prompt = e
		case e.Role == transcript.RoleTool:
			read = e
		}
	}
	if got := kinds(prompt.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) || !image(prompt.Content[1], "image/png", pngMagic) {
		t.Errorf("prompt = %v", prompt.Content)
	}
	if got := kinds(read.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) ||
		!image(read.Content[1], "image/png", pngMagic) || read.Content[1].ToolID != "call_mock_1" || read.Content[0].Text != "Read image file [image/png]" {
		t.Errorf("read = %v", read.Content)
	}
}

func TestImagesOMP(t *testing.T) {
	s := transcripttest.RoundTrip(t, OMP, ompImages)
	byID := map[string]transcript.Entry{}
	for _, e := range s.Messages() {
		byID[e.ID] = e
	}
	// The prompt's image is a blob reference in the row, read from omp's
	// blob store.
	prompt := byID["0873671d"]
	if got := kinds(prompt.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) || !image(prompt.Content[1], "image/webp", webpMagic) {
		t.Fatalf("prompt = %v", prompt.Content)
	}
	blob, err := os.ReadFile(filepath.Join("testdata/home/.omp/agent/blobs", ompBlob))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prompt.Content[1].Data, blob) {
		t.Error("prompt image is not the blob's bytes")
	}
	read := byID["d4b17826"]
	if got := kinds(read.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) ||
		!image(read.Content[1], "image/webp", webpMagic) || read.Content[1].ToolID != "call_mock_1" {
		t.Errorf("read = %v", read.Content)
	}
	// An @mention of an image alone goes to the model as the user's: the
	// <file> element, then the image.
	mention := byID["6f4c1b44"]
	if got := kinds(mention.Content); mention.Role != transcript.RoleUser ||
		!slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) ||
		!strings.HasPrefix(mention.Content[0].Text, `<file path="pic.png">`) || !image(mention.Content[1], "image/webp", webpMagic) {
		t.Errorf("mention = %s %v", mention.Role, mention.Content)
	}
}

// TestImagesOMPMissingBlob: a reference whose blob is gone stays a
// reference, as omp's loader leaves it.
func TestImagesOMPMissingBlob(t *testing.T) {
	home := t.TempDir()
	rel := ".omp/agent/sessions/-proj/2026-09-24T10-13-58-746Z_01a0d2e8-5d5a-731c-be28-3811486b3f17.jsonl"
	raw, err := os.ReadFile(filepath.Join("testdata/home", rel))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, rel), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := OMP.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), ompImages.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Messages() {
		if e.ID == "0873671d" {
			if len(e.Content) != 2 {
				t.Fatalf("prompt = %v", e.Content)
			}
			if b := e.Content[1]; b.Kind != transcript.BlockImage || b.URI != "blob:sha256:"+ompBlob || b.Data != nil {
				t.Errorf("image = %+v", b)
			}
			return
		}
	}
	t.Error("no prompt")
}

func TestImagesImported(t *testing.T) {
	transcripttest.Imported(t, Codec, piImages)
	transcripttest.Imported(t, OMP, ompImages)
}

// TestForeignImages: another agent's images are written as pi-ai
// ImageContent, in the user's message and the tool's result; what it has no
// block for is left out.
func TestForeignImages(t *testing.T) {
	png := []byte(pngMagic + "fake")
	s := &transcript.Session{Agent: "test", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
			{Kind: transcript.BlockImage, URI: "https://example.com/a.png"},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF-")},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "read", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "Read image file", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: png},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a picture"}}},
	}}
	for _, c := range []transcript.Codec{Codec, OMP} {
		st, err := c.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.Write(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		back, err := st.Read(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		msgs := back.Messages()
		if len(msgs) != 4 {
			t.Fatalf("%d messages", len(msgs))
		}
		if b := msgs[0].Content; !slices.Equal(kinds(b), []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) || !bytes.Equal(b[1].Data, png) || b[1].MediaType != "image/png" {
			t.Errorf("prompt reads back as %+v", b)
		}
		if b := msgs[2].Content; !slices.Equal(kinds(b), []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) || b[1].ToolID != "c1" || !bytes.Equal(b[1].Data, png) || b[0].Text != "Read image file" {
			t.Errorf("tool result reads back as %+v", b)
		}
	}
}
