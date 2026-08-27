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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/server"
	"github.com/tracepad/tracepad/internal/store"
)

// The MCP surface (spec 004, Testing #4). The tests drive the real go-sdk
// client against the real server over both transports; nothing is stubbed,
// because the invariant under test is that a tool call and a curl of the
// corresponding endpoint are the same answer (#16).

const (
	testKey     = "tp-sk-test-secret"
	testVersion = "test"
)

// seedBase is a fixed instant (2026-09-01T00:00:00Z) in Unix nanoseconds.
const seedBase int64 = 1788220800_000_000_000

const ms = int64(1_000_000)

type harness struct {
	url    string
	store  *store.Store
	writer *store.Writer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	project, err := st.CreateProject("test", store.KeyPair{PublicKey: "tp-pk-test", Secret: testKey})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := st.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	cfg := &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes, MCP: true}
	httpServer := httptest.NewServer(server.New(cfg, testVersion, st, writer).Handler())
	t.Cleanup(httpServer.Close)

	h := &harness{url: httpServer.URL, store: st, writer: writer}
	h.seed(t, project.ID)
	return h
}

func traceHex(n int) string { return fmt.Sprintf("%032x", n) }
func spanHex(n int) string  { return fmt.Sprintf("%016x", n) }

func (h *harness) seed(t *testing.T, projectID string) {
	t.Helper()
	batches := []*store.IngestBatch{
		{
			ProjectID: projectID,
			Traces: []*model.Trace{{ID: traceHex(1), Name: "support-chat", UserID: "u1",
				SessionID: "s1", Environment: "production", Tags: []string{"beta"}}},
			Observations: []*model.Observation{
				{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan, Name: "handle",
					Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 800*ms},
				{TraceID: traceHex(1), ID: spanHex(2), ParentObservationID: spanHex(1),
					Type: model.TypeGeneration, Name: "chat", Model: "claude-sonnet-5",
					Level: model.LevelDefault, StartTime: seedBase + 40*ms, EndTime: seedBase + 780*ms,
					CostDetails: map[string]any{"total": 0.001},
					Input:       []any{map[string]any{"role": "user", "content": "how do I reset my password?"}},
					Output:      map[string]any{"role": "assistant", "content": "Open Settings."}},
			},
		},
		{
			ProjectID: projectID,
			Traces:    []*model.Trace{{ID: traceHex(2), Name: "nightly-eval", Environment: "staging"}},
			Observations: []*model.Observation{
				{TraceID: traceHex(2), ID: spanHex(3), Type: model.TypeGeneration, Name: "judge",
					Level: model.LevelError, StatusMessage: "rate limited",
					StartTime: seedBase + 1000*ms, EndTime: seedBase + 1300*ms},
			},
		},
	}
	for _, batch := range batches {
		if err := h.writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	scores := []byte(`{"trace_id":"` + traceHex(1) + `","name":"helpfulness","value":0.9}`)
	h.post(t, "/api/v1/scores", scores)
	prompt := []byte(`{"type":"text","prompt":"Be brief.","labels":["production"]}`)
	h.post(t, "/api/v1/prompts/support/versions", prompt)
}

func (h *harness) post(t *testing.T, path string, body []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		answer, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s: %d %s", path, response.StatusCode, answer)
	}
}

// get fetches an endpoint the way curl would, for the byte-identity check.
func (h *harness) get(t *testing.T, path string) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, response.StatusCode, body)
	}
	// The endpoint's encoder ends with a newline; the tool result carries
	// the same bytes without it.
	return bytes.TrimRight(body, "\n")
}

// rpc sends one stateless JSON-RPC request to /mcp the way a 2026-07-28
// client does: no handshake, the protocol version in `_meta` and in the
// header, and nothing carried over from a previous request.
func (h *harness) rpc(t *testing.T, method string, params map[string]any) []byte {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[mcp.MetaKeyProtocolVersion] = mcpserver.ProtocolVersion
	meta[mcp.MetaKeyClientCapabilities] = map[string]any{}
	params["_meta"] = meta
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.url+mcpserver.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	// 2026-07-28 puts the method (and, for a tool call, its name) in
	// headers so a proxy can route and authorize without parsing the body.
	request.Header.Set("Mcp-Protocol-Version", mcpserver.ProtocolVersion)
	request.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		request.Header.Set("Mcp-Name", name)
	}

	// A fresh connection every time: a stateless server may not depend on
	// one being reused.
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, response.StatusCode, answer)
	}
	if id := response.Header.Get("Mcp-Session-Id"); id != "" {
		t.Fatalf("%s: the server issued session %q; 2026-07-28 is sessionless", method, id)
	}
	return frameBody(answer)
}

// frameBody unwraps a server-sent-events frame, which is what streamable HTTP
// answers with by default. A plain JSON body passes through.
func frameBody(answer []byte) []byte {
	for _, line := range bytes.Split(answer, []byte("\n")) {
		if after, found := bytes.CutPrefix(line, []byte("data: ")); found {
			return after
		}
	}
	return bytes.TrimSpace(answer)
}

// callRaw calls one tool over the wire and returns its `structuredContent`
// exactly as it was serialized.
func (h *harness) callRaw(t *testing.T, name string, arguments map[string]any) json.RawMessage {
	t.Helper()
	answer := h.rpc(t, "tools/call", map[string]any{"name": name, "arguments": arguments})
	var envelope struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		t.Fatalf("%s: %v (%s)", name, err, answer)
	}
	if len(envelope.Error) > 0 {
		t.Fatalf("%s: %s", name, envelope.Error)
	}
	if envelope.Result.IsError {
		t.Fatalf("%s failed: %s", name, answer)
	}
	return envelope.Result.StructuredContent
}

// connect opens an MCP session over streamable HTTP against the running
// server, carrying the project key the way a client configured with static
// headers does (#20).
func (h *harness) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   h.url + mcpserver.Path,
		HTTPClient: &http.Client{Transport: keyed(http.DefaultTransport)},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect to %s: %v", transport.Endpoint, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// keyed adds the project key to every request, which is how an MCP client
// authenticates against an HTTP server.
func keyed(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+testKey)
		return next.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestToolListIsTheDeclaredContract: eight read-only tools, a deterministic
// order, and the caching hints of #18.
func TestToolListIsTheDeclaredContract(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"get_last_trace", "get_observation_io", "get_prompt", "get_session",
		"get_stats", "get_trace", "list_scores", "list_traces"}
	var names []string
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want exactly %v in that order", names, want)
	}
	// There is no `search`: no search endpoint exists yet, and a tool
	// faking one over list filters would misrepresent capability (#17).
	if slices.Contains(names, "search") {
		t.Errorf("a search tool exists without a search endpoint behind it")
	}

	for _, tool := range result.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: no readOnlyHint; the whole surface reads", tool.Name)
		}
		if tool.Annotations == nil || tool.Annotations.Title == "" {
			t.Errorf("%s: no title", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s: no outputSchema; clients cannot validate what they get", tool.Name)
		}
		// A description that only restates the endpoint tells a model
		// nothing about when to reach for it (#17).
		if len(tool.Description) < 120 {
			t.Errorf("%s: description is %d characters, too short to be a when-to-use trigger",
				tool.Name, len(tool.Description))
		}
	}

	if result.TTLMs != 3600000 {
		t.Errorf("ttlMs = %d, want an hour: the tool list changes only when the binary does", result.TTLMs)
	}
	if result.CacheScope != "private" {
		t.Errorf("cacheScope = %q, want private for an authenticated per-project surface", result.CacheScope)
	}

	// Deterministic: asked twice, answered the same, which is what makes
	// the list prompt-cache friendly.
	again, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(result.Tools)
	second, _ := json.Marshal(again.Tools)
	if !bytes.Equal(first, second) {
		t.Errorf("two tools/list calls disagree")
	}
}

// TestEveryToolMatchesItsEndpoint is invariant #16: every tool's
// structuredContent is byte-for-byte the corresponding endpoint's body.
//
// The comparison is made on the wire rather than through the SDK client,
// because a client decodes `structuredContent` into a generic map and
// re-encoding that would sort the keys — the test would then pass on any
// server that merely returned the same *values*, which is a weaker claim than
// the one Decision 16 makes.
func TestEveryToolMatchesItsEndpoint(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	for _, tc := range []struct {
		tool      string
		arguments map[string]any
		endpoint  string
	}{
		{"list_traces", map[string]any{"environment": "production"},
			"/api/v1/traces?environment=production"},
		{"list_traces", map[string]any{"status": "error", "limit": 10},
			"/api/v1/traces?limit=10&status=error"},
		{"get_trace", map[string]any{"trace_id": traceHex(1), "expand": "io"},
			"/api/v1/traces/" + traceHex(1) + "?expand=io"},
		{"get_last_trace", map[string]any{"status": "error"},
			"/api/v1/traces/last?status=error"},
		{"get_observation_io", map[string]any{"observation_id": spanHex(2), "trace_id": traceHex(1)},
			"/api/v1/observations/" + spanHex(2) + "/io?trace_id=" + traceHex(1)},
		{"get_session", map[string]any{"session_id": "s1"}, "/api/v1/sessions/s1"},
		{"get_prompt", map[string]any{"name": "support", "label": "production"},
			"/api/v1/prompts/support?label=production"},
		{"list_scores", map[string]any{"trace_id": traceHex(1)},
			"/api/v1/scores?trace_id=" + traceHex(1)},
		{"get_stats", map[string]any{"group_by": "model"}, "/api/v1/stats?group_by=model"},
	} {
		t.Run(tc.tool+" "+tc.endpoint, func(t *testing.T) {
			structured := h.callRaw(t, tc.tool, tc.arguments)
			if want := h.get(t, tc.endpoint); !bytes.Equal(structured, want) {
				t.Fatalf("the tool and the endpoint disagree:\ntool: %s\ncurl: %s",
					structured, want)
			}
			// The same call through the SDK client answers without
			// error and carries a short line of text beside the
			// structured result, for clients and models that read
			// `content` (#18).
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: tc.tool, Arguments: tc.arguments,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("tool failed: %s", textOf(result))
			}
			if strings.TrimSpace(textOf(result)) == "" {
				t.Errorf("no text summary alongside the structured result")
			}
		})
	}
}

// TestTruncationMarkerIsReachableAsATool is why get_observation_io exists at
// all (#17): the expansion affordance of #2 has to be reachable without a URL
// fetcher, or the markers are dead ends for a pure MCP consumer.
func TestTruncationMarkerIsReachableAsATool(t *testing.T) {
	h := newHarness(t)
	huge := strings.Repeat("payload ", 400_000)
	project, err := h.store.ProjectByName("test")
	if err != nil {
		t.Fatal(err)
	}
	err = h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: project.ID,
		Traces:    []*model.Trace{{ID: traceHex(3)}},
		Observations: []*model.Observation{{TraceID: traceHex(3), ID: spanHex(9),
			Type: model.TypeGeneration, Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + ms, Input: huge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := h.connect(t)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_trace", Arguments: map[string]any{"trace_id": traceHex(3), "expand": "io"},
	})
	if err != nil {
		t.Fatal(err)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		Observations []struct {
			Input struct {
				Truncated     bool   `json:"truncated"`
				TraceID       string `json:"trace_id"`
				ObservationID string `json:"observation_id"`
			} `json:"input"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(structured, &trace); err != nil {
		t.Fatalf("the oversized payload is not a marker: %v", err)
	}
	marker := trace.Observations[0].Input
	if !marker.Truncated {
		t.Fatalf("payload came back whole, want a truncation marker")
	}

	// The marker's id pair is exactly what the tool takes.
	whole, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_observation_io",
		Arguments: map[string]any{
			"observation_id": marker.ObservationID,
			"trace_id":       marker.TraceID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if whole.IsError {
		t.Fatalf("following the marker failed: %s", textOf(whole))
	}
	recovered, err := json.Marshal(whole.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var payloads struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(recovered, &payloads); err != nil {
		t.Fatal(err)
	}
	if payloads.Input != huge {
		t.Fatalf("recovered %d bytes of the %d-byte payload", len(payloads.Input), len(huge))
	}
}

// TestToolErrorsAreToolErrors: a refusal by the API reaches the model rather
// than dying as a protocol error, and it carries the server's own message.
func TestToolErrorsAreToolErrors(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_trace", Arguments: map[string]any{"trace_id": traceHex(99)},
	})
	if err != nil {
		t.Fatalf("a missing trace should not be a protocol error: %v", err)
	}
	if !result.IsError || !strings.Contains(textOf(result), "not found") {
		t.Fatalf("result = %+v, want a tool error naming what was not found", result)
	}
}

// TestTightSchemasRefuseBadCalls: the constraints in the input schema are the
// contract, and the SDK enforces them before the handler runs (#18).
func TestTightSchemasRefuseBadCalls(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	for _, tc := range []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{"a trace id that is not one", "get_trace", map[string]any{"trace_id": "nope"}},
		{"a grouping that does not exist", "get_stats", map[string]any{"group_by": "week"}},
		{"a page size out of range", "list_traces", map[string]any{"limit": 5000}},
		{"a status outside the enum", "list_traces", map[string]any{"status": "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: tc.tool, Arguments: tc.arguments,
			})
			if err == nil && !result.IsError {
				t.Fatalf("the call was accepted; the schema should have refused it")
			}
		})
	}
}

// TestStatelessTransport is #14: no handshake, no session header, a fresh
// connection each time. Every request stands alone, which is what lets any
// load balancer serve it.
//
// The session-header check lives in rpc, so every raw call in this file makes
// it; here it is asserted that two independent requests both work with nothing
// carried between them.
func TestStatelessTransport(t *testing.T) {
	h := newHarness(t)

	for attempt := range 2 {
		answer := h.rpc(t, "tools/list", nil)
		if !strings.Contains(string(answer), "list_traces") {
			t.Fatalf("attempt %d: no tool list without a handshake: %s", attempt, answer)
		}
	}
	// And a tool call, with no initialize before it either.
	traces := h.callRaw(t, "list_traces", map[string]any{"limit": 1})
	if !bytes.Contains(traces, []byte(`"traces"`)) {
		t.Fatalf("a tool call without a handshake answered %s", traces)
	}
}

// TestStdioTransportServesTheSameTools is #15: the same registry, a different
// transport, and a credential that comes from configuration rather than from a
// header.
func TestStdioTransportServesTheSameTools(t *testing.T) {
	h := newHarness(t)

	api, err := client.New(h.url, testKey)
	if err != nil {
		t.Fatal(err)
	}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	stdio := mcpserver.New(testVersion, &mcpserver.Remote{Client: api})
	go func() {
		if err := stdio.Run(context.Background(), serverSide); err != nil {
			t.Errorf("stdio server: %v", err)
		}
	}()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 8 {
		t.Fatalf("tools = %d, want the same eight as over HTTP", len(tools.Tools))
	}

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "list_traces", Arguments: map[string]any{"environment": "production"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool failed: %s", textOf(result))
	}
	// The client decodes structuredContent into a generic map, so this
	// compares values rather than bytes; the byte-level claim is made over
	// the wire in TestEveryToolMatchesItsEndpoint.
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var overStdio, overHTTP any
	if err := json.Unmarshal(structured, &overStdio); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(h.get(t, "/api/v1/traces?environment=production"), &overHTTP); err != nil {
		t.Fatal(err)
	}
	fromStdio, _ := json.Marshal(overStdio)
	fromHTTP, _ := json.Marshal(overHTTP)
	if !bytes.Equal(fromStdio, fromHTTP) {
		t.Fatalf("stdio answers differently from the endpoint:\n%s\n%s", fromStdio, fromHTTP)
	}
}

// TestMCPCanBeTurnedOff is the `TRACEPAD_MCP=off` half of the MCP contract.
func TestMCPCanBeTurnedOff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateProject("test", store.KeyPair{PublicKey: "tp-pk-test", Secret: testKey}); err != nil {
		t.Fatal(err)
	}
	writer, err := st.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	cfg := &config.Config{Listen: ":0", MaxBodyBytes: config.DefaultMaxBodyBytes, MCP: false}
	httpServer := httptest.NewServer(server.New(cfg, testVersion, st, writer).Handler())
	defer httpServer.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		httpServer.URL+mcpserver.Path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when MCP is off", response.StatusCode)
	}
}

// TestIncomingTraceContextIsLogged is #22: trace context on `_meta` reaches
// the log, and nothing more happens with it. A tracing product should not be
// the one tool that drops trace context on the floor.
func TestIncomingTraceContextIsLogged(t *testing.T) {
	h := newHarness(t)

	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	answer := h.rpc(t, "tools/call", map[string]any{
		"name":      "list_traces",
		"arguments": map[string]any{"limit": 1},
		"_meta":     map[string]any{"traceparent": traceparent},
	})
	if !bytes.Contains(answer, []byte(`"traces"`)) {
		t.Fatalf("the call did not succeed: %s", answer)
	}
	if !strings.Contains(recorded.String(), traceparent) {
		t.Fatalf("the trace context was dropped; log was:\n%s", recorded.String())
	}
}

func textOf(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}
