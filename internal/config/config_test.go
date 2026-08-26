package config

import (
	"errors"
	"flag"
	"testing"
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
