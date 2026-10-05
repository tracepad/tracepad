package config

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Declared secrets long enough to start with (spec 001 #18).
var (
	secretA = "tp-sk-a-" + strings.Repeat("0", MinSecretLength)
	secretB = "tp-sk-b-" + strings.Repeat("1", MinSecretLength)
)

func TestParseProjects(t *testing.T) {
	specs, err := ParseProjects("app:tp-pk-a:" + secretA + ", eval:tp-pk-b:" + secretB)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "app" || specs[1].SecretKey != secretB {
		t.Fatalf("specs = %+v", specs)
	}

	if _, err := ParseProjects("app:only-two"); err == nil {
		t.Fatal("expected error for malformed entry")
	}
	// A malformed entry is named by its position and never quoted: the
	// error is logged, and what is malformed is usually the secret
	// (spec 001 #12).
	_, err = ParseProjects("app:tp-pk-a:" + secretA + ",eval:tp-pk-b:tp-sk-with:colon")
	if err == nil {
		t.Fatal("expected error for an entry with four fields")
	}
	if strings.Contains(err.Error(), "tp-sk") || !strings.Contains(err.Error(), "entry 2") {
		t.Fatalf("error = %q, want the entry's position and none of its secret", err)
	}
	// Three fields with one empty says which one, still without the value.
	_, err = ParseProjects("app::tp-sk-a")
	if err == nil || !strings.Contains(err.Error(), "field 2 is empty") || strings.Contains(err.Error(), "tp-sk") {
		t.Fatalf("error = %v, want field 2 named as empty and no secret", err)
	}
	if _, err := ParseProjects("app:p:" + secretA + ",app:p2:" + secretB); err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if specs, err := ParseProjects("  "); err != nil || specs != nil {
		t.Fatalf("blank input: specs=%v err=%v", specs, err)
	}
}

// With neither the variable nor the flag the server listens on loopback only
// — both addresses of it, which the server binds for the name (spec 001 #22,
// #23): a bare binary is not reachable from the network until the operator
// says it should be.
func TestListenDefaultsToLoopback(t *testing.T) {
	t.Setenv("TRACEPAD_LISTEN", "")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "localhost:4318" {
		t.Fatalf("Listen = %q, want localhost:4318", cfg.Listen)
	}
	if PlainHTTPBeyondLoopback(cfg.Listen) {
		t.Fatal("the default bind counts as reachable beyond loopback")
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("TRACEPAD_LISTEN", ":9999")
	cfg, err := Load([]string{"--listen", ":4318"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":4318" {
		t.Fatalf("Listen = %q, flags must override env", cfg.Listen)
	}
}

func TestHelpReturnsErrHelp(t *testing.T) {
	if _, err := Load([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

func TestStrayArgumentRejected(t *testing.T) {
	if _, err := Load([]string{"typo"}); err == nil {
		t.Fatal("stray positional argument must be rejected, not silently ignored")
	}
}

// Ingest defaults are the ones spec 002 documents: raw bodies kept, 20 MiB
// body cap.
func TestIngestDefaults(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StoreRaw {
		t.Error("StoreRaw must default to on")
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, DefaultMaxBodyBytes)
	}
}

func TestIngestOverrides(t *testing.T) {
	t.Setenv("TRACEPAD_STORE_RAW", "off")
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "1024")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StoreRaw || cfg.MaxBodyBytes != 1024 {
		t.Fatalf("StoreRaw = %v, MaxBodyBytes = %d", cfg.StoreRaw, cfg.MaxBodyBytes)
	}
}

// A typo in a switch is an error, not a silent fallback to the default:
// silently ingesting without raw bodies would be discovered months later.
func TestIngestRejectsMalformedValues(t *testing.T) {
	t.Setenv("TRACEPAD_STORE_RAW", "maybe")
	if _, err := Load(nil); err == nil {
		t.Error("TRACEPAD_STORE_RAW=maybe must be rejected")
	}
	t.Setenv("TRACEPAD_STORE_RAW", "on")
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "20MB")
	if _, err := Load(nil); err == nil {
		t.Error("TRACEPAD_MAX_BODY_BYTES=20MB must be rejected")
	}
}

// Retention and admin configuration (spec 005). The interval is a duration
// because every other tool an operator knows takes one; the floor exists
// because an interval is also the pause between passes.
func TestSweepIntervalAndAdminToken(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SweepInterval != DefaultSweepInterval {
		t.Errorf("SweepInterval = %s, want %s", cfg.SweepInterval, DefaultSweepInterval)
	}
	if cfg.AdminToken != "" {
		t.Errorf("AdminToken = %q, want unset by default", cfg.AdminToken)
	}

	t.Setenv("TRACEPAD_SWEEP_INTERVAL", "15m")
	t.Setenv("TRACEPAD_ADMIN_TOKEN", "  "+longToken+"\n")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SweepInterval != 15*time.Minute {
		t.Errorf("SweepInterval = %s, want 15m", cfg.SweepInterval)
	}
	if cfg.AdminToken != longToken {
		t.Errorf("AdminToken = %q, want the value without the whitespace a shell leaves behind", cfg.AdminToken)
	}

	t.Setenv("TRACEPAD_SWEEP_INTERVAL", "hourly")
	if _, err := Load(nil); err == nil {
		t.Error("a sweep interval that is not a duration must be refused, not defaulted")
	}
	t.Setenv("TRACEPAD_SWEEP_INTERVAL", "10ms")
	if _, err := Load(nil); err == nil {
		t.Error("a sub-second sweep interval is a busy loop, not a configuration")
	}
}

// longToken is an admin token of the length `openssl rand -hex 32` prints.
var longToken = strings.Repeat("ab", 32)

// The admin token owns the deployment, so it is refused at start when it is
// short enough to guess over the network — with the command that makes a good
// one, and without the value (spec 001 #18). A declared project secret is
// checked at the start itself, where the store says whether it would create a
// project (cmd/tracepad).
func TestShortSecretsRefuseToStart(t *testing.T) {
	short := "tp-admin-devcheck"
	t.Setenv("TRACEPAD_ADMIN_TOKEN", short)
	_, err := Load(nil)
	if err == nil {
		t.Fatal("a 17-character admin token must refuse the start")
	}
	for _, want := range []string{"TRACEPAD_ADMIN_TOKEN", "17 characters", "at least 32", "openssl rand -hex 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), short) {
		t.Errorf("error = %q quotes the token", err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN", strings.Repeat("x", MinSecretLength))
	if _, err := Load(nil); err != nil {
		t.Errorf("a token of exactly %d characters: %v", MinSecretLength, err)
	}
	// Characters, as the rule and the message say: eleven letters of
	// three bytes each are thirty-three bytes and eleven characters.
	t.Setenv("TRACEPAD_ADMIN_TOKEN", strings.Repeat("\u20ac", 11))
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "11 characters") {
		t.Errorf("err = %v, want eleven characters refused as eleven", err)
	}
}

// TRACEPAD_ADMIN_TOKEN_FILE reads the token from a file — a mounted secret —
// and is one way or the other, never both (spec 001 #18).
func TestAdminTokenFile(t *testing.T) {
	t.Setenv("TRACEPAD_ADMIN_TOKEN", "")
	path := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(path, []byte(longToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", path)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminToken != longToken {
		t.Errorf("AdminToken = %q, want the file's contents without the newline", cfg.AdminToken)
	}

	t.Setenv("TRACEPAD_ADMIN_TOKEN", longToken)
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Errorf("err = %v, want the two ways refused together", err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN", "")

	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "TRACEPAD_ADMIN_TOKEN_FILE") ||
		strings.Contains(err.Error(), "short\n") {
		t.Errorf("err = %v, want the short token in the file refused by the variable's name", err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want an empty file refused rather than read as no token", err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(nil); err == nil {
		t.Error("a file that is not there must refuse the start")
	}
	// A path that went to the wrong place is an error, not a read that
	// never ends: a directory, a device, a file far larger than a token.
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", t.TempDir())
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want a directory refused", err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", os.DevNull)
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want a device refused", err)
	}
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, []byte(longToken+strings.Repeat("\n", maxAdminTokenFile)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", big)
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want a file past 4 KiB refused", err)
	}
}

// TRACEPAD_SETUP=off turns the setup endpoint off; on is the default (spec
// 028 #32).
func TestSetupSwitch(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SetupDisabled {
		t.Error("setup must be on by default")
	}
	t.Setenv("TRACEPAD_SETUP", "off")
	if cfg, err = Load(nil); err != nil || !cfg.SetupDisabled {
		t.Errorf("TRACEPAD_SETUP=off: cfg=%+v err=%v", cfg, err)
	}
	t.Setenv("TRACEPAD_SETUP", "sometimes")
	if _, err := Load(nil); err == nil {
		t.Error("a switch that is neither on nor off must be refused")
	}
}

// The browser session's lifetime (spec 028 #4, Config additions).
func TestSessionDays(t *testing.T) {
	t.Setenv("TRACEPAD_SESSION_DAYS", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionLife != DefaultSessionLife {
		t.Errorf("SessionLife = %s, want the documented default %s", cfg.SessionLife, DefaultSessionLife)
	}

	t.Setenv("TRACEPAD_SESSION_DAYS", "7")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionLife != 7*24*time.Hour {
		t.Errorf("SessionLife = %s, want 168h", cfg.SessionLife)
	}

	t.Setenv("TRACEPAD_SESSION_DAYS", "0")
	if _, err := Load(nil); err == nil {
		t.Error("a session shorter than the slide's own interval is a broken session, not a short one")
	}
	t.Setenv("TRACEPAD_SESSION_DAYS", "a month")
	if _, err := Load(nil); err == nil {
		t.Error("a session length that is not a number must be refused, not defaulted")
	}
}

// The two read bounds are validated at start: out of range refuses to start,
// as TRACEPAD_RESPONSE_BUDGET_BYTES does (spec 043 #22).
func TestReadBounds(t *testing.T) {
	t.Setenv("TRACEPAD_READ_TIMEOUT", "")
	t.Setenv("TRACEPAD_READ_CONCURRENCY", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadTimeout != 20*time.Second {
		t.Errorf("ReadTimeout = %s, want the documented 20s", cfg.ReadTimeout)
	}
	if want := max(4, 2*runtime.GOMAXPROCS(0)); cfg.ReadConcurrency != want {
		t.Errorf("ReadConcurrency = %d, want twice GOMAXPROCS and at least 4, %d", cfg.ReadConcurrency, want)
	}
	for procs, want := range map[int]int{1: 4, 2: 4, 3: 6, 16: 32} {
		if got := DefaultReadConcurrency(procs); got != want {
			t.Errorf("DefaultReadConcurrency(%d) = %d, want %d", procs, got, want)
		}
	}

	t.Setenv("TRACEPAD_READ_TIMEOUT", "1s")
	t.Setenv("TRACEPAD_READ_CONCURRENCY", "1")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadTimeout != time.Second || cfg.ReadConcurrency != 1 {
		t.Errorf("read bounds = %s, %d; want the floors 1s and 1", cfg.ReadTimeout, cfg.ReadConcurrency)
	}

	for name, value := range map[string]string{
		"TRACEPAD_READ_TIMEOUT":     "999ms",
		"TRACEPAD_READ_CONCURRENCY": "0",
	} {
		t.Run(name+"="+value, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("Load = %v, want a refusal naming %s", err, name)
			}
		})
	}
	// Past the five minutes a response has to be written, the transport
	// would cut the connection before the deadline could answer.
	t.Setenv("TRACEPAD_READ_TIMEOUT", "4m")
	if _, err := Load(nil); err != nil {
		t.Errorf("4m, the ceiling, refused: %v", err)
	}
	t.Setenv("TRACEPAD_READ_TIMEOUT", "5m")
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "TRACEPAD_READ_TIMEOUT") {
		t.Errorf("Load = %v, want a refusal of a deadline the transport would cut", err)
	}
	t.Setenv("TRACEPAD_READ_TIMEOUT", "")

	for name, value := range map[string]string{
		"TRACEPAD_READ_TIMEOUT":     "twenty seconds",
		"TRACEPAD_READ_CONCURRENCY": "many",
	} {
		t.Run(name+"="+value, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(nil); err == nil {
				t.Errorf("%s=%q must be refused, not defaulted", name, value)
			}
		})
	}
}

// The connection limit is validated at start (spec 043 #45): 1024 unless set,
// at least 16, and a value that is not a number refuses to start.
func TestMaxConnections(t *testing.T) {
	t.Setenv("TRACEPAD_MAX_CONNECTIONS", "")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConnections != 1024 {
		t.Errorf("MaxConnections = %d, want the documented 1024", cfg.MaxConnections)
	}
	t.Setenv("TRACEPAD_MAX_CONNECTIONS", "16")
	if cfg, err := Load(nil); err != nil || cfg.MaxConnections != 16 {
		t.Errorf("16, the floor: cfg = %+v, err = %v", cfg, err)
	}
	for _, value := range []string{"15", "0", "-1", "many"} {
		t.Setenv("TRACEPAD_MAX_CONNECTIONS", value)
		if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "TRACEPAD_MAX_CONNECTIONS") {
			t.Errorf("TRACEPAD_MAX_CONNECTIONS=%q: Load = %v, want a refusal naming it", value, err)
		}
	}

	// One source's share: a quarter unless set, from 1 to the total.
	t.Setenv("TRACEPAD_MAX_CONNECTIONS", "100")
	t.Setenv("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE", "")
	if cfg, err := Load(nil); err != nil || cfg.MaxConnectionsPerSource != 25 {
		t.Errorf("per source by default = %+v, %v; want a quarter, 25", cfg, err)
	}
	t.Setenv("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE", "100")
	if cfg, err := Load(nil); err != nil || cfg.MaxConnectionsPerSource != 100 {
		t.Errorf("per source at the total refused: %v", err)
	}
	for _, value := range []string{"0", "101", "some"} {
		t.Setenv("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE", value)
		if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "TRACEPAD_MAX_CONNECTIONS_PER_SOURCE") {
			t.Errorf("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE=%q: Load = %v, want a refusal naming it", value, err)
		}
	}
}

// The two ingest bounds are validated at start (spec 043 #10, #13, #22): the
// budget follows the body cap unless set, and a budget smaller than the cap —
// one that would refuse a body the cap admits on an idle server — refuses to
// start.
func TestIngestBounds(t *testing.T) {
	t.Setenv("TRACEPAD_MAX_SPANS_PER_REQUEST", "")
	t.Setenv("TRACEPAD_BODY_BUDGET_BYTES", "")
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSpansPerRequest != 20000 {
		t.Errorf("MaxSpansPerRequest = %d, want the documented 20000", cfg.MaxSpansPerRequest)
	}
	if cfg.BodyBudgetBytes != 80<<20 {
		t.Errorf("BodyBudgetBytes = %d, want four times the 20 MiB cap", cfg.BodyBudgetBytes)
	}

	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "1000000")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BodyBudgetBytes != 4000000 {
		t.Errorf("BodyBudgetBytes = %d under a 1 MB cap, want 4 MB", cfg.BodyBudgetBytes)
	}

	// Four of a cap this large would not fit an int64: the default holds at
	// the largest one rather than wrapping negative.
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "3000000000000000000")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("an enormous cap with no budget set: %v", err)
	}
	if cfg.BodyBudgetBytes != math.MaxInt64 {
		t.Errorf("BodyBudgetBytes = %d, want the largest int64", cfg.BodyBudgetBytes)
	}
	t.Setenv("TRACEPAD_MAX_BODY_BYTES", "1000000")

	t.Setenv("TRACEPAD_MAX_SPANS_PER_REQUEST", "1")
	t.Setenv("TRACEPAD_BODY_BUDGET_BYTES", "1000000")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSpansPerRequest != 1 || cfg.BodyBudgetBytes != 1000000 {
		t.Errorf("ingest bounds = %d, %d; want the floors 1 and the cap", cfg.MaxSpansPerRequest, cfg.BodyBudgetBytes)
	}

	for name, value := range map[string]string{
		"TRACEPAD_MAX_SPANS_PER_REQUEST": "0",
		"TRACEPAD_BODY_BUDGET_BYTES":     "999999",
	} {
		t.Run(name+"="+value, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("Load = %v, want a refusal naming %s", err, name)
			}
		})
	}
	for name, value := range map[string]string{
		"TRACEPAD_MAX_SPANS_PER_REQUEST": "lots",
		"TRACEPAD_BODY_BUDGET_BYTES":     "80MiB",
	} {
		t.Run(name+"="+value, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(nil); err == nil {
				t.Errorf("%s=%q must be refused, not defaulted", name, value)
			}
		})
	}
}

// TRACEPAD_URL is the CLI's "which server" and, since spec 028 #11, the host
// of the links the server prints.
func TestPublicURL(t *testing.T) {
	t.Setenv("TRACEPAD_URL", "  https://traces.example.com  ")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://traces.example.com" {
		t.Errorf("URL = %q, want the value without the whitespace a shell leaves behind", cfg.URL)
	}
}

// The image sets TRACEPAD_IN_CONTAINER=1 (spec 020 #20); anywhere else it is
// unset, and a known variable, so it is never reported as a typo.
func TestInContainer(t *testing.T) {
	t.Setenv("TRACEPAD_IN_CONTAINER", "")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InContainer {
		t.Error("InContainer set with TRACEPAD_IN_CONTAINER unset")
	}
	t.Setenv("TRACEPAD_IN_CONTAINER", "1")
	if cfg, err = Load(nil); err != nil || !cfg.InContainer {
		t.Errorf("TRACEPAD_IN_CONTAINER=1: InContainer = %v, err = %v", cfg != nil && cfg.InContainer, err)
	}
	if !IsKnownEnv("TRACEPAD_IN_CONTAINER") {
		t.Error("TRACEPAD_IN_CONTAINER would be warned about as unknown")
	}
}

// A shared .env carries the packages' variables to the server (the docs
// recommend one), and none of them is a typo (spec 001 #24).
func TestWarnUnknownEnvAcceptsThePackagesVariables(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, v := range Env {
		t.Setenv(v.Name, "x")
	}

	warnUnknownEnv()

	if strings.Contains(logs.String(), "unknown TRACEPAD_*") {
		t.Errorf("a variable of config.Env is warned about: %s", logs.String())
	}
}

// TRACEPAD_HOST is the packages' deprecated name for TRACEPAD_URL (spec 017
// #21): a shared .env carries it to the server, which says what replaced it
// at Info instead of calling it a typo, and still calls a real typo one.
func TestWarnUnknownEnvNamesTheDeprecatedHost(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv("TRACEPAD_HOST", "http://localhost:4318")
	t.Setenv("TRACEPAD_LISTNE", ":4318")

	warnUnknownEnv()

	out := logs.String()
	if !strings.Contains(out, "deprecated environment variable") || strings.Contains(out, "unknown TRACEPAD_* environment variable\" name=TRACEPAD_HOST") {
		t.Errorf("TRACEPAD_HOST: %s", out)
	}
	if !strings.Contains(out, "unknown TRACEPAD_* environment variable\" name=TRACEPAD_LISTNE") {
		t.Errorf("a typo is still reported as unknown: %s", out)
	}
}

func TestDisplayHost(t *testing.T) {
	cases := map[string]string{
		":4318":          "localhost:4318",
		"0.0.0.0:4318":   "localhost:4318",
		"[::]:4318":      "localhost:4318",
		"localhost:4318": "localhost:4318",
		"127.0.0.1:4318": "127.0.0.1:4318",
		"[::1]:4318":     "[::1]:4318",
		"10.0.0.5:4318":  "10.0.0.5:4318",
		"myhost:4318":    "myhost:4318",
	}
	for in, want := range cases {
		if got := DisplayHost(in); got != want {
			t.Errorf("DisplayHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// Tracepad serves no TLS of its own, so a listener other machines can reach is
// one they send passwords and keys to in clear (spec 001 #12) — whatever
// TRACEPAD_URL says, since a proxy in front does not stop anybody from
// connecting here directly (#22).
func TestPlainHTTPBeyondLoopback(t *testing.T) {
	cases := map[string]bool{
		":4318":                true,
		"0.0.0.0:4318":         true,
		"[::]:4318":            true,
		"192.168.1.20:4318":    true,
		"traces.internal:4318": true,
		"127.0.0.1:4318":       false,
		"127.0.0.2:4318":       false,
		"[::1]:4318":           false,
		"localhost:4318":       false,
		"LOCALHOST:4318":       false,
		"not-an-address":       false,
	}
	for listen, want := range cases {
		if got := PlainHTTPBeyondLoopback(listen); got != want {
			t.Errorf("PlainHTTPBeyondLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

// An https TRACEPAD_URL is the operator's word that people come in through a
// TLS proxy (spec 001 #12); the warning reads it for its wording (#22).
func TestHTTPSURL(t *testing.T) {
	for publicURL, want := range map[string]bool{
		"https://traces.example.com":   true,
		" HTTPS://traces.example.com ": true,
		"http://traces.example.com":    false,
		"":                             false,
		"traces.example.com":           false,
	} {
		if got := HTTPSURL(publicURL); got != want {
			t.Errorf("HTTPSURL(%q) = %v, want %v", publicURL, got, want)
		}
	}
}

// TestTrustedProxies: TRACEPAD_TRUSTED_PROXIES is loopback by default, none
// trusts nobody, and addresses and ranges parse to their networks. An entry
// that does not parse refuses to start, named with its position: a typo that
// dropped a proxy would put every client behind it into one source (spec 046
// #1).
func TestTrustedProxies(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := ParseTrustedProxies(cfg.TrustedProxies); len(list) != 2 || list[0].String() != "127.0.0.0/8" {
		t.Errorf("unset: %v, want loopback", list)
	}
	if list, err := ParseTrustedProxies("none"); err != nil || list == nil || len(list) != 0 {
		t.Errorf("none: %v, %v; want an empty list", list, err)
	}

	value := " 172.17.0.1/16 , 10.0.0.5,loopback, 2001:db8::/32, ::ffff:192.0.2.0/120"
	t.Setenv("TRACEPAD_TRUSTED_PROXIES", value)
	if cfg, err = Load(nil); err != nil {
		t.Fatal(err)
	}
	list, err := ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, prefix := range list {
		got = append(got, prefix.String())
	}
	want := "172.17.0.0/16 10.0.0.5/32 127.0.0.0/8 ::1/128 2001:db8::/32 192.0.2.0/24"
	if strings.Join(got, " ") != want {
		t.Errorf("parsed %v, want %s", got, want)
	}
	// The widest ranges a real fleet of proxies has pass: an /8, and
	// Cloudflare's /13 and /29 (#18).
	for _, wide := range []string{"10.0.0.0/8", "104.16.0.0/13", "2a06:98c0::/29", "2400::/16"} {
		if _, err := ParseTrustedProxies(wide); err != nil {
			t.Errorf("%s: %v, want it accepted", wide, err)
		}
	}

	for _, bad := range []string{"10.0.0.5,proxy.internal", "10.0.0.5,,10.0.0.6", "10.0.0.0/33", "none,10.0.0.5", "fe80::1%eth0",
		// Every address: the limit would be off (#17).
		"0.0.0.0/0", "::/0", "::ffff:0:0/96", "10.0.0.5, 0.0.0.0/0",
		// Nor split into halves, nor wider than one network's proxies (#18).
		"0.0.0.0/1,128.0.0.0/1", "::/1,8000::/1", "10.0.0.0/7", "2000::/3", "::ffff:10.0.0.0/103"} {
		t.Setenv("TRACEPAD_TRUSTED_PROXIES", bad)
		if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "TRACEPAD_TRUSTED_PROXIES: entry") {
			t.Errorf("%q: err = %v, want a refusal naming the entry", bad, err)
		}
	}
}
