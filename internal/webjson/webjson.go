// Package webjson makes the small JSON requests the usage probers send to
// vendors' account APIs.
package webjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Do sends req and decodes a 200's JSON body into out. Any other status is a
// StatusError.
func Do(c *http.Client, req *http.Request, out any) error {
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(raw[:min(len(raw), 200)]))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Get sends a GET with the headers given.
func Get(ctx context.Context, c *http.Client, url string, header map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return Do(c, req, out)
}

// Post sends body as JSON with the headers given.
func Post(ctx context.Context, c *http.Client, url string, header map[string]string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return Do(c, req, out)
}

// StatusError is a response other than 200. Its message is the status alone:
// probers put errors in UsageLimits.Unavailable, which is stored and shown to
// whoever can see the agent, and a vendor's error body can name the account.
// Body keeps the start of it for a caller that logs locally.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }
