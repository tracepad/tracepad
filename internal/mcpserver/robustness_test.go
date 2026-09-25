package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mcpserver"
)

// What an unauthenticated caller can do to the MCP endpoint with a malformed
// request. The server runs each MCP request in a goroutine that net/http does
// not recover, so a panic there is a crash of the whole process, and in a
// test, of the whole test binary: every test below that finishes is a server
// that stayed up.

// rawMCP posts one body to /mcp exactly as given, with no credential, and
// returns the status and the unwrapped answer.
func rawMCP(t *testing.T, base, body string, header http.Header) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+mcpserver.Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		t.Fatalf("%s: no answer: %v", body, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, frameBody(answer)
}

// assertAnswered checks that a request got an answer the protocol allows: a
// JSON-RPC result or error, a plain HTTP refusal, or an accepted
// notification. What it may not get is a dropped connection.
func assertAnswered(t *testing.T, status int, answer []byte) {
	t.Helper()
	if status >= 500 {
		t.Fatalf("status %d: %s", status, answer)
	}
	if status != http.StatusOK || len(answer) == 0 {
		return
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		t.Fatalf("not JSON-RPC: %v (%s)", err, answer)
	}
	if envelope.JSONRPC != "2.0" || (len(envelope.Result) == 0 && len(envelope.Error) == 0) {
		t.Fatalf("neither a result nor an error: %s", answer)
	}
}

// TestRequestsWithoutParamsAreAnswered: every method the server knows, with
// `params` missing, null and empty, over the older protocol and over
// 2026-07-28, gets an answer, and the server goes on serving. A request that
// leaves out the optional `params` used to take the process down.
func TestRequestsWithoutParamsAreAnswered(t *testing.T) {
	h := newHarness(t)

	requests := []string{
		"initialize", "ping", "tools/list", "tools/call", "prompts/list", "prompts/get",
		"resources/list", "resources/templates/list", "resources/read",
		"resources/subscribe", "resources/unsubscribe", "completion/complete",
		"logging/setLevel", "server/discover", "subscriptions/listen", "no/such/method",
	}
	notifications := []string{
		"notifications/initialized", "notifications/cancelled",
		"notifications/roots/list_changed", "notifications/progress",
	}
	shapes := map[string]string{"missing": "", "null": `,"params":null`, "empty": `,"params":{}`}
	protocols := map[string]http.Header{
		"older":      nil,
		"2026-07-28": {"Mcp-Protocol-Version": {mcpserver.ProtocolVersion}},
	}

	for protocol, header := range protocols {
		for shape, params := range shapes {
			for _, method := range requests {
				t.Run(protocol+"/"+shape+"/"+method, func(t *testing.T) {
					body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"` + params + `}`
					status, answer := rawMCP(t, h.url, body, withMethod(header, method))
					assertAnswered(t, status, answer)
				})
			}
			for _, method := range notifications {
				t.Run(protocol+"/"+shape+"/"+method, func(t *testing.T) {
					body := `{"jsonrpc":"2.0","method":"` + method + `"` + params + `}`
					status, answer := rawMCP(t, h.url, body, withMethod(header, method))
					assertAnswered(t, status, answer)
				})
			}
		}
	}

	// The exact request that used to crash the server now lists the tools.
	status, answer := rawMCP(t, h.url, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if status != http.StatusOK || !bytes.Contains(answer, []byte("get_last_trace")) {
		t.Fatalf("tools/list without params: %d %s", status, answer)
	}
	status, answer = rawMCP(t, h.url, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, nil)
	if status != http.StatusOK || !bytes.Contains(answer, []byte(`"result"`)) {
		t.Fatalf("ping without params: %d %s", status, answer)
	}
	// And the rest of the server is still there.
	if body := h.get(t, "/api/v1/traces"); !bytes.Contains(body, []byte(traceHex(1))) {
		t.Fatalf("the read API stopped answering: %s", body)
	}
}

// withMethod adds the 2026-07-28 routing header when the protocol header is
// set, so the request is refused or served on its body rather than on a
// header mismatch.
func withMethod(header http.Header, method string) http.Header {
	if header == nil {
		return nil
	}
	header = header.Clone()
	header.Set("Mcp-Method", method)
	return header
}

// panickingAPI stands in for a tool handler with a bug in it.
type panickingAPI struct{}

func (panickingAPI) Get(context.Context, string, url.Values, string) (json.RawMessage, error) {
	panic("a bug in a tool handler")
}

// TestPanicIsAnErrorNotACrash: a panic anywhere in the MCP handler chain is a
// JSON-RPC internal error for that request, the next request is served, and
// the log gets the panic and its stack but not the request.
func TestPanicIsAnErrorNotACrash(t *testing.T) {
	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	httpServer := httptest.NewServer(mcpserver.HTTPHandler(testVersion, panickingAPI{}))
	t.Cleanup(httpServer.Close)
	h := &harness{url: httpServer.URL}

	const marker = "request-body-marker-7f3a"
	answer := h.rpc(t, "tools/call", map[string]any{
		"name":      "list_traces",
		"arguments": map[string]any{"name": marker},
	})
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		t.Fatalf("%v (%s)", err, answer)
	}
	if envelope.Error.Code != -32603 {
		t.Fatalf("want a JSON-RPC internal error (-32603), got %s", answer)
	}
	if strings.Contains(string(answer), "a bug in a tool handler") {
		t.Fatalf("the panic leaked to the caller: %s", answer)
	}

	if tools := h.rpc(t, "tools/list", nil); !bytes.Contains(tools, []byte("get_last_trace")) {
		t.Fatalf("the next request was not served: %s", tools)
	}

	log := recorded.String()
	for _, want := range []string{"mcp handler panicked", "a bug in a tool handler", "method=tools/call", "robustness_test.go"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log misses %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, marker) {
		t.Fatalf("the log carries the request:\n%s", log)
	}
}

// TestOnlyWellFormedTraceContextIsLogged: trace context reaches the log once
// per request and only when it is well-formed W3C trace context. The log line
// is written before any credential is checked, so anything else — oversized,
// malformed, a forged log line — is dropped rather than written.
func TestOnlyWellFormedTraceContextIsLogged(t *testing.T) {
	h := newHarness(t)

	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	withTraceparent := func(traceparent string) map[string]any {
		return map[string]any{"_meta": map[string]any{"traceparent": traceparent}}
	}

	// Once per request, on the 2026-07-28 path and on the older one.
	h.rpc(t, "tools/list", withTraceparent(valid))
	if n := strings.Count(recorded.String(), valid); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, recorded.String())
	}
	recorded.Reset()
	status, answer := rawMCP(t, h.url,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"traceparent":"`+valid+`"}}}`, nil)
	assertAnswered(t, status, answer)
	if n := strings.Count(recorded.String(), valid); n != 1 {
		t.Fatalf("older protocol: logged %d times, want once:\n%s", n, recorded.String())
	}

	invalid := map[string]string{
		"oversized":      "00-" + strings.Repeat("a", 64<<10),
		"forged line":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\nlevel=ERROR msg=forged",
		"trailing bytes": valid + "-extra",
		"uppercase":      strings.ToUpper(valid),
		"other version":  "01" + valid[2:],
		"zero trace id":  "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero parent id": "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"not hex":        "00-4bf92f3577b34da6a3ce929d0e0e473z-00f067aa0ba902b7-01",
	}
	for name, traceparent := range invalid {
		t.Run(name, func(t *testing.T) {
			recorded.Reset()
			h.rpc(t, "tools/list", withTraceparent(traceparent))
			if strings.Contains(recorded.String(), "traceparent") {
				t.Fatalf("malformed trace context reached the log:\n%.500s", recorded.String())
			}
		})
	}
}
