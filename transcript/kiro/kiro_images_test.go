package kiro

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// An ACP run of kiro-cli 2.24 in the harness-test container: a prompt with
// a PNG, then a prompt whose answer calls read on a PNG in Image mode; then
// loaded and resumed with one more prompt. kiro gave the model both images
// on resume.
const (
	imagesCWD = "/tmp/harness-test-kiro-image/test-repo"
	imagesID  = "d2db899b-7e34-4f60-be19-750e06623c82"
	redPNG    = "iVBORw0KGgoAAAANSUhEUgAAAAQAAAAECAIAAAAmkwkpAAAAEElEQVR4nGP4z8AARwzEcQCukw/x0F8jngAAAABJRU5ErkJggg=="
	bluePNG   = "iVBORw0KGgoAAAANSUhEUgAAAAMAAAADCAIAAADZSiLoAAAAD0lEQVR4nGNgYPgPQ5gsAH2gCPgfW8MjAAAAAElFTkSuQmCC"
)

func png(t *testing.T, b64 string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImages(t *testing.T) {
	s := read(t, imagesID)
	msgs := s.Messages()
	if b := msgs[0].Content; len(b) != 2 || b[1].Kind != transcript.BlockImage || b[1].MediaType != "image/png" || !bytes.Equal(b[1].Data, png(t, redPNG)) {
		t.Errorf("prompt: %+v", b)
	}
	var tool *transcript.Entry
	for i := range msgs {
		if msgs[i].Role == transcript.RoleTool {
			tool = &msgs[i]
		}
	}
	if tool == nil {
		t.Fatal("no tool result")
	}
	if b := tool.Content; len(b) != 2 || b[0].Kind != transcript.BlockToolResult || b[1].Kind != transcript.BlockImage ||
		b[1].ToolID != "mock-tool-1" || !bytes.Equal(b[1].Data, png(t, bluePNG)) {
		t.Errorf("tool result: %+v", b)
	}
	smp := transcripttest.Sample{Home: "testdata/home", CWD: imagesCWD, ID: imagesID}
	transcripttest.RoundTrip(t, Codec, smp)
	transcripttest.Imported(t, Codec, smp)
}

// TestWriteImages writes a prompt's image and a tool's image in the forms
// kiro wrote them in the capture, and each tool's outcome in the ToolResults
// row's results map in the shape kiro loads. Checked in the container: kiro
// resumed the written session and gave the model both images; with the map
// written as {"tool": name, "status": ...} it crashed on the next prompt
// ("invalid conversation history received").
func TestWriteImages(t *testing.T) {
	red := png(t, redPNG)
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "look"}, {Kind: transcript.BlockImage, MediaType: "image/png", Data: red}, {Kind: transcript.BlockFile, URI: "/tmp/p/a.pdf"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "read", Input: []byte(`{}`)}, {Kind: transcript.BlockToolUse, ToolID: "c2", Name: "read", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c1", Text: "viewed"}, {Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: red},
			{Kind: transcript.BlockToolResult, ToolID: "c2", Text: "no such file", Status: transcript.StatusError},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if b := msgs[0].Content; len(b) != 2 || !bytes.Equal(b[1].Data, red) {
		t.Errorf("prompt: %+v", b)
	}
	if b := msgs[2].Content; len(b) != 3 || b[1].Kind != transcript.BlockImage || b[1].ToolID != "c1" || !bytes.Equal(b[1].Data, red) {
		t.Errorf("tool results: %+v", b)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kiro/sessions/cli", id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row struct {
		Data struct {
			Results map[string]json.RawMessage `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &row); err != nil {
		t.Fatal(err)
	}
	if got := string(row.Data.Results["c1"]); !strings.HasPrefix(got, `{"tool":null,"result":{"Success":{"items":[{"Text":"viewed"},{"Image":{"format":"png","source":{"kind":"bytes","data":[137,80,`) {
		t.Errorf("c1 outcome: %s", got)
	}
	if got := string(row.Data.Results["c2"]); got != `{"tool":null,"result":{"Error":{"Custom":"no such file"}}}` {
		t.Errorf("c2 outcome: %s", got)
	}
}
