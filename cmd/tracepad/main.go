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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tracepad/tracepad/agent/skills"
	"github.com/tracepad/tracepad/internal/cli"
	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/server"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/ui"
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
//
// Asked of the CLI rather than listed here. Listing them here is what shipped
// `datasets`, `runs`, `score-configs` and `export` unreachable through three
// specs: the package grew a command, this copy did not, and every test calls
// cli.Run directly and so never noticed (spec 020 #14).
var clientCommands = func() map[string]bool {
	names := cli.Commands()
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}()

// serverCommands are the words this file answers itself. The CLI must not claim
// one of them: `clientCommands` is consulted first, so a collision would take
// `serve` away from the server and hand it to a client command of the same
// name. Nothing enforces this at compile time, so the test does.
var serverCommands = []string{"serve", "mcp", "skills", "version", "help"}

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
	case cmd == "skills":
		// Local: it writes the skill this binary carries and talks to no
		// server (spec 037 #6), so it sits with the binary's own words.
		os.Exit(skills.Run(skills.Options{
			Args:    args,
			Version: version,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			Env:     os.Getenv,
			Getwd:   os.Getwd,
		}))
	case clientCommands[cmd]:
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		os.Exit(cli.Run(ctx, cli.Options{
			Args:    os.Args[1:],
			Version: version,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			Stdin:   os.Stdin,
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
  tracepad skills install [--project | --dir DIR] [--force]
                             install the agent skill this binary carries
  tracepad skills show [FILE]
                             print the skill, or one of its references

Flags of serve:
  --listen addr      HTTP listen address        (env TRACEPAD_LISTEN, default :4318)
  --data-dir path    data directory             (env TRACEPAD_DATA_DIR)

Server environment:
  TRACEPAD_STORE_RAW              keep raw OTLP bodies for remap/export (default on)
  TRACEPAD_MAX_BODY_BYTES         request body cap in bytes             (default 20971520)
  TRACEPAD_RESPONSE_BUDGET_BYTES  default read response budget          (default 51200)
  TRACEPAD_READ_TIMEOUT           deadline of one read request          (default 20s)
  TRACEPAD_READ_CONCURRENCY       reads served at once                  (default 2 per CPU, at least 4)
  TRACEPAD_MCP                    serve MCP at /mcp                     (default on)
  TRACEPAD_SWEEP_INTERVAL         retention sweep cadence               (default 1h)
  TRACEPAD_ROLLUP_INTERVAL        statistics rollup cadence             (default 5m)
  TRACEPAD_ADMIN_TOKEN            bearer token for cross-project admin  (default unset)
                                  at least 32 characters: openssl rand -hex 32
  TRACEPAD_ADMIN_TOKEN_FILE       read the admin token from this file   (default unset)
  TRACEPAD_SETUP                  mint and print the setup link         (default on)

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
	if err := checkDeclaredSecrets(slog.Default(), st, specs); err != nil {
		return err
	}
	boot, err := st.Bootstrap(specs)
	if err != nil {
		return err
	}

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

	// The statistics aggregator runs beside it, on the same terms and for
	// the same reason: it writes through the one writer (spec 013 #3), so
	// it stops before the writer does.
	aggregator := st.NewAggregator(writer, store.RollupOptions{Interval: cfg.RollupInterval})
	aggregator.Start()
	defer aggregator.Close()

	srv := server.New(cfg, version, st, writer, sweeper)
	// Sized for the read slots the server took, which is where the
	// setting's default is settled (spec 043 #16).
	st.BoundPool(srv.ReadConcurrency())
	// After the server, because the server is what knows whether this
	// deployment still needs its first owner and what the link to create
	// one is (spec 028 #9).
	printStartup(os.Stdout, boot, cfg.Listen, srv.SetupURL())
	noteSetupOff(slog.Default(), cfg, srv)
	warnPlainHTTP(slog.Default(), cfg.Listen, cfg.URL, cfg.InContainer)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	// Five seconds to drain, then Shutdown's own three for the handlers it
	// interrupted, which leaves two of the ten `docker stop` allows before it
	// kills for the writer, the sweeper, the aggregator and the store to
	// close (spec 001 #16). Taken on both ways out: a Serve that failed on
	// its own leaves the connections it accepted still running handlers,
	// and the deferred closes above must not run under them.
	shutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	select {
	case err := <-errc:
		return errors.Join(err, shutdown())
	case <-ctx.Done():
		// A second signal while the stop runs kills the process, as it
		// would without the handler: the way out when a stop hangs.
		stop()
		slog.Info("shutting down")
		return shutdown()
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

// checkDeclaredSecrets holds a secret declared in TRACEPAD_PROJECTS to the
// admin token's length (spec 001 #18) — where the length can still be chosen.
//
// A declaration that creates a project is refused when it is short: that key
// does not exist yet, and a longer one in the variable is the whole fix. A
// declaration for a project that exists creates nothing — the bootstrap never
// rotates a project's keys (#9) — so refusing it would send the operator to
// lengthen a value the server then ignores, while the short key stays live
// and the applications holding the new one get 401s. There the start goes on,
// and says, when the short secret is still a live key of that project, how to
// replace it; rotating needs a running server, which a refusal would not give.
// The secret is measured, never quoted.
func checkDeclaredSecrets(log *slog.Logger, st *store.Store, specs []store.ProvisionSpec) error {
	for i, spec := range specs {
		if config.SecretLength(spec.SecretKey) >= config.MinSecretLength {
			continue
		}
		existing, err := st.ProjectByName(context.Background(), spec.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			return fmt.Errorf("TRACEPAD_PROJECTS entry %d: the secret key is %d characters; want at least %d — %s",
				i+1, config.SecretLength(spec.SecretKey), config.MinSecretLength, config.GenerateHint)
		}
		// A project on its way out is skipped by the bootstrap too, and
		// "mint a new pair in its settings" is no advice for a project
		// being deleted.
		if existing.Deleted() {
			continue
		}
		project, key, err := st.KeyBySecret(context.Background(), spec.SecretKey)
		if err != nil {
			return err
		}
		if project == nil || project.ID != existing.ID {
			continue
		}
		log.Warn("a key declared in TRACEPAD_PROJECTS is short enough to guess: mint a new pair in the project's "+
			"settings (or tracepad keys create with the admin token), move the applications onto it, revoke this "+
			"one, and declare the new secret — openssl rand -hex 32 makes a good one",
			"entry", i+1, "project", spec.Name, "public_key", key.PublicKey, "length", config.SecretLength(spec.SecretKey))
	}
	return nil
}

// printStartup hands the operator ready-to-paste connection env for every
// project created in this run — both plain OTel and Langfuse-SDK style
// (spec 001 #9) — and says where the browser interface is.
//
// A secret is printed only when this run generated it. One declared in
// TRACEPAD_PROJECTS is the operator's already, and printing it would copy it
// into every log this output is kept in — `docker logs` among them, which
// keeps it for the life of the container (spec 001 #12). The lines are
// printed with the place for it named instead.
//
// The interface's line is the setup link while this server has no owner, and
// the bare URL once it has one (spec 028 #9). The pre-authed `#key=` link of
// spec 006 #8 is gone with the key login it authenticated: a key is what a
// program holds, and the interface is where a person signs in. The idea is
// unchanged — the one moment the operator is provably at the console is the
// moment the server prints a line, so the line is the credential — and so is
// the fragment, which never reaches the server and which the app strips from
// the URL as soon as it has read it.
func printStartup(w io.Writer, boot *store.BootstrapResult, listen, setupURL string) {
	host := config.DisplayHost(listen)
	for _, c := range boot.Created {
		intro, secret := ". Connect your app with either:", c.Keys.Secret
		if c.Declared {
			intro = " from TRACEPAD_PROJECTS. Connect your app with the\nsecret key declared there, in either of:"
			secret = "<its secret key from TRACEPAD_PROJECTS>"
		}
		fmt.Fprintf(w, `
Project %q created%s

  # OpenTelemetry SDK
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://%s/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer %s"

  # Langfuse SDK
  LANGFUSE_HOST=http://%s
  LANGFUSE_PUBLIC_KEY=%s
  LANGFUSE_SECRET_KEY=%s

`, c.Project.Name, intro, host, secret, host, c.Keys.PublicKey, secret)
	}
	if !ui.Enabled {
		return
	}
	if setupURL != "" {
		fmt.Fprintf(w, `
This server has no owner yet. Create the first one — it takes an email and a
password, and nothing is written down anywhere but this database:

  %s

The link is good for 24 hours, or until this process stops. Restart to have a
new one printed.

`, setupURL)
		return
	}
	fmt.Fprintf(w, "\nWeb interface: http://%s/\n\n", host)
}

// noteSetupOff says, on a server with no owner yet and TRACEPAD_SETUP=off,
// how the first owner gets made — and warns when nothing can make one (spec
// 028 #32). A refusal to start would be wrong: the data plane works without
// an owner, and the admin token can be added by a restart.
func noteSetupOff(log *slog.Logger, cfg *config.Config, srv *server.Server) {
	if !cfg.SetupDisabled {
		return
	}
	required, err := srv.SetupRequired(context.Background())
	if err != nil || !required {
		return
	}
	if cfg.AdminToken == "" {
		log.Warn("this server has no owner, TRACEPAD_SETUP=off and no TRACEPAD_ADMIN_TOKEN: " +
			"nobody can sign in to the interface or manage accounts until one of the two is set")
		return
	}
	log.Info("this server has no owner and setup is off; create the first one with the admin token: " +
		"TRACEPAD_API_KEY=<the admin token> tracepad accounts create <email> --owner")
}

// warnPlainHTTP says so at start when other machines can reach this server
// over plain HTTP and nothing says a TLS proxy stands in front (spec 001 #12).
// A warning, not a refusal. In the image it is one INFO line instead: a
// container binds every interface by design and is fenced by where its port
// is published, which the server cannot see, and a warning that fires in the
// recommended setup teaches people to skip warnings.
func warnPlainHTTP(log *slog.Logger, listen, publicURL string, inContainer bool) {
	if !config.PlainHTTPBeyondLoopback(listen, publicURL) {
		return
	}
	if inContainer {
		log.Info("listening on all interfaces inside the container; publish the port on 127.0.0.1 "+
			"or put TLS in front — docs/docker.md", "listen", listen)
		return
	}
	log.Warn("serving plain HTTP beyond loopback: passwords, session cookies and keys cross the network unencrypted. "+
		"Put a TLS proxy in front and set TRACEPAD_URL to its https:// address, or listen on 127.0.0.1 "+
		"(in a container, publish the port with -p 127.0.0.1:4318:4318)",
		"listen", listen)
}
