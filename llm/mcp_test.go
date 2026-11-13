package llm

import (
	"net"
	"strings"
	"testing"
	"time"
)

// A client that never finishes its request headers is disconnected.
func TestServeHTTPDropsSlowHeaders(t *testing.T) {
	old := mcpHeaderTimeout
	mcpHeaderTimeout = 200 * time.Millisecond
	defer func() { mcpHeaderTimeout = old }()

	h, err := serveHTTP(&toolServer{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()

	addr := strings.TrimSuffix(strings.TrimPrefix(h.url, "http://"), "/mcp")
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("POST /mcp HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	// The server answers (408) or just closes; either way the read ends
	// with the connection closed, well before the deadline.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the server kept a connection whose headers never finished")
			}
			return
		}
	}
}
