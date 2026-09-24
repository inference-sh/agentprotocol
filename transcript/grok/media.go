package grok

import (
	"encoding/base64"
	"strings"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Grok keeps an image in chat_history.jsonl as a URL, which is a data: URL
// holding the bytes for every image a person attaches or a tool returns
// (ContentPart::Image in conversation.rs), and in updates.jsonl as an ACP
// image block with the bytes in base64. It has no part for other files: a
// PDF read_file returns reaches the model as page images.

// imageFromURL is the image block for an image part's URL: the bytes of a
// base64 data: URL, or the URL itself as the image's location.
func imageFromURL(url, toolID string) transcript.Block {
	b := transcript.Block{Kind: transcript.BlockImage, ToolID: toolID}
	if mediaType, data, ok := parseDataURL(url); ok {
		b.MediaType, b.Data = mediaType, data
		return b
	}
	b.URI = url
	return b
}

// parseDataURL reads a base64 data: URL.
func parseDataURL(url string) (mediaType string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", nil, false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", nil, false
	}
	mediaType, found = strings.CutSuffix(meta, ";base64")
	if !found {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return mediaType, data, true
}

// imageURL is the URL grok stores an image block as: a data: URL of its
// bytes, or its location when that is a web URL. An image by any other
// reference, a local path, is no URL the model can fetch, and has none.
func imageURL(b transcript.Block) (string, bool) {
	switch {
	case b.Kind != transcript.BlockImage:
		return "", false
	case b.Data != nil:
		return "data:" + b.MediaType + ";base64," + base64.StdEncoding.EncodeToString(b.Data), true
	case strings.HasPrefix(b.URI, "https://") || strings.HasPrefix(b.URI, "http://"):
		return b.URI, true
	}
	return "", false
}

// imagePart is an image part of a user item or a tool result's images.
type imagePart struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// imageParts is the image parts for an entry's images: those of the tool
// call toolID, or the entry's own when toolID is empty.
func imageParts(e transcript.Entry, toolID string) []any {
	var out []any
	for _, b := range e.Content {
		if b.ToolID != toolID {
			continue
		}
		if url, ok := imageURL(b); ok {
			out = append(out, imagePart{Type: "image", URL: url})
		}
	}
	return out
}

// acpImage is an ACP image content block.
type acpImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// acpImages is the ACP image blocks for an entry's images, those of the
// tool call toolID or the entry's own. An ACP image carries its bytes; one
// known only by URL has none.
func acpImages(e transcript.Entry, toolID string) []acpImage {
	var out []acpImage
	for _, b := range e.Content {
		if b.Kind == transcript.BlockImage && b.ToolID == toolID && b.Data != nil {
			out = append(out, acpImage{Type: "image", Data: base64.StdEncoding.EncodeToString(b.Data), MimeType: b.MediaType})
		}
	}
	return out
}
