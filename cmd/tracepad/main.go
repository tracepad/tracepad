// Command tracepad is the Tracepad server and (in later stages) its CLI.
// Bare `tracepad` runs the server (spec 001 #1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tracepad/tracepad/internal/cli"
	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/server"
	"github.com/tracepad/tracepad/internal/store"
)

// version is stamped by the release build (-ldflags "-X main.version=...").
var version = "dev"

// splitCommand separates the subcommand from its arguments. A leading flag
// belongs to the default command, so `tracepad --listen :9999` serves
// (spec 001 #1).
func splitCommand(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "serve", args
}

// clientCommands are the subcommands served by the CLI rather than by the
// server half of the binary. They are the same binary on purpose: for an agent
// with a terminal, "the tool is already on PATH" is zero integration
// (design §3.3).
var clientCommands = map[string]bool{
	"traces":   true,
	"tail":     true,
	"sessions": true,
	"scores":   true,
	"prompts":  true,
	"stats":    true,
	"system":   true,
}

func main() {
	cmd, args := splitCommand(os.Args[1:])

	switch {
	case cmd == "serve":
		if err := serve(args); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case cmd == "mcp":
		if err := serveMCP(args); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case clientCommands[cmd]:
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		os.Exit(cli.Run(ctx, cli.Options{
			Args:    os.Args[1:],
			Version: version,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			TTY:     isTerminal(os.Stdout),
			Env:     os.Getenv,
			Now:     time.Now,
		}))
	case cmd == "version":
		fmt.Println(version)
	case cmd == "help", cmd == "-h", cmd == "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tracepad: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// isTerminal reports whether output is going to a terminal rather than into a
// pipe or a file. It is what makes agent-first the default: a pipe gets JSON
// with no flags at all (spec 004 #12).
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func usage() {
	fmt.Fprint(os.Stderr, `Tracepad — lightweight OTLP-native store and viewer for LLM traces.

Usage:
  tracepad [serve] [flags]   run the server (default command)
  tracepad mcp [flags]       serve MCP over stdio, against a running server
  tracepad version           print the version

Flags of serve:
  --listen addr      HTTP listen address        (env TRACEPAD_LISTEN, default :4318)
  --data-dir path    data directory             (env TRACEPAD_DATA_DIR)

Server environment:
  TRACEPAD_STORE_RAW              keep raw OTLP bodies for remap/export (default on)
  TRACEPAD_MAX_BODY_BYTES         request body cap in bytes             (default 20971520)
  TRACEPAD_RESPONSE_BUDGET_BYTES  default read response budget          (default 51200)
  TRACEPAD_MCP                    serve MCP at /mcp                     (default on)
  TRACEPAD_SWEEP_INTERVAL         retention sweep cadence               (default 1h)
  TRACEPAD_ADMIN_TOKEN            bearer token for cross-project admin  (default unset)

`+cli.Usage)
}

func serve(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return nil
		}
		return err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	specs, err := provisionSpecs(cfg)
	if err != nil {
		return err
	}
	boot, err := st.Bootstrap(specs)
	if err != nil {
		return err
	}
	printCreated(boot, cfg.Listen)

	// The writer outlives the HTTP server on purpose: it is closed after
	// Shutdown has drained the handlers that are still waiting on a commit.
	writer, err := st.NewWriter(store.WriterOptions{})
	if err != nil {
		return err
	}
	defer writer.Close()

	// The sweeper writes through that same writer (spec 005 #3), so it is
	// stopped before the writer is: a pass still waiting on a commit must
	// get its answer first.
	sweeper := st.NewSweeper(writer, store.SweepOptions{Interval: cfg.SweepInterval})
	sweeper.Start()
	defer sweeper.Close()

	srv := server.New(cfg, version, st, writer, sweeper)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// serveMCP runs the MCP server over stdin and stdout for clients that cannot
// speak remote HTTP (spec 004 #15). It is the same tool registry the running
// server exposes at /mcp, pointed at that server over the network instead of
// at itself in process — one implementation, two transports.
func serveMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", envOr("TRACEPAD_URL", client.DefaultURL), "server to talk to")
	key := fs.String("key", os.Getenv("TRACEPAD_API_KEY"), "project secret key")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return nil
		}
		return err
	}
	if *key == "" {
		return errors.New("no API key: set TRACEPAD_API_KEY or pass --key")
	}
	api, err := client.New(*url, *key)
	if err != nil {
		return err
	}

	// stdout is the transport; every diagnostic has to go to stderr or it
	// would be read as a JSON-RPC frame.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	slog.Info("serving MCP over stdio", "server", *url, "protocol", mcpserver.ProtocolVersion)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return mcpserver.ServeStdio(ctx, version, &mcpserver.Remote{Client: api})
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func provisionSpecs(cfg *config.Config) ([]store.ProvisionSpec, error) {
	parsed, err := config.ParseProjects(cfg.Projects)
	if err != nil {
		return nil, err
	}
	specs := make([]store.ProvisionSpec, 0, len(parsed))
	for _, p := range parsed {
		specs = append(specs, store.ProvisionSpec{Name: p.Name, PublicKey: p.PublicKey, SecretKey: p.SecretKey})
	}
	return specs, nil
}

// printCreated hands the operator ready-to-paste connection env for every
// project created in this run — both plain OTel and Langfuse-SDK style
// (spec 001 #9).
func printCreated(boot *store.BootstrapResult, listen string) {
	host := displayHost(listen)
	for _, c := range boot.Created {
		fmt.Printf(`
Project %q created. Connect your app with either:

  # OpenTelemetry SDK
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://%s/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer %s"

  # Langfuse SDK
  LANGFUSE_HOST=http://%s
  LANGFUSE_PUBLIC_KEY=%s
  LANGFUSE_SECRET_KEY=%s

`, c.Project.Name, host, c.Keys.Secret, host, c.Keys.PublicKey, c.Keys.Secret)
	}
}

// displayHost turns a listen address into a connectable host:port. Wildcard
// bind hosts (empty, 0.0.0.0, ::) are not valid connect targets, so they are
// shown as localhost.
func displayHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		return "localhost:" + port
	}
	return listen
}
