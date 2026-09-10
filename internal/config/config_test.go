package config

import (
	"errors"
	"flag"
	"testing"
	"time"
)

func TestParseProjects(t *testing.T) {
	specs, err := ParseProjects("app:tp-pk-a:tp-sk-a, eval:tp-pk-b:tp-sk-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "app" || specs[1].SecretKey != "tp-sk-b" {
		t.Fatalf("specs = %+v", specs)
	}

	if _, err := ParseProjects("app:only-two"); err == nil {
		t.Fatal("expected error for malformed entry")
	}
	if _, err := ParseProjects("app:p:s,app:p2:s2"); err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if specs, err := ParseProjects("  "); err != nil || specs != nil {
		t.Fatalf("blank input: specs=%v err=%v", specs, err)
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
	t.Setenv("TRACEPAD_ADMIN_TOKEN", "  secret  ")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SweepInterval != 15*time.Minute {
		t.Errorf("SweepInterval = %s, want 15m", cfg.SweepInterval)
	}
	if cfg.AdminToken != "secret" {
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

func TestDisplayHost(t *testing.T) {
	cases := map[string]string{
		":4318":          "localhost:4318",
		"0.0.0.0:4318":   "localhost:4318",
		"[::]:4318":      "localhost:4318",
		"127.0.0.1:4318": "127.0.0.1:4318",
		"myhost:4318":    "myhost:4318",
	}
	for in, want := range cases {
		if got := DisplayHost(in); got != want {
			t.Errorf("DisplayHost(%q) = %q, want %q", in, got, want)
		}
	}
}
