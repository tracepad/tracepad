// Package client is an HTTP client of the tracepad read API. The CLI and the
// MCP server are both built on it, and neither ever opens the database: the
// server holds the SQLite writer, and a second process reading the live file
// is exactly the class of corruption-adjacent cleverness this project refuses
// (spec 004 #11).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the server a client talks to when nothing says otherwise: the
// port an unconfigured OTel exporter also uses, so one running tracepad serves
// both without configuration.
const DefaultURL = "http://localhost:4318"

// VersionHeader carries the server's build on every API response, so a client
// learns about version skew without spending a round trip to ask (spec 004
// Decision 28).
const VersionHeader = "X-Tracepad-Version"

// Client talks to one tracepad.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
	// OnVersion, when set, is called with the server version carried by
	// each response.
	OnVersion func(string)
}

// New builds a client for a base URL and a project key.
func New(baseURL, key string) (*Client, error) {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("server url %q is not an http(s) url", baseURL)
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Key:     key,
		// A generous timeout rather than none: `?expand=io` over a big
		// trace is a real request, and a hung one should still end.
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Error is a response the server refused. Status is what a caller maps onto
// its own exit code, and Message is what the server said.
type Error struct {
	Status  int
	Message string
	// Path is the request that failed, so a message like "not found"
	// still says what was not found.
	Path string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", e.Path, http.StatusText(e.Status))
	}
	return e.Message
}

// Get calls a read endpoint and returns its body verbatim. Verbatim is the
// point: the CLI, an MCP tool and `curl` must be able to answer with the same
// bytes (spec 004 #1, #16).
func (c *Client) Get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return c.do(request, path)
}

// Fetch calls a read endpoint that does not answer with JSON and returns the
// body with the response headers. The archive's body endpoint is the first
// such: what it answers is an OTLP export in the encoding it arrived in, and
// two headers carry what a replay needs to send it on (spec 019 #3).
//
// ErrNotFound is what a 404 becomes, because for this endpoint that is not a
// failure: a batch the sweeper took between the listing and the fetch is a
// thing the caller counts and walks past.
func (c *Client) Fetch(ctx context.Context, path string) ([]byte, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	if c.Key != "" {
		request.Header.Set("Authorization", "Bearer "+c.Key)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer response.Body.Close()

	if c.OnVersion != nil {
		if version := response.Header.Get(VersionHeader); version != "" {
			c.OnVersion(version)
		}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the response from %s: %w", path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, &Error{Status: response.StatusCode, Message: errorMessage(body), Path: path}
	}
	return body, response.Header, nil
}

// Post calls a write endpoint with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return c.Send(ctx, http.MethodPost, path, nil, body)
}

// Send is the general form: any method, an optional query and an optional JSON
// body. The administrative endpoints of spec 005 are the first that are
// neither a plain GET nor a plain POST — a PATCH carrying the wanted state and
// a DELETE carrying only `?confirm=`.
func (c *Client) Send(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return c.do(request, path)
}

func (c *Client) do(request *http.Request, path string) (json.RawMessage, error) {
	if c.Key != "" {
		request.Header.Set("Authorization", "Bearer "+c.Key)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer response.Body.Close()

	if c.OnVersion != nil {
		if version := response.Header.Get(VersionHeader); version != "" {
			c.OnVersion(version)
		}
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot read the response from %s: %w", path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &Error{Status: response.StatusCode, Message: errorMessage(body), Path: path}
	}
	return body, nil
}

// errorMessage reads the one error shape the server uses, falling back to the
// raw body when the failure came from something that is not the server (a
// proxy, a wrong URL pointing at some other service).
func errorMessage(body []byte) string {
	var shape struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &shape); err == nil && shape.Error != "" {
		return shape.Error
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}
