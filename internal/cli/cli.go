// Package cli is the tracepad command line: `traces ls`, `traces last`,
// `tail`, `stats`, `system` and the rest. It is an HTTP client of the read API
// and nothing else — it never opens the database, and it can do nothing the
// API cannot (spec 004 #1, #11). Any command that would need a workaround here
// is an API gap, found before merge rather than after.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/client"
)

// Exit codes (#12). A script has to be able to tell "no such trace" from "you
// typed the flag wrong", and these are the two it acts on differently.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Options is everything the command line resolves before a command runs.
type Options struct {
	// Args are the arguments after the program name, subcommand first.
	Args    []string
	Version string
	Stdout  io.Writer
	Stderr  io.Writer
	// Stdin is where an interactive confirmation is read from (spec 005
	// #13). Nil outside a terminal, where `--yes` is required instead.
	Stdin io.Reader
	// TTY reports whether Stdout is a terminal. It decides the output
	// mode: human-readable on a terminal, JSON everywhere else, so an
	// agent piping `tracepad traces ls` gets machine output with no flags
	// at all (#12).
	TTY bool
	// Env resolves environment variables; os.Getenv in production.
	Env func(string) string
	// Now is the clock `--since 1h` counts back from.
	Now func() time.Time
}

// run carries the resolved connection and output mode through one command.
type run struct {
	opt        Options
	url        string
	key        string
	forceJSON  bool
	api        *client.Client
	warnedSkew bool
}

// Run executes one command and returns the process exit code.
func Run(ctx context.Context, opt Options) int {
	if opt.Env == nil {
		opt.Env = func(string) string { return "" }
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	r := &run{
		opt: opt,
		url: firstNonEmpty(opt.Env("TRACEPAD_URL"), client.DefaultURL),
		key: opt.Env("TRACEPAD_API_KEY"),
	}

	command, rest := split(opt.Args)
	var err error
	switch command {
	case "traces":
		err = r.traces(ctx, rest)
	case "tail":
		err = r.tail(ctx, rest)
	case "sessions":
		err = r.sessions(ctx, rest)
	case "scores":
		err = r.scores(ctx, rest)
	case "prompts":
		err = r.prompts(ctx, rest)
	case "stats":
		err = r.stats(ctx, rest)
	case "system":
		err = r.system(ctx, rest)
	case "projects":
		err = r.projects(ctx, rest)
	case "keys":
		err = r.keys(ctx, rest)
	case "retention":
		err = r.retention(ctx, rest)
	case "users":
		err = r.users(ctx, rest)
	default:
		return r.fail(usageErrorf("unknown command %q", command))
	}
	switch {
	case errors.Is(err, errHelp):
		fmt.Fprint(opt.Stdout, Usage)
	case err != nil:
		return r.fail(err)
	}
	return ExitOK
}

// Usage is the client half of the binary's help text.
const Usage = `Client commands (they talk to a running server over HTTP):
  tracepad traces ls    [--env E] [--error] [--since 1h] [--user U] [--session S] [--name N]
                        [--tag T] [--min-cost C] [--fields a,b] [--limit N]
  tracepad traces show  <trace-id> [--full]
  tracepad traces last  [--error] [--env E] [--since 1h] [--full]
  tracepad tail         [--env E] [--error] [--interval 2s] [--limit N]
  tracepad sessions show <session-id> [--limit N]
  tracepad scores ls    [--trace ID] [--name N] [--session S] [--since 1h] [--limit N]
  tracepad prompts ls
  tracepad prompts get  <name> [--label L | --version N]
  tracepad prompts push <name> --file prompt.json [--label L] [--message M]
  tracepad prompts diff <name> --from N --to M
  tracepad stats        [--group-by hour|day|model|environment] [--since 1h] [--env E]
  tracepad system

Administration (spec 005). Every destructive command shows what it would do
and asks you to type the name back; --yes answers that for a script:
  tracepad projects ls   [--deleted]
  tracepad projects show [<project-id>]
  tracepad projects create  <name>                     (admin token)
  tracepad projects rename  <project-id> <new-name>    (admin token)
  tracepad projects rm      <project-id> [--yes]       (admin token)
  tracepad projects restore <project-id>
  tracepad keys ls      [--project ID]
  tracepad keys create  [--project ID]
  tracepad keys rm      <public-key> [--project ID] [--yes]
  tracepad retention show [--project ID]
  tracepad retention set  [--days N | --forever] [--raw-days N | --raw-follow]
                          [--project ID] [--yes]
  tracepad users rm-data  <user-id> [--project ID] [--yes]

Connection:
  --url URL    server to talk to   (env TRACEPAD_URL, default http://localhost:4318)
  --key KEY    project secret key, or TRACEPAD_ADMIN_TOKEN for the commands
               marked (admin token)   (env TRACEPAD_API_KEY)

Output:
  --json       force JSON. Without it, output is a table on a terminal and
               JSON everywhere else, so a pipe is machine-readable by default.

Exit codes: 0 ok, 1 request or server error, 2 usage error.
`

// usageError is a mistake in how the command was typed, which exits 2 rather
// than 1: a script has to tell a typo from a server saying no.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usageErrorf(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// fail renders an error and returns the exit code it maps to.
func (r *run) fail(err error) int {
	fmt.Fprintf(r.opt.Stderr, "tracepad: %s\n", err)
	var usage *usageError
	if errors.As(err, &usage) {
		fmt.Fprint(r.opt.Stderr, "\n", Usage)
		return ExitUsage
	}
	return ExitFailure
}

// flags builds a flag set carrying the global flags every command accepts, so
// that `--json` and `--url` work wherever a user thinks to put them.
func (r *run) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// The caller owns user-facing usage output.
	fs.SetOutput(io.Discard)
	fs.StringVar(&r.url, "url", r.url, "")
	fs.StringVar(&r.key, "key", r.key, "")
	fs.BoolVar(&r.forceJSON, "json", r.forceJSON, "")
	return fs
}

// errHelp is `--help` on a subcommand: not a mistake, so not an error exit.
var errHelp = errors.New("help requested")

// anyArgs lets a command count its own positionals, which is only needed where
// one is optional — `projects show` with no id means "the one this key
// reaches".
const anyArgs = -1

// parse reads a command's flags and then builds the connection, which cannot
// happen earlier: `--url` is one of the flags.
func (r *run) parse(fs *flag.FlagSet, args []string, wantArgs int) ([]string, error) {
	if err := fs.Parse(permute(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, usageErrorf("%s: %s", fs.Name(), err)
	}
	if wantArgs >= 0 && fs.NArg() != wantArgs {
		if wantArgs == 0 {
			return nil, usageErrorf("%s takes no arguments, got %q", fs.Name(), fs.Arg(0))
		}
		return nil, usageErrorf("%s needs %d argument(s), got %d", fs.Name(), wantArgs, fs.NArg())
	}
	api, err := client.New(r.url, r.key)
	if err != nil {
		return nil, usageErrorf("%s", err)
	}
	if r.key == "" {
		return nil, usageErrorf("no API key: set TRACEPAD_API_KEY or pass --key")
	}
	api.OnVersion = r.noteServerVersion
	r.api = api
	return fs.Args(), nil
}

// noteServerVersion warns once when the binary and the server are different
// builds. It is a warning and not an error: a skew is usually fine, and a CLI
// that refused to run would be worse than one that says what it noticed
// (edge cases).
func (r *run) noteServerVersion(version string) {
	if r.warnedSkew || version == "" || version == r.opt.Version {
		return
	}
	r.warnedSkew = true
	fmt.Fprintf(r.opt.Stderr,
		"tracepad: warning: this is tracepad %s talking to a server running %s\n",
		r.opt.Version, version)
}

// wantJSON reports whether this invocation prints the API's bytes rather than
// a table.
func (r *run) wantJSON() bool { return r.forceJSON || !r.opt.TTY }

// emit prints a response body verbatim. Verbatim matters: the same question
// asked through the CLI, through an MCP tool and with curl answers with the
// same bytes (#1).
func (r *run) emit(body json.RawMessage) error {
	_, err := fmt.Fprintf(r.opt.Stdout, "%s\n", strings.TrimRight(string(body), "\n"))
	return err
}

// decode reads a response into a value the human renderer understands.
func decode[T any](body json.RawMessage) (T, error) {
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("the server answered with something unexpected: %w", err)
	}
	return out, nil
}

// permute moves flags ahead of positional arguments. Go's flag package stops
// parsing at the first non-flag word, which would make `traces show <id>
// --full` — the form the CLI contract itself is written in — silently ignore
// `--full`. Reordering is the smallest fix that keeps the standard parser.
//
// Whether a flag consumes the next word is asked of the flag set rather than
// guessed, so `--limit 5` keeps its value and `--full` does not steal the id.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after it is positional by convention.
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		definition := fs.Lookup(name)
		if definition == nil {
			// Unknown: leave it to the parser, which has the error
			// message for it.
			continue
		}
		if boolean, ok := definition.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// split separates the subcommand from its arguments.
func split(args []string) (string, []string) {
	if len(args) == 0 {
		return "", nil
	}
	return args[0], args[1:]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// since turns `--since` into the `from` filter. Both spellings are accepted
// because both are natural: a duration is what a human types at a terminal,
// and an instant is what a script computed.
func (r *run) since(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return "", usageErrorf("--since must be a duration in the past, got %q", value)
		}
		return r.opt.Now().Add(-duration).UTC().Format(time.RFC3339), nil
	}
	if instant, err := time.Parse(time.RFC3339, value); err == nil {
		return instant.UTC().Format(time.RFC3339), nil
	}
	return "", usageErrorf("--since takes a duration (1h, 30m) or an RFC 3339 timestamp, got %q", value)
}

// addSome sets a query parameter only when there is one, so an unset flag is
// never sent as an empty value the server would refuse (spec 003 #23).
func addSome(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

func addLimit(query url.Values, limit int) error {
	if limit == 0 {
		return nil
	}
	if limit < 1 || limit > 500 {
		return usageErrorf("--limit must be between 1 and 500, got %d", limit)
	}
	query.Set("limit", strconv.Itoa(limit))
	return nil
}
