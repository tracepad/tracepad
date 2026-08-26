// Package config resolves Tracepad configuration from environment variables
// and command-line flags. Flags override environment (spec 001).
package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Listen is the HTTP listen address. Defaults to :4318 so that
	// unconfigured OTel SDK exporters (default endpoint localhost:4318)
	// reach a locally running Tracepad (spec 001 #2).
	Listen string
	// DataDir holds the SQLite database and, later, payload storage.
	DataDir string
	// Projects is the raw TRACEPAD_PROJECTS declarative bootstrap value,
	// "name:pk:sk[,name:pk:sk...]" (spec 001 #9).
	Projects string
	// StoreRaw keeps every accepted OTLP body for later remap/export
	// (spec 002 #9). On by default: raw copies are what makes a mapping
	// bug a retroactively fixable mistake instead of data loss.
	StoreRaw bool
	// MaxBodyBytes caps an ingest request body (spec 002 #16). Default
	// batch processors ship far below it; the cap exists to stop
	// pathological bodies from ballooning memory.
	MaxBodyBytes int64
}

// DefaultMaxBodyBytes is the request body cap when unset (20 MiB).
const DefaultMaxBodyBytes = 20 * 1024 * 1024

// knownEnv lists every TRACEPAD_* variable the binary understands.
var knownEnv = map[string]bool{
	"TRACEPAD_LISTEN":         true,
	"TRACEPAD_DATA_DIR":       true,
	"TRACEPAD_PROJECTS":       true,
	"TRACEPAD_STORE_RAW":      true,
	"TRACEPAD_MAX_BODY_BYTES": true,
}

// Load resolves configuration from env and the given flag arguments.
func Load(args []string) (*Config, error) {
	storeRaw, err := parseOnOff("TRACEPAD_STORE_RAW", true)
	if err != nil {
		return nil, err
	}
	maxBody, err := parseBytes("TRACEPAD_MAX_BODY_BYTES", DefaultMaxBodyBytes)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:       envOr("TRACEPAD_LISTEN", ":4318"),
		DataDir:      envOr("TRACEPAD_DATA_DIR", defaultDataDir()),
		Projects:     os.Getenv("TRACEPAD_PROJECTS"),
		StoreRaw:     storeRaw,
		MaxBodyBytes: maxBody,
	}

	fs := flag.NewFlagSet("tracepad", flag.ContinueOnError)
	// The caller owns user-facing usage output; suppress the FlagSet's own
	// printing and let the returned error (flag.ErrHelp included) drive it.
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTP listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory (database, payloads)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	warnUnknownEnv()
	return cfg, nil
}

// DBPath returns the path of the SQLite database file.
func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "tracepad.db")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseOnOff reads a boolean-ish switch. Spelled on/off in the docs, but the
// usual true/false/1/0 are accepted too — an operator should never have to
// look up which spelling this particular flag wanted.
func parseOnOff(key string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def, nil
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s: want on or off, got %q", key, v)
}

// parseBytes reads a plain byte count. No k/M/G suffixes: the value appears
// in exactly one place (the docs table) and an ambiguous suffix convention
// costs more than it saves.
func parseBytes(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: want a positive byte count, got %q", key, v)
	}
	return n, nil
}

func defaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "tracepad")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Last resort; a relative dir still lets the server start.
		return "tracepad-data"
	}
	return filepath.Join(home, ".local", "share", "tracepad")
}

// warnUnknownEnv flags TRACEPAD_* variables the binary does not understand —
// a cheap typo detector (spec 001, Configuration).
func warnUnknownEnv() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "TRACEPAD_") && !knownEnv[name] {
			slog.Warn("unknown TRACEPAD_* environment variable", "name", name)
		}
	}
}

// ParseProjects parses the TRACEPAD_PROJECTS value into bootstrap entries.
// Format: comma-separated "name:public_key:secret_key". Duplicate names are
// an error (ambiguous intent, spec 001 edge cases).
func ParseProjects(raw string) ([]ProjectSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var specs []ProjectSpec
	for _, part := range strings.Split(raw, ",") {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("TRACEPAD_PROJECTS entry %q: want name:public_key:secret_key", part)
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("TRACEPAD_PROJECTS: duplicate project name %q", fields[0])
		}
		seen[fields[0]] = true
		specs = append(specs, ProjectSpec{Name: fields[0], PublicKey: fields[1], SecretKey: fields[2]})
	}
	return specs, nil
}

// ProjectSpec is one declaratively provisioned project.
type ProjectSpec struct {
	Name      string
	PublicKey string
	SecretKey string
}
