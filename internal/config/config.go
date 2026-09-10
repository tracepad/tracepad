// Package config resolves Tracepad configuration from environment variables
// and command-line flags. Flags override environment (spec 001).
package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// ResponseBudgetBytes is the default byte budget a read response
	// spends on payloads (spec 004 #2). The consumer's context window is
	// a scarce resource, so the API respects it by default rather than on
	// request.
	ResponseBudgetBytes int64
	// MCP serves the MCP endpoint at /mcp on the main listener
	// (spec 004 #14). On by default: it is the reason the read API exists
	// in this shape.
	MCP bool
	// SweepInterval is the retention sweeper's cadence (spec 005 #3).
	// Retention changes take effect on the next pass, which is also the
	// margin a mistaken change can be corrected in (#14).
	SweepInterval time.Duration
	// RollupInterval is the statistics aggregator's cadence (spec 013 #3),
	// and with it the lag the docs publish: a closed hour reaches the
	// rollup within one of these, and the hour in progress is always
	// answered live.
	RollupInterval time.Duration
	// AdminToken authenticates everything cross-project: creating,
	// listing, deleting and restoring any project, and reading or changing
	// another project's settings (spec 005 #11). Since spec 028 it also
	// reaches the account routes, which is what makes it the documented
	// recovery when every owner's password is lost (#16). Unset by
	// default, which makes those endpoints answer 403 and leaves each
	// project's own key as the administrator of itself.
	AdminToken string
	// SessionLife is how long a browser session lasts (spec 028 #4). It
	// slides: a request seen more than a day after the last one moves the
	// expiry forward, so "about once a month" is what a person who opens
	// this daily is asked for.
	SessionLife time.Duration
	// URL is the address the operator's people actually use, when the
	// server cannot guess it (spec 028 #11). Set it behind a proxy: it is
	// the host of the setup and invitation links the server prints and
	// hands out. Read by the CLI as the server to talk to, which is why
	// one variable serves both.
	URL string
}

// DefaultMaxBodyBytes is the request body cap when unset (20 MiB).
const DefaultMaxBodyBytes = 20 * 1024 * 1024

// Sweeper cadence bounds (spec 005 #3). The floor exists because the interval
// is also the delay between passes: a sub-second value is a busy loop holding
// the writer, not a configuration.
const (
	DefaultSweepInterval = time.Hour
	MinSweepInterval     = time.Second
)

// Aggregator cadence bounds (spec 013 #3). The interval is also how long a
// closed hour waits before the rollup holds it, so the floor is there for the
// reason the sweeper's is: below a second this is a busy loop holding the
// writer, not a configuration.
const (
	DefaultRollupInterval = 5 * time.Minute
	MinRollupInterval     = time.Second
)

// Browser session lifetime (spec 028 #4). Thirty days, sliding, is "sign in
// about once", which is what a tool you open every day should ask. The floor
// is a day: below that the slide — which runs at most once a day — could not
// keep a session alive at all, so a smaller number would not be a shorter
// session but a broken one.
const (
	DefaultSessionDays = 30
	MinSessionDays     = 1
	DefaultSessionLife = DefaultSessionDays * 24 * time.Hour
)

// Response budget bounds (spec 004 #2). The floor is what a skeleton response
// needs before any payload is inlined; the ceiling is a consumer asking for
// everything, which is a legitimate ask from a script and a mistake from an
// agent — either way it is bounded.
const (
	DefaultResponseBudgetBytes = 50 * 1024
	MinResponseBudgetBytes     = 4 * 1024
	MaxResponseBudgetBytes     = 5 * 1024 * 1024
)

// knownEnv lists every TRACEPAD_* variable the binary understands.
var knownEnv = map[string]bool{
	"TRACEPAD_LISTEN":                true,
	"TRACEPAD_DATA_DIR":              true,
	"TRACEPAD_PROJECTS":              true,
	"TRACEPAD_STORE_RAW":             true,
	"TRACEPAD_MAX_BODY_BYTES":        true,
	"TRACEPAD_RESPONSE_BUDGET_BYTES": true,
	"TRACEPAD_MCP":                   true,
	"TRACEPAD_SWEEP_INTERVAL":        true,
	"TRACEPAD_ROLLUP_INTERVAL":       true,
	"TRACEPAD_ADMIN_TOKEN":           true,
	"TRACEPAD_SESSION_DAYS":          true,
	// The server reads TRACEPAD_URL too since spec 028 #11 — as the host
	// of the links it prints — but it is still the CLI's "which server",
	// which is the whole reason there is one variable and not two.
	"TRACEPAD_URL":     true,
	"TRACEPAD_API_KEY": true,
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
	budget, err := parseBytes("TRACEPAD_RESPONSE_BUDGET_BYTES", DefaultResponseBudgetBytes)
	if err != nil {
		return nil, err
	}
	if budget < MinResponseBudgetBytes || budget > MaxResponseBudgetBytes {
		return nil, fmt.Errorf("TRACEPAD_RESPONSE_BUDGET_BYTES: want between %d and %d, got %d",
			MinResponseBudgetBytes, MaxResponseBudgetBytes, budget)
	}
	mcp, err := parseOnOff("TRACEPAD_MCP", true)
	if err != nil {
		return nil, err
	}
	sweep, err := parseDuration("TRACEPAD_SWEEP_INTERVAL", DefaultSweepInterval)
	if err != nil {
		return nil, err
	}
	if sweep < MinSweepInterval {
		return nil, fmt.Errorf("TRACEPAD_SWEEP_INTERVAL: want at least %s, got %s", MinSweepInterval, sweep)
	}
	rollup, err := parseDuration("TRACEPAD_ROLLUP_INTERVAL", DefaultRollupInterval)
	if err != nil {
		return nil, err
	}
	if rollup < MinRollupInterval {
		return nil, fmt.Errorf("TRACEPAD_ROLLUP_INTERVAL: want at least %s, got %s",
			MinRollupInterval, rollup)
	}
	sessionDays, err := parseCount("TRACEPAD_SESSION_DAYS", DefaultSessionDays)
	if err != nil {
		return nil, err
	}
	if sessionDays < MinSessionDays {
		return nil, fmt.Errorf("TRACEPAD_SESSION_DAYS: want at least %d, got %d",
			MinSessionDays, sessionDays)
	}
	cfg := &Config{
		Listen:              envOr("TRACEPAD_LISTEN", ":4318"),
		DataDir:             envOr("TRACEPAD_DATA_DIR", defaultDataDir()),
		Projects:            os.Getenv("TRACEPAD_PROJECTS"),
		StoreRaw:            storeRaw,
		MaxBodyBytes:        maxBody,
		ResponseBudgetBytes: budget,
		MCP:                 mcp,
		SweepInterval:       sweep,
		RollupInterval:      rollup,
		AdminToken:          strings.TrimSpace(os.Getenv("TRACEPAD_ADMIN_TOKEN")),
		SessionLife:         time.Duration(sessionDays) * 24 * time.Hour,
		URL:                 strings.TrimSpace(os.Getenv("TRACEPAD_URL")),
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

// DisplayHost turns a listen address into a connectable host:port. Wildcard
// bind hosts (empty, 0.0.0.0, ::) are not valid connect targets, so they are
// shown as localhost.
//
// It lives here because two things print addresses and must print the same
// one: the startup banner's connection lines, and the setup link the server
// hands the operator when there is no request to take a host from (spec 028
// #11).
func DisplayHost(listen string) string {
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

// parseCount reads a plain whole number, for a setting whose unit is in its
// name (days, here) rather than in its value.
func parseCount(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: want a whole number, got %q", key, v)
	}
	return n, nil
}

// parseDuration reads a Go duration ("1h", "30m", "24h"), the spelling an
// operator already knows from every other tool that takes one.
func parseDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: want a duration such as 1h or 30m, got %q", key, v)
	}
	return d, nil
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
