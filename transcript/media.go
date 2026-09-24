package transcript

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// ParseDataURL decodes a data: URL (RFC 2397) into its media type and
// bytes, the form most agents store an inline image in. The payload is
// base64 when the URL says so and percent-encoded otherwise.
func ParseDataURL(u string) (mediaType string, data []byte, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, false
	}
	mediaType, params, _ := strings.Cut(meta, ";")
	if strings.HasSuffix(params, "base64") {
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", nil, false
		}
		return mediaType, data, true
	}
	text, err := url.PathUnescape(payload)
	if err != nil {
		return "", nil, false
	}
	return mediaType, []byte(text), true
}

// DataURL encodes bytes as a base64 data: URL.
func DataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
