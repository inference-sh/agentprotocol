package sqlite

import (
	"bytes"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// TestCursorMedia reads image and file parts of a stored message. No
// automated run yields one: Cursor's backend writes the message blobs, and
// the mock's stand-in writes text and tool parts only. The part types and
// filename are those Cursor's transcript writer reads (TranscriptStore,
// "[Image]" and "[File: name]"); the fields around them are the AI SDK's.
func TestCursorMedia(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nxx")
	e, err := cursorEntry([]byte(`{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","image":"iVBORw0KGgp4eA==","mimeType":"image/png"},` +
		`{"type":"image","image":"data:image/png;base64,iVBORw0KGgp4eA=="},` +
		`{"type":"image","image":"https://example.com/a.png"},` +
		`{"type":"file","data":"aGk=","mediaType":"text/plain","filename":"notes.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := e.Content
	if len(c) != 5 {
		t.Fatalf("content: %+v", c)
	}
	for _, b := range c[1:3] {
		if b.Kind != transcript.BlockImage || b.MediaType != "image/png" || !bytes.Equal(b.Data, png) {
			t.Errorf("image: %+v", b)
		}
	}
	if b := c[3]; b.Kind != transcript.BlockImage || b.URI != "https://example.com/a.png" {
		t.Errorf("image by URL: %+v", b)
	}
	if b := c[4]; b.Kind != transcript.BlockFile || b.Name != "notes.txt" || b.MediaType != "text/plain" || string(b.Data) != "hi" {
		t.Errorf("file: %+v", b)
	}
}
