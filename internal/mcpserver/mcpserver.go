package mcpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

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
	server.AddReceivingMiddleware(cacheableToolList, logTraceContext)
	register(server, api)
	return server
}

// traceparentKey is where the 2026-07-28 spec documents incoming W3C trace
// context: on the request's `_meta`.
const traceparentKey = "traceparent"

// logTraceContext records incoming trace context, and nothing more (#22). A
// tracing product should at least not drop trace context on the floor; full
// self-instrumentation is deliberately out of scope, so the request log is
// where it lands and where it stops.
func logTraceContext(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if params := req.GetParams(); params != nil {
			if traceparent, ok := params.GetMeta()[traceparentKey].(string); ok && traceparent != "" {
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
