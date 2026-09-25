package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tracepad/tracepad/internal/mcpserver"
)

// What an unauthenticated caller can do to the MCP endpoint with a malformed
// request. The server runs each MCP request in a goroutine that net/http does
// not recover, so a panic there is a crash of the whole process, and in a
// test, of the whole test binary: every test below that finishes is a server
// that stayed up.

// mcpAnswer is what came back from one POST to /mcp.
type mcpAnswer struct {
	status int
	header http.Header
	body   []byte // unwrapped from its server-sent-events frame
}

// postMCP posts one body to /mcp exactly as given, over a fresh connection —
// a stateless server may not depend on one being reused. It sets only what
// the transport requires; a credential and protocol headers are the caller's
// to add.
func postMCP(t *testing.T, base string, body []byte, header http.Header) mcpAnswer {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+mcpserver.Path, bytes.NewReader(body))
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
	return mcpAnswer{status: response.StatusCode, header: response.Header, body: frameBody(answer)}
}

// captureLog routes the default logger into a buffer for the length of the
// test. The logger is process-global, so no test that calls this may run in
// parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &recorded
}

// assertServed checks that a request got past the transport and was answered
// by the server: a call with a JSON-RPC result or error, a notification with
// 202 Accepted. A refusal by the transport is not an answer here — it never
// reaches the code these tests are about.
func assertServed(t *testing.T, answer mcpAnswer, notification bool) {
	t.Helper()
	if notification {
		if answer.status != http.StatusAccepted {
			t.Fatalf("notification: status %d, want 202: %s", answer.status, answer.body)
		}
		return
	}
	if answer.status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", answer.status, answer.body)
	}
	assertJSONRPC(t, answer)
}

// assertJSONRPC checks that the body is a JSON-RPC response to request 1 and
// returns its error code, zero for a result. 2026-07-28 maps some error
// codes to HTTP statuses, so the status alone does not say who answered.
func assertJSONRPC(t *testing.T, answer mcpAnswer) int {
	t.Helper()
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer.body, &envelope); err != nil {
		t.Fatalf("status %d, not JSON-RPC: %v (%s)", answer.status, err, answer.body)
	}
	if envelope.JSONRPC != "2.0" || envelope.ID != 1 || (len(envelope.Result) == 0 && envelope.Error == nil) {
		t.Fatalf("status %d, not a response to request 1: %s", answer.status, answer.body)
	}
	if envelope.Error == nil {
		return 0
	}
	return envelope.Error.Code
}

// assertRefused checks that the transport turned a request away before
// dispatch, with a plain 400 naming why.
func assertRefused(t *testing.T, answer mcpAnswer, why string) {
	t.Helper()
	if answer.status != http.StatusBadRequest || !bytes.Contains(answer.body, []byte(why)) {
		t.Fatalf("status %d, want the transport's 400 %q: %s", answer.status, why, answer.body)
	}
}

var (
	// everyRequest is every request method the SDK dispatches, plus one it
	// does not.
	everyRequest = []string{
		"initialize", "ping", "tools/list", "tools/call", "prompts/list", "prompts/get",
		"resources/list", "resources/templates/list", "resources/read",
		"resources/subscribe", "resources/unsubscribe", "completion/complete",
		"logging/setLevel", "server/discover", "subscriptions/listen", unknownMethod,
	}
	everyNotification = []string{
		"notifications/initialized", "notifications/cancelled",
		"notifications/roots/list_changed", "notifications/progress",
	}
	// paramsOptional are the methods whose `params` the protocol lets a
	// caller leave out. These are the ones that reached the handler chain
	// with a typed nil and crashed it; the SDK's transport refuses the
	// others outright when `params` is missing.
	paramsOptional = map[string]bool{
		"ping": true, "tools/list": true, "prompts/list": true, "resources/list": true,
		"resources/templates/list": true, "server/discover": true,
		"notifications/initialized": true, "notifications/cancelled": true,
		"notifications/roots/list_changed": true,
	}
)

const unknownMethod = "no/such/method"

// message is one JSON-RPC message; a notification carries no id.
func message(method, params string, notification bool) []byte {
	id := `"id":1,`
	if notification {
		id = ""
	}
	return []byte(`{"jsonrpc":"2.0",` + id + `"method":"` + method + `"` + params + `}`)
}

// TestRequestsWithoutParamsAreAnswered: every method the server knows, with
// `params` missing, null and empty, is either answered by the server or
// turned away by the transport before dispatch — each case says which — and
// the server goes on serving. A request that left out the optional `params`
// used to take the process down. Unauthenticated throughout: that is who
// could do it.
func TestRequestsWithoutParamsAreAnswered(t *testing.T) {
	h := newHarness(t)

	// The older protocols carry nothing in `params` the transport insists
	// on. A missing `params` passes it only where the protocol makes them
	// optional; `null` and `{}` pass it everywhere, and the SDK then
	// answers, or the handler chain does.
	shapes := map[string]string{"missing": "", "null": `,"params":null`, "empty": `,"params":{}`}
	for shape, params := range shapes {
		for _, method := range append(append([]string{}, everyRequest...), everyNotification...) {
			notification := strings.HasPrefix(method, "notifications/")
			t.Run("older/"+shape+"/"+method, func(t *testing.T) {
				answer := postMCP(t, h.url, message(method, params, notification), nil)
				switch {
				case method == unknownMethod:
					assertRefused(t, answer, "unsupported")
				case shape == "missing" && !paramsOptional[method]:
					assertRefused(t, answer, `missing required "params"`)
				default:
					assertServed(t, answer, notification)
				}
			})
		}
	}

	// 2026-07-28 requires the protocol version on `_meta`, and the transport
	// refuses a request without it before any of our code runs. So the
	// closest a caller gets to "no params" is `_meta` and nothing else.
	// Whether the request reached the handler chain is read off the log: the
	// `_meta` carries trace context, which the chain's middleware logs.
	recorded := captureLog(t)
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	meta, err := json.Marshal(map[string]any{"_meta": map[string]any{
		mcp.MetaKeyProtocolVersion:    mcpserver.ProtocolVersion,
		mcp.MetaKeyClientCapabilities: map[string]any{},
		"traceparent":                 traceparent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	onlyMeta := `,"params":` + string(meta)
	// What 2026-07-28 does before dispatch: the lifecycle, logging and
	// subscription methods are gone from it (method not found), and a
	// method that acts on a named thing must name it in a header, which a
	// request with no params cannot do.
	const methodNotFound, headerMismatch = -32601, -32020
	refused := map[string]int{
		"initialize": methodNotFound, "ping": methodNotFound, "logging/setLevel": methodNotFound,
		"resources/subscribe": methodNotFound, "resources/unsubscribe": methodNotFound,
		unknownMethod: methodNotFound,
		"tools/call":  headerMismatch, "prompts/get": headerMismatch, "resources/read": headerMismatch,
	}
	for _, method := range everyRequest {
		t.Run("2026-07-28/meta-only/"+method, func(t *testing.T) {
			recorded.Reset()
			code := assertJSONRPC(t, postMCP(t, h.url, message(method, onlyMeta, false), newProtocol(method)))
			reached := strings.Contains(recorded.String(), traceparent)
			if want, isRefused := refused[method]; isRefused {
				if code != want || reached {
					t.Fatalf("code %d, reached the chain %v; want %d before dispatch", code, reached, want)
				}
				return
			}
			if !reached {
				t.Fatalf("answered with code %d without reaching the handler chain", code)
			}
		})
	}
	// Notifications are handled after the 202 goes out, so whether they
	// reached the chain cannot be read off the log here; that the server
	// is still serving afterwards is checked below.
	for _, method := range everyNotification {
		t.Run("2026-07-28/meta-only/"+method, func(t *testing.T) {
			assertServed(t, postMCP(t, h.url, message(method, onlyMeta, true), newProtocol(method)), true)
		})
	}
	// A tool call that names its tool and nothing else does reach it.
	t.Run("2026-07-28/name-only/tools/call", func(t *testing.T) {
		recorded.Reset()
		header := newProtocol("tools/call")
		header.Set("Mcp-Name", "list_traces")
		named := `,"params":{"name":"list_traces",` + string(meta[1:])
		answer := postMCP(t, h.url, message("tools/call", named, false), header)
		assertServed(t, answer, false)
		if !strings.Contains(recorded.String(), traceparent) {
			t.Fatalf("the call did not reach the handler chain: %s", answer.body)
		}
	})

	// The exact request that used to crash the server now lists the tools.
	answer := postMCP(t, h.url, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), nil)
	if answer.status != http.StatusOK || !bytes.Contains(answer.body, []byte("get_last_trace")) {
		t.Fatalf("tools/list without params: %d %s", answer.status, answer.body)
	}
	answer = postMCP(t, h.url, []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`), nil)
	if answer.status != http.StatusOK || !bytes.Contains(answer.body, []byte(`"result"`)) {
		t.Fatalf("ping without params: %d %s", answer.status, answer.body)
	}
	// And the rest of the server is still there.
	if body := h.get(t, "/api/v1/traces"); !bytes.Contains(body, []byte(traceHex(1))) {
		t.Fatalf("the read API stopped answering: %s", body)
	}
}

// newProtocol is the 2026-07-28 header set for one method: the version, and
// the method itself, which the transport checks against the body.
func newProtocol(method string) http.Header {
	return http.Header{
		"Mcp-Protocol-Version": {mcpserver.ProtocolVersion},
		"Mcp-Method":           {method},
	}
}

// panickingAPI stands in for a tool handler with a bug in it, one whose
// panic message is built from the caller's input.
type panickingAPI struct{}

func (panickingAPI) Get(_ context.Context, _ string, query url.Values, _ string) (json.RawMessage, error) {
	panic(fmt.Sprintf("a bug in a tool handler, on %v", query))
}

// TestPanicIsAnErrorNotACrash: a panic in the MCP handler chain is a JSON-RPC
// internal error for that request, the next request is served, and the log
// gets the panic's type and stack but not its value, which here carries the
// request.
func TestPanicIsAnErrorNotACrash(t *testing.T) {
	recorded := captureLog(t)

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
	for _, leak := range []string{marker, "a bug in a tool handler"} {
		if strings.Contains(log, leak) {
			t.Fatalf("the log carries the panic's value (%q):\n%s", leak, log)
		}
	}
	for _, want := range []string{"mcp handler panicked", "method=tools/call", "panic_type=string", "robustness_test.go"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log misses %q:\n%s", want, log)
		}
	}
}

// TestOnlyWellFormedTraceContextIsLogged: trace context reaches the log once
// per request and only when it is well-formed W3C trace context. The log line
// is written before any credential is checked, so the malformed cases go in
// the way an attacker would send them — unauthenticated, older protocol, no
// headers beyond what the transport requires — and anything that is not
// trace context — oversized, malformed, a forged log line — is dropped rather
// than written.
func TestOnlyWellFormedTraceContextIsLogged(t *testing.T) {
	h := newHarness(t)
	recorded := captureLog(t)

	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	unauthenticated := func(t *testing.T, traceparent string) {
		t.Helper()
		params, err := json.Marshal(map[string]any{"_meta": map[string]any{"traceparent": traceparent}})
		if err != nil {
			t.Fatal(err)
		}
		assertServed(t, postMCP(t, h.url, message("tools/list", `,"params":`+string(params), false), nil), false)
	}

	// Once per request, on the 2026-07-28 path and on the older one.
	h.rpc(t, "tools/list", map[string]any{"_meta": map[string]any{"traceparent": valid}})
	if n := strings.Count(recorded.String(), valid); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, recorded.String())
	}
	recorded.Reset()
	unauthenticated(t, valid)
	if n := strings.Count(recorded.String(), valid); n != 1 {
		t.Fatalf("older protocol: logged %d times, want once:\n%s", n, recorded.String())
	}

	invalid := map[string]string{
		"oversized":      "00-" + strings.Repeat("a", 64<<10),
		"forged line":    valid + "\nlevel=ERROR msg=forged",
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
			unauthenticated(t, traceparent)
			if strings.Contains(recorded.String(), "traceparent") {
				t.Fatalf("malformed trace context reached the log:\n%.500s", recorded.String())
			}
		})
	}
}
