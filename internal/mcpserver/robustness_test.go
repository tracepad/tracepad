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
	"sync"
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
func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	recorded := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return recorded
}

// lockedLog is a log buffer the server's goroutines can write while the test
// reads it: a notification, for one, is handled after its 202 has gone out.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lockedLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
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
// says whether it is an error, and with which code. 2026-07-28 maps some
// error codes to HTTP statuses, so the status alone does not say who
// answered.
func assertJSONRPC(t *testing.T, answer mcpAnswer) (failed bool, code int) {
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
		return false, 0
	}
	return true, envelope.Error.Code
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
	// resultWithEmptyParams are the methods whose `params` are required but
	// have no required field, so `{}` is a complete request to them.
	resultWithEmptyParams = map[string]bool{"initialize": true, "logging/setLevel": true}
)

// laterMethod is a 2026-07-28 method, unknown to the older protocols.
const laterMethod = "server/discover"

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
	// optional; `null` and `{}` pass it everywhere. Then, as spec 004's edge
	// case has it, a method with optional `params` answers as if they were
	// `{}`, and one with required `params` refuses `null` with an error —
	// and answers `{}` like any request, which is a result where no field of
	// its `params` is required.
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
				case notification:
					assertServed(t, answer, true)
				default:
					assertServed(t, answer, false)
					wantResult := method != laterMethod && (paramsOptional[method] ||
						shape == "empty" && resultWithEmptyParams[method])
					if failed, code := assertJSONRPC(t, answer); failed == wantResult {
						t.Fatalf("error %v (code %d), want a result %v: %s", failed, code, wantResult, answer.body)
					}
				}
			})
		}
	}

	// 2026-07-28 requires the protocol version on `_meta`, and the transport
	// refuses a request without it before any of our code runs. So the
	// closest a caller gets to "no params" is `_meta` and nothing else.
	// Whether a request reached the handler chain is read off the log: its
	// `_meta` carries trace context, which the chain's middleware logs.
	recorded := captureLog(t)
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	metaWith := func(extra map[string]any) []byte {
		fields := map[string]any{
			mcp.MetaKeyProtocolVersion:    mcpserver.ProtocolVersion,
			mcp.MetaKeyClientCapabilities: map[string]any{},
		}
		for key, value := range extra {
			fields[key] = value
		}
		meta, err := json.Marshal(map[string]any{"_meta": fields})
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}
	meta := metaWith(map[string]any{"traceparent": traceparent})
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
			_, code := assertJSONRPC(t, postMCP(t, h.url, message(method, onlyMeta, false), newProtocol(method)))
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
	// is still serving afterwards is checked below. Their `_meta` carries no
	// trace context: a line one of them logs late would otherwise pass for
	// the next subtest's.
	notificationMeta := `,"params":` + string(metaWith(nil))
	for _, method := range everyNotification {
		t.Run("2026-07-28/meta-only/"+method, func(t *testing.T) {
			assertServed(t, postMCP(t, h.url, message(method, notificationMeta, true), newProtocol(method)), true)
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

// panickingAPI stands in for a tool handler with a bug in it.
type panickingAPI struct{ bug func(query url.Values) }

func (p panickingAPI) Get(_ context.Context, _ string, query url.Values, _ string) (json.RawMessage, error) {
	p.bug(query)
	return nil, nil
}

// panickingServer serves MCP over tools whose every call panics.
func panickingServer(t *testing.T, bug func(url.Values)) *harness {
	t.Helper()
	httpServer := httptest.NewServer(mcpserver.HTTPHandler(testVersion, panickingAPI{bug: bug}))
	t.Cleanup(httpServer.Close)
	return &harness{url: httpServer.URL}
}

// panickingCall calls a tool that panics and checks the caller gets a
// JSON-RPC internal error that says nothing about the panic.
func panickingCall(t *testing.T, h *harness, tool string, arguments map[string]any) {
	t.Helper()
	answer := h.rpc(t, "tools/call", map[string]any{"name": tool, "arguments": arguments})
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		t.Fatalf("%v (%s)", err, answer)
	}
	if envelope.Error.Code != -32603 || envelope.Error.Message != "internal error" {
		t.Fatalf("want a bare JSON-RPC internal error (-32603), got %s", answer)
	}
}

// TestPanicIsAnErrorNotACrash: a panic in the MCP handler chain is a JSON-RPC
// internal error for that request, and the next request is served. The log
// gets the panic's type, where it was raised and in which tool, and the
// stack — but not the panic's value, which here is built from the request.
// The stack comes once per kind, not once per request: a caller who can
// trigger a panic can do it in a loop (the timing of the summaries is
// TestPanicSummariesAreTimed's).
func TestPanicIsAnErrorNotACrash(t *testing.T) {
	recorded := captureLog(t)
	h := panickingServer(t, func(query url.Values) {
		panic(fmt.Sprintf("a bug in a tool handler, on %v", query))
	})

	const marker = "request-body-marker-7f3a"
	panickingCall(t, h, "list_traces", map[string]any{"name": marker})
	if tools := h.rpc(t, "tools/list", nil); !bytes.Contains(tools, []byte("get_last_trace")) {
		t.Fatalf("the next request was not served: %s", tools)
	}

	log := recorded.String()
	for _, leak := range []string{marker, "a bug in a tool handler"} {
		if strings.Contains(log, leak) {
			t.Fatalf("the log carries the panic's value (%q):\n%s", leak, log)
		}
	}
	for _, want := range []string{"mcp handler panicked", "method=tools/call", "tool=list_traces",
		"panic_type=string", "site=github.com/tracepad/tracepad/internal/mcpserver_test.TestPanicIsAnErrorNotACrash",
		"stack=", "robustness_test.go"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log misses %q:\n%s", want, log)
		}
	}

	// Seven more of the same inside the minute add nothing; the same bug
	// reached through another tool is a kind of its own.
	for range 7 {
		panickingCall(t, h, "list_traces", map[string]any{"name": marker})
	}
	if log := recorded.String(); strings.Count(log, "stack=") != 1 || strings.Contains(log, "repeated") {
		t.Fatalf("repeats inside the minute were logged:\n%s", log)
	}
	panickingCall(t, h, "get_last_trace", map[string]any{})
	if log := recorded.String(); strings.Count(log, "stack=") != 2 || !strings.Contains(log, "tool=get_last_trace") {
		t.Fatalf("a second tool's panic was folded into the first:\n%s", log)
	}
}

// TestRuntimeErrorPanicKeepsItsMessage: a panic the Go runtime raised is
// logged with the runtime's own message, which names what went wrong; the
// most of the request it can carry is a number derived from it (here an
// index), never the request's bytes. Its site is the code that made it, not
// the runtime's frames above it.
func TestRuntimeErrorPanicKeepsItsMessage(t *testing.T) {
	recorded := captureLog(t)
	h := panickingServer(t, func(query url.Values) {
		var none []string
		_ = none[len(query)]
	})

	panickingCall(t, h, "list_traces", map[string]any{"name": "request-body-marker-7f3a"})
	log := recorded.String()
	if !strings.Contains(log, "index out of range") || !strings.Contains(log, "panic_type=runtime.boundsError") ||
		!strings.Contains(log, "site=github.com/tracepad/tracepad/internal/mcpserver_test.TestRuntimeErrorPanicKeepsItsMessage") {
		t.Fatalf("the runtime's message did not reach the log:\n%s", log)
	}
	if strings.Contains(log, "request-body-marker-7f3a") {
		t.Fatalf("the log carries the request:\n%s", log)
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

	// A later version is read the way W3C Trace Context §3.2.4 tells a
	// parser to: its first 55 characters as version 00, when a dash or
	// nothing follows them. Only those 55 reach the log.
	later := "01" + valid[2:]
	for name, traceparent := range map[string]string{
		"later version":           later,
		"later version, extended": later + "-" + strings.Repeat("b", 64<<10),
	} {
		t.Run(name, func(t *testing.T) {
			recorded.Reset()
			unauthenticated(t, traceparent)
			log := recorded.String()
			if n := strings.Count(log, later); n != 1 || strings.Contains(log, later+"-") {
				t.Fatalf("want exactly the first 55 characters, once:\n%.500s", log)
			}
		})
	}

	invalid := map[string]string{
		"oversized":              "00-" + strings.Repeat("a", 64<<10),
		"forged line":            valid + "\nlevel=ERROR msg=forged",
		"version 00, extended":   valid + "-extra",
		"short":                  valid[:54],
		"uppercase":              strings.ToUpper(valid),
		"version ff":             "ff" + valid[2:],
		"later version, no dash": later + "x",
		"zero trace id":          "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero parent id":         "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"not hex":                "00-4bf92f3577b34da6a3ce929d0e0e473z-00f067aa0ba902b7-01",
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
