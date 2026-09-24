package claude

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

// The images sample is two headless turns of claude 2.1.281 in the
// harness-test container, fed over --input-format stream-json: a prompt
// with a PNG, answered by a mock Read of a PNG; then a prompt with a PDF and
// a plain text document, answered by a mock Read of the PDF.
const (
	imagesCWD = "/tmp/harness-test-claude-1885166021/test-repo"
	imagesID  = "07f0f8b5-1255-4c04-b97b-e1f209e315ac"
)

var images = transcripttest.Sample{Home: "testdata/home", CWD: imagesCWD, ID: imagesID}

func kinds(bs []transcript.Block) []transcript.BlockKind {
	var out []transcript.BlockKind
	for _, b := range bs {
		out = append(out, b.Kind)
	}
	return out
}

func TestImages(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, images)
	var msgs []transcript.Entry
	for _, e := range s.Linearize() {
		if e.Audience == transcript.AudienceAll {
			msgs = append(msgs, e)
		}
	}
	find := func(role transcript.Role, n int) transcript.Entry {
		t.Helper()
		for _, e := range msgs {
			if e.Role == role {
				if n == 0 {
					return e
				}
				n--
			}
		}
		t.Fatalf("no %s entry %d", role, n)
		return transcript.Entry{}
	}
	isPNG := func(b transcript.Block) bool {
		return b.Kind == transcript.BlockImage && b.MediaType == "image/png" && bytes.HasPrefix(b.Data, []byte("\x89PNG\r\n\x1a\n"))
	}

	prompt := find(transcript.RoleUser, 0)
	if got := kinds(prompt.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockImage}) || !isPNG(prompt.Content[1]) {
		t.Errorf("image prompt = %v", prompt.Content)
	}
	read := find(transcript.RoleTool, 0)
	if got := kinds(read.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockImage}) ||
		!isPNG(read.Content[1]) || read.Content[1].ToolID != read.Content[0].ToolID {
		t.Errorf("image read = %v", read.Content)
	}

	docs := find(transcript.RoleUser, 1)
	if got := kinds(docs.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockText, transcript.BlockFile, transcript.BlockFile}) {
		t.Fatalf("document prompt = %v", got)
	}
	if b := docs.Content[1]; b.MediaType != "application/pdf" || b.Name != "notes.pdf" || !bytes.HasPrefix(b.Data, []byte("%PDF-")) {
		t.Errorf("pdf = %s %q %.8q", b.MediaType, b.Name, b.Data)
	}
	if b := docs.Content[2]; b.MediaType != "text/plain" || b.Name != "notes.txt" || string(b.Data) != "plain text attachment" {
		t.Errorf("text document = %s %q %q", b.MediaType, b.Name, b.Data)
	}
	pdf := find(transcript.RoleTool, 1)
	if got := kinds(pdf.Content); !slices.Equal(got, []transcript.BlockKind{transcript.BlockToolResult, transcript.BlockFile}) ||
		pdf.Content[1].ToolID != pdf.Content[0].ToolID || !bytes.HasPrefix(pdf.Content[1].Data, []byte("%PDF-")) {
		t.Errorf("pdf read = %v", pdf.Content)
	}
}

func TestImagesImported(t *testing.T) {
	transcripttest.Imported(t, Codec, images)
}

// TestForeignImages: images and files from another agent are written as
// Claude writes them, inside the tool_result when a tool returned them, and
// what the API would refuse is left out.
func TestForeignImages(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	pdf := []byte("%PDF-1.1 fake")
	s := &transcript.Session{Agent: "test", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
			{Kind: transcript.BlockImage, URI: "https://example.com/a.png"},
			{Kind: transcript.BlockImage, URI: "/tmp/p/local.png", MediaType: "image/png"},
			{Kind: transcript.BlockImage, MediaType: "image/svg+xml", Data: []byte("<svg/>")},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: pdf, Name: "a.pdf"},
			{Kind: transcript.BlockFile, MediaType: "text/markdown", Data: []byte("# notes"), Name: "notes.md"},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "view", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "viewed", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: png},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a picture"}}},
	}}
	home := t.TempDir()
	st, err := Codec.Open(home)
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
	want := []transcript.Block{
		{Kind: transcript.BlockText, Text: "look"},
		{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
		{Kind: transcript.BlockImage, URI: "https://example.com/a.png"},
		{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: pdf, Name: "a.pdf"},
		{Kind: transcript.BlockFile, MediaType: "text/plain", Data: []byte("# notes"), Name: "notes.md"},
	}
	if !blocksEqual(msgs[0].Content, want) {
		t.Errorf("prompt reads back as %+v", msgs[0].Content)
	}
	tool := msgs[2]
	if tool.Role != transcript.RoleTool || !blocksEqual(tool.Content, []transcript.Block{
		{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "viewed", Status: transcript.StatusOK},
		{Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: png},
	}) {
		t.Errorf("tool result reads back as %s %+v", tool.Role, tool.Content)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "projects", transcript.MangledCwd.Name("/tmp/p"), id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":[{"type":"text","text":"viewed"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgpmYWtl"}}]`) {
		t.Errorf("tool_result is not written with its image inside:\n%s", raw)
	}
}

func blocksEqual(a, b []transcript.Block) bool {
	return slices.EqualFunc(a, b, func(x, y transcript.Block) bool {
		return x.Kind == y.Kind && x.Text == y.Text && x.ToolID == y.ToolID && x.Status == y.Status &&
			x.MediaType == y.MediaType && bytes.Equal(x.Data, y.Data) && x.URI == y.URI && x.Name == y.Name
	})
}
