package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in      []string
		cmd     string
		numArgs int
	}{
		{nil, "serve", 0},
		{[]string{"--listen", ":9999"}, "serve", 2}, // spec 001 #1: leading flag = default command
		{[]string{"serve", "--listen", ":9999"}, "serve", 2},
		{[]string{"version"}, "version", 0},
	}
	for _, c := range cases {
		cmd, args := splitCommand(c.in)
		if cmd != c.cmd || len(args) != c.numArgs {
			t.Errorf("splitCommand(%v) = %q, %v", c.in, cmd, args)
		}
	}
}

// A secret the operator declared in TRACEPAD_PROJECTS is theirs already; the
// banner names where it is instead of copying it into the log. A generated one
// has no other way out, so it is printed once (spec 001 #9, #12).
func TestPrintStartupPrintsOnlyGeneratedSecrets(t *testing.T) {
	var out bytes.Buffer
	printStartup(&out, &store.BootstrapResult{Created: []store.BootstrapCreated{
		{Project: store.Project{Name: "default"}, Keys: store.KeyPair{PublicKey: "tp-pk-gen", Secret: "tp-sk-generated"},
			Scopes: store.GeneratedKeyScopes},
		{Project: store.Project{Name: "app"}, Keys: store.KeyPair{PublicKey: "tp-pk-app", Secret: "tp-sk-declared"},
			Declared: true, Scopes: store.AllScopes},
	}}, "localhost:4318", "", true)

	got := out.String()
	if strings.Contains(got, "tp-sk-declared") {
		t.Errorf("the declared secret was printed:\n%s", got)
	}
	for _, want := range []string{
		`Project "default" created. Connect your app with either:`,
		// No gRPC receiver: an exporter left to its default reports nothing.
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		// The default bind is both loopback addresses, printed as the name
		// that reaches both (spec 001 #22, #23).
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces",
		// The JavaScript SDK reads LANGFUSE_BASE_URL alone and sends to
		// Langfuse's cloud without it (spec 002 #35); the old name stays for
		// the SDKs that read only that one.
		"LANGFUSE_BASE_URL=http://localhost:4318\n  LANGFUSE_HOST=http://localhost:4318",
		"Bearer tp-sk-generated",
		"LANGFUSE_SECRET_KEY=tp-sk-generated",
		`Project "app" created from TRACEPAD_PROJECTS.`,
		"LANGFUSE_PUBLIC_KEY=tp-pk-app",
		"LANGFUSE_SECRET_KEY=<its secret key from TRACEPAD_PROJECTS>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}

	// The printed key is for the application and says so, with where a key
	// that reads comes from; the declared one holds all three and needs no
	// such line (spec 045 #28).
	ingestOnly := "This key holds the ingest scope alone"
	if strings.Count(got, ingestOnly) != 1 {
		t.Errorf("want the ingest-only note once, under the generated key:\n%s", got)
	}
	generated, declared, _ := strings.Cut(got, `Project "app"`)
	if !strings.Contains(generated, ingestOnly) || strings.Contains(declared, ingestOnly) {
		t.Errorf("the ingest-only note is under the wrong project:\n%s", got)
	}
	if !strings.Contains(generated, "tracepad keys create --scope read,write") {
		t.Errorf("the note does not say where a reading key comes from:\n%s", got)
	}
}

// The way to a key that reads is one this server offers: the interface when
// somebody can sign in to it, the admin token when there is one, and with
// neither, how to configure the token — never a screen nobody can reach or a
// token that does not exist (spec 045 #28).
func TestReadKeyHint(t *testing.T) {
	cases := []struct {
		signIn, token bool
		want, not     []string
	}{
		{true, true, []string{"Settings → Project → API keys", "with the admin token", "--scope read,write"}, []string{"set TRACEPAD_ADMIN_TOKEN"}},
		{true, false, []string{"Settings → Project → API keys"}, []string{"admin token", "keys create"}},
		{false, true, []string{"with the admin token", "--scope read,write"}, []string{"Settings", "set TRACEPAD_ADMIN_TOKEN"}},
		{false, false, []string{"set TRACEPAD_ADMIN_TOKEN", "restart", "--scope read,write"}, []string{"Settings"}},
	}
	for _, c := range cases {
		hint := readKeyHint(c.signIn, c.token)
		if !strings.HasPrefix(hint, "This key holds the ingest scope alone") {
			t.Errorf("sign-in %v, token %v: the hint does not say what the key is:\n%s", c.signIn, c.token, hint)
		}
		for _, want := range c.want {
			if !strings.Contains(hint, want) {
				t.Errorf("sign-in %v, token %v: want %q in\n%s", c.signIn, c.token, want, hint)
			}
		}
		for _, not := range c.not {
			if strings.Contains(hint, not) {
				t.Errorf("sign-in %v, token %v: %q offers a way this server does not have:\n%s", c.signIn, c.token, not, hint)
			}
		}
	}
}

// Other machines reaching a plain-HTTP listener is a warning at start, naming
// the address — and, in the image, where the wildcard bind is the design and
// the publish decides the reach, one INFO line instead (spec 001 #12). An
// https TRACEPAD_URL silences neither: a proxy in front does not stop anybody
// from connecting here directly (#22).
func TestWarnPlainHTTP(t *testing.T) {
	cases := []struct {
		listen, url string
		container   bool
		level       string // "" when nothing is logged
	}{
		{":4318", "", false, "WARN"},
		{":4318", "https://traces.example.com", false, "WARN"},
		{"192.168.1.20:4318", "HTTPS://traces.example.com", false, "WARN"},
		{"127.0.0.1:4318", "", false, ""},
		{"127.0.0.1:4318", "https://traces.example.com", false, ""},
		{":4318", "", true, "INFO"},
		{":4318", "https://traces.example.com", true, "INFO"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		warnPlainHTTP(slog.New(slog.NewTextHandler(&out, nil)), c.listen, c.url, c.container)
		logged := out.String()
		if c.level == "" {
			if logged != "" {
				t.Errorf("listen %q, url %q, container %v: want nothing logged, got\n%s",
					c.listen, c.url, c.container, logged)
			}
			continue
		}
		if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "level="+c.level) ||
			!strings.Contains(logged, "listen="+c.listen) {
			t.Errorf("listen %q, url %q, container %v: want one %s line naming the address, got\n%s",
				c.listen, c.url, c.container, c.level, logged)
		}
		fix := "TRACEPAD_URL"
		if c.container {
			fix = "docs/docker.md"
		}
		if !strings.Contains(logged, fix) {
			t.Errorf("the line should point at %s:\n%s", fix, logged)
		}
		// With an https URL the host's line says what the proxy does not
		// cover, rather than asking for the URL it already has.
		if !c.container && c.url != "" && !strings.Contains(logged, "skips the TLS proxy") {
			t.Errorf("listen %q, url %q: the line should say a direct client skips the proxy:\n%s",
				c.listen, c.url, logged)
		}
	}
}

// A short secret in TRACEPAD_PROJECTS refuses the start only where lengthening
// it fixes something: an entry that creates a project. For a project that
// exists the declaration creates nothing — its keys are never rotated from the
// variable — so the start goes on and says how to replace the key, when the
// short secret is still one (spec 001 #18).
func TestCheckDeclaredSecrets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateProject("app", store.KeyPair{PublicKey: "tp-pk-app", Secret: "tp-sk-short"}); err != nil {
		t.Fatal(err)
	}
	long := "tp-sk-" + strings.Repeat("0", 32)
	check := func(specs ...store.ProvisionSpec) (string, error) {
		var logged bytes.Buffer
		err := checkDeclaredSecrets(slog.New(slog.NewTextHandler(&logged, nil)), st, specs)
		return logged.String(), err
	}

	// A new project with a short secret: refused, by position, without it.
	_, err = check(store.ProvisionSpec{Name: "fresh", PublicKey: "tp-pk-fresh", SecretKey: long},
		store.ProvisionSpec{Name: "new", PublicKey: "tp-pk-new", SecretKey: "tp-sk-tiny"})
	if err == nil {
		t.Fatal("a short secret that would create a project must refuse the start")
	}
	for _, want := range []string{"entry 2", "10 characters", "at least 32", "openssl rand -hex 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "tp-sk-tiny") {
		t.Errorf("error = %q quotes the secret", err)
	}

	// An existing project whose short secret is still its key: the start
	// goes on, and says how to replace it.
	logged, err := check(store.ProvisionSpec{Name: "app", PublicKey: "tp-pk-app", SecretKey: "tp-sk-short"})
	if err != nil {
		t.Fatalf("a short secret for an existing project refused the start: %v", err)
	}
	for _, want := range []string{"WARN", "project=app", "public_key=tp-pk-app", "revoke"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log = %q, want it to say %q", logged, want)
		}
	}
	if strings.Contains(logged, "tp-sk-short") {
		t.Errorf("log = %q quotes the secret", logged)
	}

	// Lengthened in the variable but not rotated: the server cannot tell,
	// and the short key it cannot see is not one it can warn about by
	// secret. Nothing is said about a short secret that is no key at all.
	logged, err = check(store.ProvisionSpec{Name: "app", PublicKey: "tp-pk-app", SecretKey: "tp-sk-other"})
	if err != nil || logged != "" {
		t.Errorf("a short secret that is no live key: err=%v log=%q, want neither", err, logged)
	}
	if logged, err := check(store.ProvisionSpec{Name: "app", PublicKey: "tp-pk-app", SecretKey: long}); err != nil || logged != "" {
		t.Errorf("a long secret: err=%v log=%q, want neither", err, logged)
	}

	// A project on its way out: the bootstrap skips it, and so does the
	// warning — minting a pair in its settings is no advice for it.
	gone, err := st.CreateProject("gone", store.KeyPair{PublicKey: "tp-pk-gone", Secret: "tp-sk-gone"})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := st.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Submit(t.Context(), &store.ProjectDelete{ProjectID: gone.ID, Confirm: "gone", Now: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if logged, err := check(store.ProvisionSpec{Name: "gone", PublicKey: "tp-pk-gone", SecretKey: "tp-sk-gone"}); err != nil || logged != "" {
		t.Errorf("a short key of a deleted project: err=%v log=%q, want neither", err, logged)
	}
}

// `serve` on a data directory another server holds refuses before it opens
// the database, and says so (spec 001 #20).
func TestServeRefusesADataDirectoryInUse(t *testing.T) {
	dir := t.TempDir()
	held, err := store.LockDatabase(filepath.Join(dir, "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	err = serve([]string{"--data-dir", dir, "--listen", "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "another tracepad is already running") {
		t.Fatalf("serve on a held data directory = %v, want the refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "tracepad.db")); statErr == nil {
		t.Error("the refused server had already created the database")
	}
}

// cleanTracepadEnv removes every TRACEPAD_* variable for the test and puts
// them back after it: a start's log depends on the environment, and a
// developer's own server settings must not decide whether a test passes.
func cleanTracepadEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TRACEPAD_") {
			t.Setenv(name, "") // registers the restore
			os.Unsetenv(name)
		}
	}
}

// logsTo sends the default logger into a buffer for the test.
func logsTo(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// The version heads the log (spec 001 #25): ahead of the environment's
// warnings and of a refusal to start, which is what a report is made of.
func TestServeLogsTheVersionFirst(t *testing.T) {
	cleanTracepadEnv(t)
	t.Setenv("TRACEPAD_NOT_A_SETTING", "1") // a warning that would otherwise come first
	logs := logsTo(t)

	dir := t.TempDir()
	held, err := store.LockDatabase(filepath.Join(dir, "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var help bytes.Buffer
	args := []string{"--data-dir", dir, "--listen", "127.0.0.1:0"}
	if err := serveAs(args, buildLabel("0.1.0-rc.1", "b14b11e2a9c0", nil), &help); err == nil {
		t.Fatal("serve on a held data directory started")
	}

	first, _, _ := strings.Cut(logs.String(), "\n")
	if !strings.Contains(first, `msg="tracepad 0.1.0-rc.1 (b14b11e)"`) {
		t.Errorf("first log line = %q, want the version and the short commit", first)
	}
	if !strings.Contains(logs.String(), "TRACEPAD_NOT_A_SETTING") {
		t.Errorf("the environment warning never logged, so this test proved nothing about order:\n%s", logs.String())
	}
	if help.Len() != 0 {
		t.Errorf("a start printed usage: %s", help.String())
	}
}

// The version is first when the environment is what refuses the start, too.
func TestServeLogsTheVersionBeforeAnEnvironmentRefusal(t *testing.T) {
	cleanTracepadEnv(t)
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "not a size")
	logs := logsTo(t)

	err := serveAs([]string{"--data-dir", t.TempDir()}, "tracepad test", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "TRACEPAD_MAX_BODY_BYTES") {
		t.Fatalf("serve = %v, want the environment's refusal", err)
	}
	if first, _, _ := strings.Cut(logs.String(), "\n"); !strings.Contains(first, `msg="tracepad test"`) {
		t.Errorf("first log line = %q, want the version", first)
	}
}

// A request for help is not a start, in every spelling the flag package has
// for it, and a value that happens to read `-h` is not a request.
func TestServeHelpLogsNoVersion(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--h"}, {"-help"}, {"--help"}, {"-h=true"}, {"--help=true"}, {"--listen", "127.0.0.1:0", "-h"}} {
		cleanTracepadEnv(t)
		logs := logsTo(t)
		var help bytes.Buffer
		if err := serveAs(args, "tracepad test", &help); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if logs.Len() != 0 {
			t.Errorf("%v: a request for help logged a start: %s", args, logs.String())
		}
		if !strings.Contains(help.String(), "Usage:") {
			t.Errorf("%v: no usage was printed: %q", args, help.String())
		}
	}
}

func TestServeTakesHelpAsTheValueOfAFlag(t *testing.T) {
	cleanTracepadEnv(t)
	logs := logsTo(t)
	// The data directory is literally named `-h`, relative to a directory of
	// the test's own; the lock on it makes the start refuse, which is all this
	// needs of it.
	t.Chdir(t.TempDir())
	dir := "-h"
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := store.LockDatabase(filepath.Join(dir, "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	var help bytes.Buffer
	err = serveAs([]string{"--data-dir", dir, "--listen", "127.0.0.1:0"}, "tracepad test", &help)
	if err == nil || !strings.Contains(err.Error(), "another tracepad is already running") {
		t.Fatalf("serve = %v, want the refusal of a start", err)
	}
	if first, _, _ := strings.Cut(logs.String(), "\n"); !strings.Contains(first, `msg="tracepad test"`) {
		t.Errorf("first log line = %q, want the version", first)
	}
	if help.Len() != 0 {
		t.Errorf("usage printed for a start: %s", help.String())
	}
}
