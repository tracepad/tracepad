package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport and protocol (#14, #15). The 2026-07-28 revision made the protocol
// stateless — no handshake, no `Mcp-Session-Id`, version and capabilities ride
// in `_meta` per request — and these tools are stateless read wrappers, so the
// new core fits exactly: any load balancer works and there is no session
// bookkeeping to get wrong. Older clients fall back to the stateful
// 2025-11-25 through the SDK's own negotiation, so the compatibility window is
// the SDK's problem rather than ours.
//
// Deprecated features are not adopted (#19): no roots, no sampling, no logging
// capability, no tasks extension, no elicitation. Roots, sampling and logging
// are formally deprecated with a twelve-month removal window, and building on
// a condemned floor is not a saving. Tasks exist for long-running work; every
// tool here answers in milliseconds.

// Path is where the MCP endpoint lives on the main listener.
const Path = "/mcp"

// ProtocolVersion is the revision this server targets. The SDK negotiates
// older ones for clients that ask.
const ProtocolVersion = "2026-07-28"

// toolListTTLMs is how long a client may cache `tools/list`. The tool set
// changes only when this binary does, and a stable, cacheable list is what
// keeps a model's prompt prefix stable across turns (#18).
const toolListTTLMs = 3600000

// toolListCacheScope is `private` rather than `public`: this is an
// authenticated, per-project surface, and calling it public would invite a
// shared cache to serve one project's answer to another.
const toolListCacheScope = "private"

// New builds the MCP server over one API caller.
func New(version string, api API) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tracepad",
		Title:   "Tracepad",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Tracepad stores LLM and agent traces. Start from list_traces or " +
			"get_last_trace to find a run, then get_trace to see what happened inside it. " +
			"Every tool reads; none of them change anything.",
	})
	// recoverPanics comes first so that it wraps everything after it: the
	// other middleware, the SDK's method dispatch and every tool handler.
	server.AddReceivingMiddleware(recoverPanics(&panicLog{now: time.Now}), cacheableToolList, logTraceContext)
	register(server, api)
	return server
}

// recoverPanics turns a panic in the receiving handler chain — the
// middleware after it, the SDK's method dispatch and every tool handler —
// into a JSON-RPC internal error for that one request. The SDK runs each
// request in a goroutine of its own, which net/http's per-connection recovery
// does not cover, so without this a single bad request takes down the whole
// process: ingest, the UI and every other client with it.
//
// The SDK's own work before the chain (checking the request, decoding its
// params) and after it (shaping and encoding the result) runs in that same
// goroutine and is not covered: the SDK calls our code here and nowhere else.
//
// What reaches the log is decided by panicLog.
func recoverPanics(log *panicLog) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.record(method, toolName(req), recovered, panicSite())
					result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
				}
			}()
			return next(ctx, method, req)
		}
	}
}

// panicSite is the function and line a panic was raised at: the first frame
// below runtime.gopanic that is not the runtime's own, which for a nil
// dereference or a bad index is the code that made it. It must be called
// from the deferred function that recovered, while the panicking frames are
// still on the stack. The function name, not the file, so the build path
// stays out of the log.
func panicSite() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for panicking := false; ; {
		frame, more := frames.Next()
		if frame.Function == "runtime.gopanic" {
			panicking = true
		} else if panicking && !strings.HasPrefix(frame.Function, "runtime.") {
			return fmt.Sprintf("%s:%d", frame.Function, frame.Line)
		}
		if !more {
			return "unknown"
		}
	}
}

// toolNameFormat is the shape of every tool name this server registers.
var toolNameFormat = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// toolName is the tool a `tools/call` names, when the name has the shape of
// one of ours, so it can tell apart two tools' bugs that panic at the same
// shared site. A panic in a tool handler means the SDK found the tool, so
// the name is a registered one; the shape check bounds what could reach the
// log from a panic before that lookup.
func toolName(req mcp.Request) string {
	if req == nil {
		return ""
	}
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil || !toolNameFormat.MatchString(params.Name) {
		return ""
	}
	return params.Name
}

// runtimeMessage is the message of a panic the Go runtime raised, or "" for
// any other value. It calls Error() on a value the panicking code chose, so
// that call gets a recover of its own: a panic here would be past the one
// that recovered the first.
func runtimeMessage(recovered any) (message string) {
	runtimeErr, ok := recovered.(runtime.Error)
	if !ok {
		return ""
	}
	defer func() {
		if recover() != nil {
			message = ""
		}
	}()
	return runtimeErr.Error()
}

const (
	// panicSummaryInterval is how often, at most, one kind of panic gets a
	// line after its first.
	panicSummaryInterval = time.Minute
	// panicKindsLimit bounds how many kinds of panic are tracked apart.
	panicKindsLimit = 64
)

// panicLog writes recovered panics to the log without letting a caller who
// can trigger one grow the log without bound. A panic reachable without a
// credential can be sent in a loop, and the full stack is kilobytes, so:
//
//   - A kind of panic is its type, the site it was raised at and the tool
//     it was raised in, if any. The first of a kind is logged in full, with
//     its stack: that is what a fix needs, and it needs it once.
//   - Repeats are counted, and at most once a minute a kind gets a one-line
//     summary with the count since its last line. A count still pending
//     when the panics stop is reported with the next one.
//   - At most panicKindsLimit kinds are tracked. Past that, new kinds are
//     counted together, summarised the same way under a line that names no
//     method or type, because the panics it counts have different ones.
//
// The panic's value is logged only when it is a runtime.Error: the Go runtime
// writes that message, and the most of the request it can carry is a number
// derived from it — an index, a length — never its bytes. Any other value
// may be a message built from an argument, which would carry the request,
// arbitrary input from a caller nobody has authenticated yet, into the log;
// for those the type stands in.
//
// One panicLog belongs to one server, and a process serves one.
type panicLog struct {
	now func() time.Time

	mu       sync.Mutex
	kinds    map[panicKind]*panicTally
	overflow panicTally
}

type panicKind struct{ panicType, site, tool string }

type panicTally struct {
	pending int64     // occurrences since the last line
	logged  time.Time // when the last line was written; zero for never
}

func (l *panicLog) record(method, tool string, recovered any, site string) {
	kind := panicKind{panicType: fmt.Sprintf("%T", recovered), site: site, tool: tool}
	attrs := []any{"method", method}
	if tool != "" {
		attrs = append(attrs, "tool", tool)
	}
	attrs = append(attrs, "panic_type", kind.panicType, "site", site)
	if message := runtimeMessage(recovered); message != "" {
		attrs = append(attrs, "panic", message)
	}
	now := l.now()

	l.mu.Lock()
	if l.kinds == nil {
		l.kinds = map[panicKind]*panicTally{}
	}
	tally, known := l.kinds[kind]
	if !known && len(l.kinds) < panicKindsLimit {
		l.kinds[kind] = &panicTally{logged: now}
		l.mu.Unlock()
		slog.Error("mcp handler panicked", append(attrs, "stack", string(debug.Stack()))...)
		return
	}
	if !known {
		tally = &l.overflow
	}
	tally.pending++
	if !tally.logged.IsZero() && now.Sub(tally.logged) < panicSummaryInterval {
		l.mu.Unlock()
		return
	}
	count, since := tally.pending, tally.logged
	tally.pending, tally.logged = 0, now
	l.mu.Unlock()

	if tally == &l.overflow {
		slog.Error("mcp handler panics of untracked kinds", "occurrences", count,
			"note", "more kinds of panic than are tracked apart; these are counted together")
		return
	}
	slog.Error("mcp handler panic repeated",
		append(attrs, "occurrences", count, "since", since.UTC().Format(time.RFC3339))...)
}

// requestMeta is the request's `_meta`, or nil when it has none. A request
// that omits the optional `params` member arrives with a typed nil pointer
// inside a non-nil Params interface, so comparing the interface with nil is
// not enough, and calling GetMeta on it dereferences nil. Reflection is the
// check because it covers every params type the SDK has or will add; the
// SDK's own isNil is unexported, and a type switch over today's types would
// silently miss tomorrow's.
func requestMeta(req mcp.Request) map[string]any {
	if req == nil {
		return nil
	}
	params := req.GetParams()
	if params == nil {
		return nil
	}
	if v := reflect.ValueOf(params); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	return params.GetMeta()
}

// traceparentKey is where the 2026-07-28 spec documents incoming W3C trace
// context: on the request's `_meta`.
const traceparentKey = "traceparent"

// traceparentLen is the length of a version 00 traceparent, and of the part
// of a later version that version 00 defines.
const traceparentLen = 55

// traceparentFields is the version 00 layout: version, trace id, parent id
// and flags, lowercase hex separated by dashes.
var traceparentFields = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// traceparentOf returns the loggable part of a traceparent, and whether it is
// well-formed W3C trace context at all (Trace Context §3.2.4). Version `ff`
// is invalid. Version 00 is exactly 55 characters. A later version may be
// longer, and a parser reads its first 55 characters as version 00 when what
// follows them is a dash or nothing; only those 55 are returned. All-zero
// trace and parent ids are invalid in every version.
func traceparentOf(value string) (string, bool) {
	if len(value) < traceparentLen {
		return "", false
	}
	head := value[:traceparentLen]
	switch version := head[:2]; {
	case version == "ff":
		return "", false
	case version == "00" && len(value) != traceparentLen:
		return "", false
	case len(value) > traceparentLen && value[traceparentLen] != '-':
		return "", false
	}
	if !traceparentFields.MatchString(head) ||
		head[3:35] == "00000000000000000000000000000000" ||
		head[36:52] == "0000000000000000" {
		return "", false
	}
	return head, true
}

// logTraceContext records incoming trace context, and nothing more (#22). A
// tracing product should at least not drop trace context on the floor; full
// self-instrumentation is deliberately out of scope, so the request log is
// where it lands and where it stops.
//
// This runs before any credential is checked, so only well-formed trace
// context is logged: anything else is dropped rather than written, and an
// unauthenticated caller cannot put arbitrary bytes into the log through it.
func logTraceContext(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if value, ok := requestMeta(req)[traceparentKey].(string); ok {
			if traceparent, valid := traceparentOf(value); valid {
				slog.Info("mcp request", "method", method, traceparentKey, traceparent)
			}
		}
		return next(ctx, method, req)
	}
}

// cacheableToolList stamps the caching hints of #18 onto `tools/list`. The SDK
// fills in a `public` default, which would be a lie about an authenticated
// surface, and no TTL at all, which throws away the one list that never
// changes between deploys.
func cacheableToolList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if list, ok := result.(*mcp.ListToolsResult); ok && err == nil {
			list.TTLMs = toolListTTLMs
			list.CacheScope = toolListCacheScope
		}
		return result, err
	}
}

// HTTPHandler serves MCP over streamable HTTP, stateless, for mounting at
// Path on the main listener. Authentication is the server's own
// `Bearer tp-sk-…`/Basic, applied by the read API the tools call: there is no
// second credential story and no OAuth, because a self-hosted server with
// pre-shared project keys is not the multi-tenant public deployment the OAuth
// framework in the MCP spec targets (#20).
func HTTPHandler(version string, api API) http.Handler {
	server := New(version, api)
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
}

// ServeStdio runs the same tool registry over stdin and stdout, for clients
// that cannot speak remote HTTP (#15). It is one implementation with two
// transports, not two implementations.
//
// It returns nil when the client closes the pipe: that is how an stdio session
// ends, not how it fails, and exiting non-zero would make every normal
// disconnect look like a crash in the host's log.
func ServeStdio(ctx context.Context, version string, api API) error {
	if err := New(version, api).Run(ctx, &mcp.StdioTransport{}); err != nil && !disconnected(err) {
		return err
	}
	return nil
}

// codeServerClosing is the JSON-RPC code the SDK reports when the connection
// is shutting down. The SDK keeps the sentinel itself in an internal package
// and formats the underlying cause with %v rather than %w, so the code is the
// only thing left to match on.
const codeServerClosing = -32004

func disconnected(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var wire *jsonrpc.Error
	return errors.As(err, &wire) && wire.Code == codeServerClosing
}
