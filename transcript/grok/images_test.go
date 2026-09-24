package grok

import (
	"bytes"
	"slices"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func TestImageSampleRoundTrip(t *testing.T) {
	smp := transcripttest.Sample{Home: "testdata/home", CWD: "/tmp/harness-test-grok-3546040497", ID: imageID}
	transcripttest.RoundTrip(t, Codec, smp)
	transcripttest.Imported(t, Codec, smp)
}

// The attached image is an image block of the prompt, and the image
// read_file returned follows its result, in both views: chat_history.jsonl
// keeps them as data: URLs, updates.jsonl as ACP image blocks.
func TestImages(t *testing.T) {
	s := read(t, "testdata/home", imageID)
	want := []string{
		"user: What is in this image? [image image/png]",
		"assistant: Hello from mock server.",
		"user: Read pic.png please.",
		"assistant: call read_file",
		"tool: result Read image file: /tmp/harness-test-grok-3546040497/pic.png [image image/png]",
		"assistant: Hello from mock server.",
	}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize\n  %q\nwant\n  %q", got, want)
	}
	var sent []string
	for _, e := range s.Context() {
		if e.Audience == transcript.AudienceAll {
			sent = append(sent, texts([]transcript.Entry{e})...)
		}
	}
	if !slices.Equal(sent, want) {
		t.Errorf("context\n  %q\nwant\n  %q", sent, want)
	}
	for _, view := range [][]transcript.Entry{s.Linearize(), s.Context()} {
		for _, e := range view {
			for _, b := range e.Content {
				if b.Kind != transcript.BlockImage {
					continue
				}
				if !bytes.HasPrefix(b.Data, pngMagic) {
					t.Errorf("%s image data %.8q, want a PNG", e.Role, b.Data)
				}
				if e.Role == transcript.RoleTool && b.ToolID != "call_mock_1" {
					t.Errorf("tool image for %q, want call_mock_1", b.ToolID)
				}
			}
		}
	}
}

// Written for grok from another agent, an image is a data: URL image part
// in chat_history.jsonl and an ACP image in updates.jsonl, a tool's among
// the result's images; an image grok could not fetch, and any other file,
// is left out.
func TestWriteMedia(t *testing.T) {
	in := &transcript.Session{Agent: "elsewhere", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "look"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: pngMagic},
			{Kind: transcript.BlockImage, MediaType: "image/png", URI: "/tmp/a.png"},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF")},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c", Name: "read_file", Input: []byte(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c", Text: "read", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c", MediaType: "image/png", Data: pngMagic},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a picture"}}},
	}}
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, home, id)
	want := []string{
		"user: look [image image/png]",
		"assistant: call read_file",
		"tool: result read [image image/png]",
		"assistant: a picture",
	}
	if got := texts(s.Linearize()); !slices.Equal(got, want) {
		t.Errorf("linearize\n  %q\nwant\n  %q", got, want)
	}
	if got := texts(s.Context()); !slices.Equal(got, want) {
		t.Errorf("context\n  %q\nwant\n  %q", got, want)
	}
	for _, e := range s.Context() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockImage && !bytes.Equal(b.Data, pngMagic) {
				t.Errorf("image data %q", b.Data)
			}
		}
	}
}
