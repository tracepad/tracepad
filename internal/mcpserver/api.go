// Package mcpserver exposes the read API as MCP tools (spec 004 #14–#18).
//
// Every tool handler calls the HTTP read API — over an in-process loopback
// when the server is embedded at `/mcp`, over the network in stdio mode — and
// never the store (#16). That is what makes budgets, truncation, auth and JSON
// shape have exactly one implementation: a tool result and a curl of the
// corresponding endpoint carry the same bytes, and the rule that a feature
// does not exist until it is in the read API holds by construction.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/tracepad/tracepad/internal/client"
)

// API performs one read-API GET on behalf of a tool.
type API interface {
	// Get returns the endpoint's response body. The credential is the
	// caller's own `Authorization` header when there is one; an
	// implementation that holds its own key ignores it.
	Get(ctx context.Context, path string, query url.Values, credential string) (json.RawMessage, error)
}

// Loopback calls the server's own handler in process. No socket, no second
// authentication story: the credential the MCP request arrived with is the
// credential the read API sees.
type Loopback struct {
	Handler http.Handler
}

func (l *Loopback) Get(ctx context.Context, path string, query url.Values, credential string) (json.RawMessage, error) {
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	// The host is a placeholder: nothing routes on it, and the request
	// never leaves this process.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://loopback"+target, nil)
	if err != nil {
		return nil, err
	}
	if credential != "" {
		request.Header.Set("Authorization", credential)
	}
	recorder := &recorder{header: http.Header{}}
	l.Handler.ServeHTTP(recorder, request)

	body := bytes.TrimRight(recorder.body.Bytes(), "\n")
	if recorder.status != 0 && (recorder.status < 200 || recorder.status >= 300) {
		return nil, &client.Error{Status: recorder.status, Message: apiError(body), Path: path}
	}
	return body, nil
}

// Remote calls a running server over HTTP, for the stdio mode local clients
// use (#15). It is the same tool registry with a different transport.
type Remote struct {
	Client *client.Client
}

func (r *Remote) Get(ctx context.Context, path string, query url.Values, _ string) (json.RawMessage, error) {
	body, err := r.Client.Get(ctx, path, query)
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(body, "\n"), nil
}

// recorder collects a loopback response. It is http.ResponseWriter reduced to
// what an in-process JSON call needs; the alternative, httptest.NewRecorder,
// belongs to the testing half of the standard library.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// apiError reads the one error shape the server uses.
func apiError(body []byte) string {
	var shape struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &shape); err == nil && shape.Error != "" {
		return shape.Error
	}
	return fmt.Sprintf("the server answered %s", body)
}
