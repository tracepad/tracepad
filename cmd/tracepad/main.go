// Command tracepad is the Tracepad server and (in later stages) its CLI.
// Bare `tracepad` runs the server (spec 001 #1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tracepad/tracepad/internal/config"
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

func main() {
	cmd, args := splitCommand(os.Args[1:])

	switch cmd {
	case "serve":
		if err := serve(args); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tracepad: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Tracepad — lightweight OTLP-native store and viewer for LLM traces.

Usage:
  tracepad [serve] [flags]   run the server (default command)
  tracepad version           print the version

Flags of serve:
  --listen addr      HTTP listen address        (env TRACEPAD_LISTEN, default :4318)
  --data-dir path    data directory             (env TRACEPAD_DATA_DIR)
`)
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

	srv := server.New(cfg.Listen, version, st)

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
