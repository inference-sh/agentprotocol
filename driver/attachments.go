package driver

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"

	ap "github.com/inference-sh/agentprotocol"
)

// maxImageBytes is the largest image a driver inlines. It is the Anthropic
// API's limit per image, the lowest of the providers behind these agents; a
// larger image is named in the text instead, where the agent can open it.
const maxImageBytes = 5 << 20

// imageTypes are the image types every provider behind these agents takes.
var imageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// inlineImage is an attached image's bytes, base64-encoded.
type inlineImage struct {
	MimeType string
	Data     string
}

// imageOf reads an attached image a driver can inline: one of imageTypes,
// with its bytes on this host (a file:// URI or a path) or in a data: URI,
// within maxImageBytes. Anything else is not an image to inline, and the
// driver names it in the text.
func imageOf(f ap.FileRef) (inlineImage, bool) {
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(f.ContentType, ";", 2)[0]))
	if strings.HasPrefix(f.URI, "data:") {
		head, data, ok := strings.Cut(strings.TrimPrefix(f.URI, "data:"), ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return inlineImage{}, false
		}
		if m := strings.TrimSuffix(head, ";base64"); m != "" {
			mime = strings.ToLower(m)
		}
		if !imageTypes[mime] || base64.StdEncoding.DecodedLen(len(data)) > maxImageBytes {
			return inlineImage{}, false
		}
		return inlineImage{MimeType: mime, Data: data}, true
	}
	if !imageTypes[mime] {
		return inlineImage{}, false
	}
	path := localPath(f.URI)
	if path == "" {
		return inlineImage{}, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxImageBytes {
		return inlineImage{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > maxImageBytes {
		return inlineImage{}, false
	}
	return inlineImage{MimeType: mime, Data: base64.StdEncoding.EncodeToString(b)}, true
}

// localPath is the path a file:// URI or an absolute path names on this
// host, or "" for anything else.
func localPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		u, err := url.Parse(uri)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return ""
		}
		return u.Path
	}
	if strings.HasPrefix(uri, "/") {
		return uri
	}
	return ""
}

// withFiles appends the files a driver did not send in the agent's own form
// to the text, each by its local path where it has one, so the agent can
// open it with its own tools.
func withFiles(text string, files []ap.FileRef) string {
	if len(files) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\nAttached files:")
	for _, f := range files {
		where := f.URI
		if p := localPath(f.URI); p != "" {
			where = p
		}
		name := f.Filename
		if name == "" || name == where {
			fmt.Fprintf(&b, "\n- %s", where)
			continue
		}
		fmt.Fprintf(&b, "\n- %s (%s)", name, where)
	}
	return b.String()
}
