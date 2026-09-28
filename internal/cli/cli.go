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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/termsafe"
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
	// retryBackoff is the first delay `export --otlp` waits before asking a
	// receiver again (spec 019 #6); it doubles from there. Unexported and
	// zero in production, where the constant applies: it exists so the
	// retry tests exercise the real loop without spending half a minute in
	// it, and so it belongs to one run rather than to the process.
	retryBackoff time.Duration
	// observeFlags, when set, is handed every flag set a command builds.
	// Unexported because it is for the parity test in this package, which
	// reads what a command registers rather than listing it again by hand
	// (spec 004 #9) — and because it belongs to one run rather than to the
	// process, which a package-level hook would not.
	observeFlags func(*flag.FlagSet)
}

// run carries the resolved connection and output mode through one command.
type run struct {
	// opt's Stdout and Stderr are escaping writers wherever a person may be
	// reading them (spec 004 #35). jsonOut is stdout unwrapped, and only emit
	// writes to it: the JSON mode's bytes are the API's (#1).
	opt        Options
	jsonOut    io.Writer
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
	r := newRun(opt)

	command, rest := split(opt.Args)
	handler, known := r.handlers()[command]
	if !known {
		return r.fail(usageErrorf("unknown command %q", command))
	}
	err := handler(ctx, rest)
	switch {
	case errors.Is(err, errHelp):
		fmt.Fprint(opt.Stdout, Usage)
	case err != nil:
		return r.fail(err)
	}
	return ExitOK
}

// newRun is one command's state, with the writers a person reads made safe.
func newRun(opt Options) *run {
	r := &run{
		opt:     opt,
		jsonOut: opt.Stdout,
		url:     firstNonEmpty(opt.Env("TRACEPAD_URL"), client.DefaultURL),
		key:     opt.Env("TRACEPAD_API_KEY"),
	}
	// The backstop under every renderer: whatever reaches a terminal
	// through these goes out escaped, a value a renderer printed raw
	// included. Stdout only on a terminal, where it is the human mode or
	// `--json`, which writes to r.jsonOut; stderr always, since it is a
	// terminal even when stdout is a pipe.
	if opt.TTY {
		r.opt.Stdout = termsafe.NewWriter(opt.Stdout, faintOn, faintOff)
	}
	r.opt.Stderr = termsafe.NewWriter(opt.Stderr)
	return r
}

// handlers is every subcommand this package dispatches, in one table.
//
// A table rather than a switch because there are two readers of it: this
// package, which runs the command, and `cmd/tracepad`, which has to decide
// whether a word on the command line belongs to the CLI at all. That second
// list used to be written out by hand, and it drifted — `datasets`, `runs`,
// `score-configs` and `export` shipped for three specs answering "unknown
// command" from the real binary, because the tests call Run directly and never
// go through the binary's routing (spec 020 #14). One table, read by both, is
// the shape in which that cannot happen again.
func (r *run) handlers() map[string]func(context.Context, []string) error {
	return map[string]func(context.Context, []string) error{
		"traces":        r.traces,
		"tail":          r.tail,
		"sessions":      r.sessions,
		"scores":        r.scores,
		"prompts":       r.prompts,
		"datasets":      r.datasets,
		"runs":          r.runs,
		"score-configs": r.scoreConfigs,
		"queues":        r.queues,
		"stats":         r.stats,
		"facets":        r.facets,
		"system":        r.system,
		"health":        r.health,
		"export":        r.export,
		"projects":      r.projects,
		"keys":          r.keys,
		"retention":     r.retention,
		"users":         r.users,
		"accounts":      r.accounts,
	}
}

// Commands is every subcommand the CLI serves, sorted. `cmd/tracepad` routes on
// it, so the binary dispatches exactly what this package implements.
func Commands() []string {
	handlers := (&run{}).handlers()
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Usage is the client half of the binary's help text.
const Usage = `Client commands (they talk to a running server over HTTP):
  tracepad traces ls    [--search "text"] [--env E] [--error] [--since 1h] [--until T]
                        [--user U] [--session S] [--name N] [--tag T] [--min-cost C]
                        [--release R] [--version V] [--type T] [--prompt N[@V]]
                        [--fields a,b] [--limit N] [--cursor C] [--oldest] [--newer] [--total]
  tracepad traces show  <trace-id> [--full]
  tracepad traces last  [--search "text"] [--env E] [--error] [--since 1h] [--until T]
                        [--user U] [--session S] [--name N] [--tag T] [--min-cost C]
                        [--release R] [--version V] [--type T] [--prompt N[@V]] [--full]
  tracepad traces rm    <trace-id> [--yes]
  tracepad traces rm    --to T [--search "text"] [--env E] [--error] [--since 1h]
                        [--user U] [--session S] [--name N] [--tag T] [--min-cost C]
                        [--release R] [--version V] [--type T] [--prompt N[@V]]
                        [--limit N] [--yes]
  tracepad tail         [--env E] [--error] [--since 1h] [--user U] [--session S]
                        [--name N] [--tag T] [--min-cost C] [--release R] [--version V]
                        [--type T] [--prompt N[@V]] [--interval 2s] [--limit N]
  tracepad sessions ls   [--since 1h] [--until T] [--env E] [--user U] [--limit N]
                         [--cursor C] [--oldest] [--newer] [--total]
  tracepad sessions show <session-id> [--limit N]
  tracepad users ls      [--sort last_seen|traces|cost|errors] [--prefix P]
                         [--limit N] [--cursor C] [--oldest] [--newer] [--total]
  tracepad users show    <user-id>
  tracepad scores ls    [--trace ID] [--observation ID] [--session S] [--name N]
                        [--type numeric|boolean|categorical|text] [--since 1h]
                        [--limit N] [--cursor C]
  tracepad scores add   (--trace ID [--observation ID] | --session S) --name N
                        (--value V | --string S)
                        [--type numeric|boolean|categorical|text] [--comment C]
                        [--id ID]
  tracepad scores rm    <score-id>
  tracepad scores trend [--name N] [--group-by hour|day|environment|release|model]
                        [--since 168h] [--until T] [--env E] [--limit N]
  tracepad prompts ls   [--limit N] [--cursor C]
  tracepad prompts get  <name> [--label L | --version N]
  tracepad prompts push <name> --file prompt.json [--label L] [--message M]
                        [--expect N]
  tracepad prompts diff <name> --from N --to M
  tracepad prompts label <name> <label> (--version N | --rm)
  tracepad prompts rm   <name> [--yes]
  tracepad stats        [--group-by hour|day|model|environment|release|total]
                        [--since 1h] [--until T] [--env E] [--user U]
  tracepad facets       [--since 1h] [--until T]
  tracepad system

--env, --release and --name take a comma-separated list — --env production,staging
keeps traces from either — and facets is what lists the values with their counts.

Taking the data out (spec 019). The archive is every export body as it arrived,
and this replays it into any OTLP receiver — this server included — or writes
it to disk beside a manifest:
  tracepad export --otlp (--to <url> | --dir <path>)
                        [--header k=v]... [--gzip] [--since 1h] [--until T]
                        [--after <cursor>] [--dry-run] [--allow-tracepad-key]

--to posts each body under the content type it was received in, retrying a
receiver that answers 429 or 5xx and stopping on anything else with the cursor
to resume from; --dir writes <received_at_ms>-<id>.pb (or .json) plus
manifest.jsonl, so ls is in replay order. --dry-run prints what would be sent
and sends nothing. The summary ends with how many traces started before the
archive begins, which are the ones no export can carry. The receiver's
credentials go in --header; OTEL_EXPORTER_OTLP_HEADERS is not read. A Tracepad
key bound for the receiver, in a header or in --to, is refused: the ones this
machine holds for your Tracepad (--key, TRACEPAD_API_KEY, TRACEPAD_ADMIN_TOKEN
or its _FILE, LANGFUSE_SECRET_KEY, OTEL_EXPORTER_OTLP_HEADERS) always, any other tp-sk-…
unless --allow-tracepad-key says the receiver is a Tracepad server of yours.

Evals (spec 014). The loop is: declare the configs, push the cases, open the
run, stamp each trace with tracepad.run_id and tracepad.item_id, post the
scores, finish, compare:
  tracepad datasets ls        [--limit N] [--cursor C]
  tracepad datasets show      <name> [--version N] [--limit N] [--cursor C]
  tracepad datasets push      <name> --file cases.jsonl [--description D]
  tracepad datasets rm-item   <name> <item-id>
  tracepad datasets rm        <name> [--yes]
  tracepad runs ls            [dataset] [--limit N] [--cursor C]
  tracepad runs create        <dataset> [--name N] [--id ID] [--dataset-version N]
                              [--metadata-file run.json]
  tracepad runs show          <id> [--items [--unknown] [--limit N] [--cursor C]]
  tracepad runs finish        <id> [--failed "reason"]
  tracepad runs compare       <a> <b> [--all]
  tracepad runs rm            <id>
  tracepad score-configs ls
  tracepad score-configs show <name>
  tracepad score-configs push <name> --file cfg.json
  tracepad score-configs rm   <name>

Review programmes (spec 024). A queue is a named list of traces or observations
and the score names a reviewer must set on each of them; the loop is: declare
the queue, fill it, take the next item, post the scores, complete:
  tracepad queues ls
  tracepad queues put      <name> --config N [--config M]... [--description D]
  tracepad queues rm       <name> [--yes]
  tracepad queues add      <name> (--trace ID [--observation ID] | --from-traces
                                   [--search "text"] [--env E] [--error]
                                   [--since 1h] [--until T] [--user U]
                                   [--session S] [--name N] [--tag T]
                                   [--min-cost C] [--release R] [--version V]
                                   [--type T] [--prompt N[@V]] [--limit N])
  tracepad queues items    <name> [--status pending|completed|skipped]
                                  [--annotator A] [--limit N] [--cursor C]
                                  [--newer] [--total]
  tracepad queues next     <name> --annotator A
  tracepad queues complete <name> <item-id> --annotator A
  tracepad queues skip     <name> <item-id> --annotator A [--reason R]
  tracepad queues reopen   <name> <item-id> --annotator A

queues add --from-traces takes the same filters as traces ls and adds the
newest matches, at most --limit (100 by default, 1000 at most); it says how
many matched and whether the cap bit. queues next claims the item it hands out
for ten minutes, and hands the same one back on a second call from the same
annotator. queues complete is refused until every score the queue names is on
the item's target, whoever wrote it — post them with scores add first. The
items listing reads oldest first, which is the order they are worked in.

datasets push takes a .jsonl (one case per line) or a .json array and sends it
as one batch, which is one version tick, or, past the 10,000 cases one request
takes, as one batch per 10,000, each a tick of its own if it changes anything;
it prints the version it landed on and how many cases changed. datasets show --json walks every page, so it is the
dataset's export. runs create --json answers with the whole run, so a script
reads both the id and the version it pinned. runs ls without a dataset lists
the whole project's runs, newest first. runs compare lists the items whose
verdict is not "same"; --all lists them all.

Administration (spec 005). Every destructive command shows what it would do
and asks you to type the name back; --yes answers that for a script:
  tracepad projects ls   [--deleted]
  tracepad projects show [<project-id> | --project ID]
  tracepad projects create  <name>                     (admin token)
  tracepad projects rename  <project-id> <new-name>    (admin token)
  tracepad projects rm      <project-id> [--yes]       (admin token)
  tracepad projects restore <project-id>
  tracepad keys ls      [--project ID]                 (admin token)
  tracepad keys create  --scope ingest[,read,write] [--name NAME] [--project ID]
                                                       (admin token)
  tracepad keys rm      <public-key> [--project ID] [--yes]
                                                       (admin token)
  tracepad retention show [--project ID]
  tracepad retention set  [--days N | --forever] [--raw-days N | --raw-follow]
                          [--stats-days N | --stats-forever]
                          [--media store|placeholder] [--project ID] [--yes]
  tracepad users rm-data  <user-id> [--project ID] [--yes] [--no-wait]
  tracepad users erasure  <erasure-id> [--project ID]
  tracepad users erasures [--project ID]

No project key lists, mints or revokes keys (spec 045): the keys commands take
the admin token, and an owner or editor can do the same in the web interface,
under Settings, Project, API keys. keys ls says who minted each key and when it
was last used, to within a minute.

Accounts (spec 028). People sign in; programs use keys. These need the admin
token, and keeping it somewhere is how you get back in when every owner's
password is lost — set it, run accounts invite, open the link:
  tracepad accounts ls
  tracepad accounts show   <id|email>
  tracepad accounts create <email> [--name N] [--owner]
                           [--project <project-id>:viewer|editor]...
  tracepad accounts invite <id|email>
  tracepad accounts set    <id|email> [--name N] [--owner | --no-owner]
                           [--disable | --enable]
  tracepad accounts grant  <id|email> <project-id> viewer|editor
  tracepad accounts revoke <id|email> <project-id>
  tracepad accounts rm     <id|email> [--confirm <email>]

An owner has every project; everyone else has a role in the ones they are
given. accounts create prints the invitation link once — carry it to the
person, and the link is what sets their password. accounts invite mints a
fresh one, which is also the password reset: it ends that account's sessions,
and the old password works until the new link is used. accounts rm shows what
it would delete and asks for the email back; --confirm answers that for a
script, and it is the email rather than a --yes because naming the account is
the point.

Liveness (spec 020). The one command that needs no key, because the route it
calls needs none. It prints the server's version and exits 0, or says what went
wrong on stderr and exits 1 — which is what a container's HEALTHCHECK, a
systemd unit or a load balancer reads:
  tracepad health

Connection:
  --url URL    server to talk to   (env TRACEPAD_URL, default http://localhost:4318)
  --key KEY    project secret key, or TRACEPAD_ADMIN_TOKEN for the commands
               marked (admin token)   (env TRACEPAD_API_KEY)

What the wire carried (spec 012): --type keeps traces containing one kind of
step — span, generation, event, agent, tool, chain, retriever, guardrail,
evaluator or embedding — and --prompt keeps the traces that ran a prompt, at
any version or at name@7. A version is a number, so an @ inside a name is part
of it. The listing's ttft column is the wait before the first token of the
trace's earliest completion.

Search (spec 011): --search takes words (all must occur), "quoted phrases" and
prefix*. Words, not substrings: error does not find errors, err* finds both.
Each matching row gains a second line saying which observation and field it
matched and what the text says around the hit.

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
	// An error can quote the server, and the server can quote a trace.
	// Its lines after the first are indented under the label, so a message
	// cannot open a line of its own that reads like the CLI's (#35).
	fmt.Fprintf(r.opt.Stderr, "tracepad: %s\n", block(err.Error(), "          "))
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
	if r.opt.observeFlags != nil {
		r.opt.observeFlags(fs)
	}
	fs.StringVar(&r.url, "url", r.url, "")
	fs.StringVar(&r.key, "key", r.key, "")
	fs.BoolVar(&r.forceJSON, "json", r.forceJSON, "")
	return fs
}

// errHelp is `--help` on a subcommand: not a mistake, so not an error exit.
var errHelp = errors.New("help requested")

// wasGiven reports whether a flag was passed at all, as opposed to sitting at
// its zero value. The two are different questions and an empty string cannot
// tell them apart: `--project ""` is what an unset shell variable expands to,
// and reading it as "not passed" answers a wider question than was asked —
// which is the reinterpretation spec 003 #23 refuses.
func wasGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// anyArgs lets a command count its own positionals, which is only needed where
// one is optional — `projects show` with no id means "the one this key
// reaches".
const anyArgs = -1

// parse reads a command's flags and then builds the connection, which cannot
// happen earlier: `--url` is one of the flags.
func (r *run) parse(fs *flag.FlagSet, args []string, wantArgs int) ([]string, error) {
	rest, err := r.parseUnauthenticated(fs, args, wantArgs)
	if err != nil {
		return nil, err
	}
	if r.key == "" {
		return nil, usageErrorf("no API key: set TRACEPAD_API_KEY or pass --key")
	}
	return rest, nil
}

// parseUnauthenticated is the same without the key requirement, for the one
// command that calls the one route that has none: `health` is what a container
// probe, a systemd unit or a load balancer runs, and asking those for a
// project secret to learn whether the process is up would be a key pasted into
// three more places for nothing (spec 020 #4).
func (r *run) parseUnauthenticated(fs *flag.FlagSet, args []string, wantArgs int) ([]string, error) {
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
		r.opt.Version, termsafe.String(version))
}

// wantJSON reports whether this invocation prints the API's bytes rather than
// a table.
func (r *run) wantJSON() bool { return r.forceJSON || !r.opt.TTY }

// emit prints a response body verbatim. Verbatim matters: the same question
// asked through the CLI, through an MCP tool and with curl answers with the
// same bytes (#1).
func (r *run) emit(body json.RawMessage) error {
	// A `204` answered with nothing, and verbatim means nothing: a blank
	// line is not JSON, and the whole reason a piped run is JSON is that
	// something is going to parse it (#12). The account routes are the first
	// that answer that way (spec 028 #12).
	if strings.TrimSpace(string(body)) == "" {
		return nil
	}
	_, err := fmt.Fprintf(r.jsonOut, "%s\n", strings.TrimRight(string(body), "\n"))
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

// instant turns what `--since` and `--until` accept into one end of a window.
// Both spellings are taken because both are natural: a duration is what a
// human types at a terminal, and an instant is what a script computed. Either
// way the duration counts backwards from now, so `--until 1h` is an hour ago.
//
// The flag is passed in because the refusal has to name the flag that was
// actually parsed: being told about `--since` when you typed `--until` sends
// you looking at the wrong half of your own command line.
func (r *run) instant(flag, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return "", usageErrorf("%s must be a duration in the past, got %q", flag, value)
		}
		return r.opt.Now().Add(-duration).UTC().Format(time.RFC3339Nano), nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		// Nano rather than second precision: the sub-second part of what
		// the user typed is part of what they asked for, and dropping it
		// silently widens the window. The archive's arrivals are
		// milliseconds apart (spec 019 #3), which is where a truncated
		// bound stops being harmless.
		return at.UTC().Format(time.RFC3339Nano), nil
	}
	// `30d` is what somebody reaching for a month types first, and Go's
	// duration units stop at the hour (spec 025 #26). The refusal names the
	// unit that does not exist and does the arithmetic, because listing the
	// units again leaves the reader to notice which one is missing from the
	// list and then to multiply.
	if days, found := strings.CutSuffix(value, "d"); found {
		if count, err := strconv.ParseFloat(days, 64); err == nil && count > 0 {
			// `'f'` rather than `%g`: the number is there to be retyped, and
			// `2.4e+06h` is not a duration the parser two lines up would take
			// either (the rule spec 025 #25 set for `scores trend`).
			hours := strconv.FormatFloat(count*24, 'f', -1, 64)
			return "", usageErrorf("%s has no day unit: %s is %sh", flag, value, hours)
		}
	}
	return "", usageErrorf("%s takes a duration (1h, 30m) or an RFC 3339 timestamp, got %q", flag, value)
}

// addSome sets a query parameter only when there is one, so an unset flag is
// never sent as an empty value the server would refuse (spec 003 #23).
func addSome(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

// addCursor passes a page cursor on, and refuses one that arrived without a
// value. `--cursor "$NEXT"` with nothing in NEXT is a script that has lost its
// place, and dropping the parameter answers it with the *newest* page — so the
// walk silently restarts and a loop over the pages never ends. That is spec
// 003 #23's rule, the one addWalk applies to the direction flags, on the
// parameter the walk is actually made of (found in review of PR #27).
func addCursor(query url.Values, fs *flag.FlagSet, cursor string) error {
	if cursor == "" && wasGiven(fs, "cursor") {
		return usageErrorf("--cursor needs the value the previous page printed; " +
			"it was passed empty")
	}
	addSome(query, "cursor", cursor)
	return nil
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
