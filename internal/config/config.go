// Package config resolves Tracepad configuration from environment variables
// and command-line flags. Flags override environment (spec 001).
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Listen is the HTTP listen address. Defaults to localhost:4318: the
	// OTLP port, so that unconfigured OTel SDK exporters (default endpoint
	// localhost:4318) reach a locally running Tracepad (spec 001 #2), on
	// both loopback addresses, so that nothing else does until the operator
	// says so (#22, #23). The image sets :4318 itself.
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
	// MaxSpansPerRequest is the most spans one export may carry (spec 043
	// #10); an export with more is refused whole with `413`.
	MaxSpansPerRequest int
	// BodyBudgetBytes bounds the request bodies held in memory at once,
	// counted decompressed (spec 043 #13); a request whose next step does
	// not fit is `429`. Zero means four times MaxBodyBytes.
	BodyBudgetBytes int64
	// ResponseBudgetBytes is the default byte budget a read response
	// spends on payloads (spec 004 #2). The consumer's context window is
	// a scarce resource, so the API respects it by default rather than on
	// request.
	ResponseBudgetBytes int64
	// ReadTimeout is the deadline of one read request, its wait for a read
	// slot included (spec 043 #15): a read the deadline stops is `503`.
	ReadTimeout time.Duration
	// ReadConcurrency is how many reads are served at once (spec 043 #16);
	// a read that gets no slot before its deadline is `503` "busy".
	ReadConcurrency int
	// MaxConnections is how many client connections the listener holds at
	// once (spec 043 #45): past it, a new connection waits in the kernel's
	// backlog, and an idle keep-alive is closed to make room for it.
	MaxConnections int
	// MaxConnectionsPerSource bounds one source's share of them, a trusted
	// proxy exempt (spec 043 #45).
	MaxConnectionsPerSource int
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
	//
	// Read from TRACEPAD_ADMIN_TOKEN or from the file TRACEPAD_ADMIN_TOKEN_FILE
	// names, never both, and at least MinSecretLength characters (spec 001
	// #18): it creates owner accounts, so it is the deployment.
	AdminToken string
	// SetupDisabled is TRACEPAD_SETUP=off: no setup token is minted and
	// `POST /api/v1/setup` refuses, for a deployment whose first owner is
	// made with the admin token (spec 028 #32).
	SetupDisabled bool
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
	// InContainer says the process runs in the published image, which sets
	// TRACEPAD_IN_CONTAINER=1 (spec 001 #12, spec 020 #20). There a wildcard
	// bind is the design and the port's reach is decided by where it is
	// published, which the process cannot see — so the plain-HTTP notice at
	// start is information, not a warning.
	InContainer bool
	// TrustedProxies is TRACEPAD_TRUSTED_PROXIES as written, checked at
	// Load: the peers whose X-Forwarded-For is believed when the server
	// works out where a request comes from (spec 046 #1, #2). Empty is the
	// default, loopback, and `none` trusts no peer. Kept as the text rather
	// than as a list, so that "the default" and "nobody" cannot be told
	// apart by whether a slice is nil — a difference any copy loses (#17).
	// ParseTrustedProxies is the one reading of it.
	TrustedProxies string
}

// DefaultMaxBodyBytes is the request body cap when unset (20 MiB).
const DefaultMaxBodyBytes = 20 * 1024 * 1024

// Ingest bounds (spec 043 #10, #13, #22). 20,000 spans is far above the 512
// an OpenTelemetry batch processor sends and the Collector's default batch of
// 8,192; the body budget is four full bodies, and never less than one, since a
// budget below the cap would refuse a body the cap admits even on an idle
// server.
const (
	DefaultMaxSpansPerRequest = 20000
	MinMaxSpansPerRequest     = 1
	bodyBudgetBodies          = 4
)

// DefaultBodyBudgetBytes is the body budget for a body cap: four bodies, held
// at the largest int64 for a cap so large that four of it would not fit one.
func DefaultBodyBudgetBytes(maxBody int64) int64 {
	if maxBody > math.MaxInt64/bodyBudgetBodies {
		return math.MaxInt64
	}
	return bodyBudgetBodies * maxBody
}

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

// Read bounds (spec 043 #15, #16, #22). The deadline answers before the
// interface's own 30-second clock gives up, so a person sees the server's
// reason rather than a network error; its floor is a second, below which an
// ordinary listing of a busy project would be refused. Its ceiling is
// under the five minutes the server gives a response to be written
// (spec 001 #15): past that the transport drops the connection, and the
// deadline's answer would never arrive (spec 043 #28). Reads are CPU-bound, so
// the default concurrency is twice the processors, and at least four so that
// a dashboard's burst of questions on a small machine is served together.
const (
	DefaultReadTimeout    = 20 * time.Second
	MinReadTimeout        = time.Second
	MaxReadTimeout        = 4 * time.Minute
	MinReadConcurrency    = 1
	minDefaultConcurrency = 4
)

// DefaultMaxConnections and MinMaxConnections bound the connections the
// listener holds (spec 043 #45). A browser tab holds six, an exporter one or
// two; a thousand is a team's worth of both, and its goroutines and buffers
// are a few tens of megabytes. Fewer than sixteen would let one open interface
// and a busy exporter starve each other.
const (
	DefaultMaxConnections = 1024
	MinMaxConnections     = 16
)

// DefaultMaxConnectionsPerSource is one source's share of max connections: a
// quarter, so that one address cannot hold the rest in the backlog, and still
// a few hundred by default for an office or a fleet behind one NAT.
func DefaultMaxConnectionsPerSource(maxConnections int) int {
	return max(1, maxConnections/4)
}

// DefaultReadConcurrency is the read slots of a machine with this many
// processors: twice as many, at least four.
func DefaultReadConcurrency(procs int) int {
	return max(minDefaultConcurrency, 2*procs)
}

// MinSecretLength is the shortest admin token or declared project secret the
// server starts with (spec 001 #18). A wrong one is refused in well under a
// millisecond, so what stops guessing is the size of the space: thirty-two
// hex characters is 128 bits, which thousands of guesses a second on any
// number of connections do not exhaust. `openssl rand -hex 32` is 256.
const MinSecretLength = 32

// SecretLength is a secret's length as MinSecretLength and every refusal count
// it: in characters, not bytes — so the rule, the message and the check say
// the same thing, and a secret outside plain ASCII is held to at least as many
// bytes as a hex one.
func SecretLength(secret string) int { return utf8.RuneCountInString(secret) }

// GenerateHint is how every short-secret refusal ends: the admin token's
// here, and a declared project secret's at the start, where the store says
// whether the declaration creates a project (spec 001 #18).
const GenerateHint = "generate one with: openssl rand -hex 32"

// deprecatedEnv names the variables that were read once, are not any more, and
// still turn up in a shared .env: the server says what replaced them at Info,
// not as the typo warning an unknown name gets.
var deprecatedEnv = map[string]string{
	// The packages' name for the store's address before TRACEPAD_URL (spec
	// 017 #21). The server never read it; a shared .env carries it here.
	"TRACEPAD_HOST": "TRACEPAD_URL",
}

// IsDeprecatedEnv reports whether the variable was read once and is not any
// more (see deprecatedEnv).
func IsDeprecatedEnv(name string) bool {
	_, ok := deprecatedEnv[name]
	return ok
}

// flagTargets are the flags of the server and what each one sets, in one
// table: ParseFlags registers them from it and FromEnv applies them from it, so
// a new flag is a new row and cannot be registered and then left unapplied (a
// test holds every row to changing the configuration).
var flagTargets = []struct {
	name, usage string
	set         func(*Config, string)
}{
	{"listen", "HTTP listen address", func(c *Config, v string) { c.Listen = v }},
	{"data-dir", "data directory (database, payloads)", func(c *Config, v string) { c.DataDir = v }},
}

// Flags are the command line's part of the configuration: the flags that were
// given, and nothing else.
type Flags struct{ given map[string]string }

// ParseFlags reads the server's flags, and nothing from the environment. It is
// the first of two steps (the second is FromEnv) so that the caller can write
// the server's first log line between them (spec 001 #25): after the
// arguments are known to be a start — not a request for help, in any spelling
// the flag package has for one, not a flag the server does not have — and
// before the environment can say anything. The FlagSet decides what is help,
// so `--data-dir -h` is a data directory called `-h`.
func ParseFlags(args []string) (*Flags, error) {
	fs := flag.NewFlagSet("tracepad", flag.ContinueOnError)
	// The caller owns user-facing usage output; suppress the FlagSet's own
	// printing and let the returned error (flag.ErrHelp included) drive it.
	fs.SetOutput(io.Discard)
	for _, t := range flagTargets {
		fs.String(t.name, "", t.usage)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	flags := &Flags{given: map[string]string{}}
	fs.Visit(func(f *flag.Flag) { flags.given[f.Name] = f.Value.String() })
	return flags, nil
}

// Load resolves configuration from the given flag arguments and the
// environment.
func Load(args []string) (*Config, error) {
	flags, err := ParseFlags(args)
	if err != nil {
		return nil, err
	}
	return FromEnv(flags)
}

// FromEnv resolves the configuration from the environment, with the flags laid
// over it: a flag that was given wins, one that was not leaves the
// environment's value, and that the default, alone.
func FromEnv(flags *Flags) (*Config, error) {
	storeRaw, err := parseOnOff("TRACEPAD_STORE_RAW", true)
	if err != nil {
		return nil, err
	}
	maxBody, err := parseBytes("TRACEPAD_MAX_BODY_BYTES", DefaultMaxBodyBytes)
	if err != nil {
		return nil, err
	}
	maxSpans, err := parseCount("TRACEPAD_MAX_SPANS_PER_REQUEST", DefaultMaxSpansPerRequest)
	if err != nil {
		return nil, err
	}
	if maxSpans < MinMaxSpansPerRequest {
		return nil, fmt.Errorf("TRACEPAD_MAX_SPANS_PER_REQUEST: want at least %d, got %d",
			MinMaxSpansPerRequest, maxSpans)
	}
	bodyBudget, err := parseBytes("TRACEPAD_BODY_BUDGET_BYTES", DefaultBodyBudgetBytes(maxBody))
	if err != nil {
		return nil, err
	}
	if bodyBudget < maxBody {
		return nil, fmt.Errorf("TRACEPAD_BODY_BUDGET_BYTES: want at least TRACEPAD_MAX_BODY_BYTES (%d), got %d",
			maxBody, bodyBudget)
	}
	budget, err := parseBytes("TRACEPAD_RESPONSE_BUDGET_BYTES", DefaultResponseBudgetBytes)
	if err != nil {
		return nil, err
	}
	if budget < MinResponseBudgetBytes || budget > MaxResponseBudgetBytes {
		return nil, fmt.Errorf("TRACEPAD_RESPONSE_BUDGET_BYTES: want between %d and %d, got %d",
			MinResponseBudgetBytes, MaxResponseBudgetBytes, budget)
	}
	readTimeout, err := parseDuration("TRACEPAD_READ_TIMEOUT", DefaultReadTimeout)
	if err != nil {
		return nil, err
	}
	if readTimeout < MinReadTimeout || readTimeout > MaxReadTimeout {
		return nil, fmt.Errorf("TRACEPAD_READ_TIMEOUT: want between %s and %s, got %s",
			MinReadTimeout, MaxReadTimeout, readTimeout)
	}
	readConcurrency, err := parseCount("TRACEPAD_READ_CONCURRENCY", DefaultReadConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		return nil, err
	}
	if readConcurrency < MinReadConcurrency {
		return nil, fmt.Errorf("TRACEPAD_READ_CONCURRENCY: want at least %d, got %d",
			MinReadConcurrency, readConcurrency)
	}
	maxConnections, err := parseCount("TRACEPAD_MAX_CONNECTIONS", DefaultMaxConnections)
	if err != nil {
		return nil, err
	}
	if maxConnections < MinMaxConnections {
		return nil, fmt.Errorf("TRACEPAD_MAX_CONNECTIONS: want at least %d, got %d",
			MinMaxConnections, maxConnections)
	}
	perSource, err := parseCount("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE", DefaultMaxConnectionsPerSource(maxConnections))
	if err != nil {
		return nil, err
	}
	if perSource < 1 || perSource > maxConnections {
		return nil, fmt.Errorf("TRACEPAD_MAX_CONNECTIONS_PER_SOURCE: want between 1 and TRACEPAD_MAX_CONNECTIONS (%d), got %d",
			maxConnections, perSource)
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
	inContainer, err := parseOnOff("TRACEPAD_IN_CONTAINER", false)
	if err != nil {
		return nil, err
	}
	sessionDays, err := parseCount("TRACEPAD_SESSION_DAYS", DefaultSessionDays)
	if err != nil {
		return nil, err
	}
	if sessionDays < MinSessionDays {
		return nil, fmt.Errorf("TRACEPAD_SESSION_DAYS: want at least %d, got %d",
			MinSessionDays, sessionDays)
	}
	adminToken, err := readAdminToken()
	if err != nil {
		return nil, err
	}
	setup, err := parseOnOff("TRACEPAD_SETUP", true)
	if err != nil {
		return nil, err
	}
	trusted := strings.TrimSpace(os.Getenv("TRACEPAD_TRUSTED_PROXIES"))
	if _, err := ParseTrustedProxies(trusted); err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:                  envOr("TRACEPAD_LISTEN", DefaultListen),
		DataDir:                 envOr("TRACEPAD_DATA_DIR", defaultDataDir()),
		Projects:                os.Getenv("TRACEPAD_PROJECTS"),
		StoreRaw:                storeRaw,
		MaxBodyBytes:            maxBody,
		MaxSpansPerRequest:      maxSpans,
		BodyBudgetBytes:         bodyBudget,
		ResponseBudgetBytes:     budget,
		ReadTimeout:             readTimeout,
		ReadConcurrency:         readConcurrency,
		MaxConnections:          maxConnections,
		MaxConnectionsPerSource: perSource,
		MCP:                     mcp,
		SweepInterval:           sweep,
		RollupInterval:          rollup,
		AdminToken:              adminToken,
		SetupDisabled:           !setup,
		SessionLife:             time.Duration(sessionDays) * 24 * time.Hour,
		URL:                     strings.TrimSpace(os.Getenv("TRACEPAD_URL")),
		InContainer:             inContainer,
		TrustedProxies:          trusted,
	}

	if flags != nil {
		for _, t := range flagTargets {
			if v, ok := flags.given[t.name]; ok {
				t.set(cfg, v)
			}
		}
	}

	warnUnknownEnv()
	return cfg, nil
}

// loopbackProxies is what `loopback`, the default of TRACEPAD_TRUSTED_PROXIES,
// stands for (spec 046 #1): a proxy on the same host, which is the deployment
// the docs recommend outside a container. A TCP relay on loopback is the
// exception the docs name (#16). A fresh list each time, so that no caller
// can change the default by appending to it (#18).
func loopbackProxies() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
}

// ParseTrustedProxies reads TRACEPAD_TRUSTED_PROXIES (spec 046 #1): a
// comma-separated list of addresses and CIDR ranges, or one of two words —
// `loopback`, the default when the value is empty, and `none`, which trusts no
// peer and is an empty list. `loopback` may also stand beside other entries.
// An entry that does not parse refuses to start, named with its position: the
// list decides whose word the server takes for a client's address, and a typo
// that silently dropped a proxy would put every client behind it into one
// bucket. So does a range that holds every address — 0.0.0.0/0, ::/0, or
// ::ffff:0:0/96, which is 0.0.0.0/0 written for IPv6 — since trusting every
// peer lets every client name its own source, and the limit is off (#17).
func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "loopback") {
		return loopbackProxies(), nil
	}
	if strings.EqualFold(value, "none") {
		return []netip.Prefix{}, nil
	}
	var list []netip.Prefix
	for i, raw := range strings.Split(value, ",") {
		entry := strings.TrimSpace(raw)
		switch {
		case strings.EqualFold(entry, "loopback"):
			list = append(list, loopbackProxies()...)
			continue
		case strings.EqualFold(entry, "none"):
			return nil, fmt.Errorf("TRACEPAD_TRUSTED_PROXIES: entry %d: none trusts no peer, so it stands alone", i+1)
		case entry == "":
			return nil, fmt.Errorf("TRACEPAD_TRUSTED_PROXIES: entry %d is empty", i+1)
		}
		prefix, err := parseProxyEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("TRACEPAD_TRUSTED_PROXIES: entry %d, %q: want an IP address such as 172.17.0.1, or a CIDR range such as 10.0.0.0/24",
				i+1, entry)
		}
		if prefix.Bits() < widestProxyRange(prefix.Addr()) {
			return nil, fmt.Errorf("TRACEPAD_TRUSTED_PROXIES: entry %d, %q: wider than any one network's proxies (at most /%d), "+
				"so clients could name their own sources and the limit on password checks would be off; name the proxies themselves",
				i+1, entry, widestProxyRange(prefix.Addr()))
		}
		list = append(list, prefix)
	}
	return list, nil
}

// widestProxyRange is the shortest prefix a trusted range may have (spec 046
// #18): an IPv4 /8, the largest block one organisation holds, and an IPv6
// /16. Cloudflare's widest ranges are a /13 and a /29. A wider one trusts
// peers no deployment's proxies are among, and a list of them — 0.0.0.0/1 and
// 128.0.0.0/1 — trusts every peer, which switches the limit off.
func widestProxyRange(addr netip.Addr) int {
	if addr.Is4() {
		return 8
	}
	return 16
}

// parseProxyEntry is one address or range, masked to its network: a range
// written with host bits set (172.17.0.1/16) means the network it names, as
// every firewall reads it.
func parseProxyEntry(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, err
		}
		if prefix.Addr().Is4In6() {
			bits := prefix.Bits() - 96
			if bits < 0 {
				return netip.Prefix{}, errors.New("an IPv4-mapped range shorter than ::ffff:0:0/96")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), bits)
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, errors.New("an address with a zone")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// readAdminToken reads the admin token from TRACEPAD_ADMIN_TOKEN or from the
// file TRACEPAD_ADMIN_TOKEN_FILE names (spec 001 #18). The file is how a secret
// reaches a container without passing through the environment every `docker
// inspect` prints — a mounted Docker or Kubernetes secret. Both set is a
// refusal rather than a precedence rule: which one the operator meant is not
// something to guess about the credential that owns the deployment.
//
// Whitespace around the value is dropped, as a shell or an editor leaves a
// newline behind. The value is never quoted in an error: it is the secret.
func readAdminToken() (string, error) {
	token, err := AdminToken(os.Getenv)
	if err != nil {
		return "", err
	}
	name := "TRACEPAD_ADMIN_TOKEN"
	if strings.TrimSpace(os.Getenv("TRACEPAD_ADMIN_TOKEN_FILE")) != "" {
		name = "TRACEPAD_ADMIN_TOKEN_FILE"
	}
	if token != "" && SecretLength(token) < MinSecretLength {
		return "", fmt.Errorf("%s: the admin token is %d characters; it creates owner accounts, "+
			"so it must be at least %d — %s", name, SecretLength(token), MinSecretLength, GenerateHint)
	}
	return token, nil
}

// AdminToken is the admin token as the environment getenv reads holds it:
// TRACEPAD_ADMIN_TOKEN, or the contents of the file TRACEPAD_ADMIN_TOKEN_FILE
// names, trimmed; "" when neither is set. Both set is a refusal, since which one
// the operator meant is not something to guess about the credential that owns
// the deployment. It is the one reading: the server's start (readAdminToken,
// which adds the length rule) and the CLI's commands that take the token both
// go through it. It checks no length — that is the server's rule for a token it
// will accept, not a client's for one it presents — and never quotes the value.
func AdminToken(getenv func(string) string) (string, error) {
	inline := strings.TrimSpace(getenv("TRACEPAD_ADMIN_TOKEN"))
	if strings.TrimSpace(getenv("TRACEPAD_ADMIN_TOKEN_FILE")) == "" {
		return inline, nil
	}
	if inline != "" {
		return "", errors.New("TRACEPAD_ADMIN_TOKEN and TRACEPAD_ADMIN_TOKEN_FILE are both set; set one")
	}
	return AdminTokenFile(getenv)
}

// maxAdminTokenFile is the most TRACEPAD_ADMIN_TOKEN_FILE may hold. A token is
// sixty-four characters and a newline; a file far past that is a mount that
// went to the wrong place, and reading it whole could be reading forever.
const maxAdminTokenFile = 4 * 1024

// AdminTokenFile reads the admin token from the file TRACEPAD_ADMIN_TOKEN_FILE
// names, trimmed, or "" when the variable is unset. The file must be a regular
// one — a symbolic link to one is followed, which is how Kubernetes mounts a
// secret — of at most 4 KiB, and not empty: a device, a directory or a large
// file is a path that went wrong, and an error says so instead of reading
// /dev/zero until memory runs out. getenv is how the caller reads its
// environment: the server's start (readAdminToken) and the CLI's export
// guard, which must know every key of this Tracepad the machine holds, read
// the file the one same way.
func AdminTokenFile(getenv func(string) string) (string, error) {
	path := strings.TrimSpace(getenv("TRACEPAD_ADMIN_TOKEN_FILE"))
	if path == "" {
		return "", nil
	}
	// Opened without waiting, and asked what it is through what was opened:
	// a plain open of a named pipe waits for somebody to write to it, and a
	// start that waits for ever says nothing at all. Asking by name first
	// and opening afterwards would leave a moment for the name to become a
	// pipe in between.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAdminTokenFile+1))
	if err != nil {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %w", err)
	}
	if len(raw) > maxAdminTokenFile {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %s is larger than %d bytes; it should hold the token alone",
			path, maxAdminTokenFile)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("TRACEPAD_ADMIN_TOKEN_FILE: %s is empty", path)
	}
	return token, nil
}

// DBPath returns the path of the SQLite database file.
func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "tracepad.db")
}

// DefaultListen is where the server listens when nothing says otherwise: the
// OTLP port, on this machine only (spec 001 #22). The server binds a
// `localhost` host as both loopback addresses, 127.0.0.1 and ::1 (#23), so
// that the name the OTel default and the docs use reaches it whichever
// family a client tries first.
const DefaultListen = "localhost:4318"

// DisplayHost turns a listen address into a connectable host:port. Wildcard
// bind hosts (empty, 0.0.0.0, ::) are not valid connect targets, so they are
// shown as localhost. A loopback address is shown as itself: bound alone, it
// is the one family a client must use (spec 001 #23).
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

// PlainHTTPBeyondLoopback reports whether a server listening on listen takes
// passwords, session cookies and keys in clear from other machines (spec 001
// #12). Tracepad serves no TLS itself, so the listener is plain HTTP whatever
// it binds; what decides is whether anything but this machine can reach it.
//
// TRACEPAD_URL is not consulted (#22): an https one (HTTPSURL) says where
// people are meant to connect, not that nobody can connect here directly,
// and the server has no way to check it. It changes what the warning says,
// not whether there is one.
//
// A hostname other than localhost counts as reachable: it names some
// interface, and which one is the resolver's business, not a thing to guess
// at here.
func PlainHTTPBeyondLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// HTTPSURL reports whether TRACEPAD_URL names an https address: the
// operator's statement that people reach this server through a TLS proxy
// (spec 001 #12). The start's plain-HTTP warning reads it for its wording
// (#22).
func HTTPSURL(publicURL string) bool {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	return err == nil && strings.EqualFold(u.Scheme, "https")
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
		if now, ok := deprecatedEnv[name]; ok {
			slog.Info("deprecated environment variable, not read by the server", "name", name, "use", now)
		} else if strings.HasPrefix(name, "TRACEPAD_") && !IsKnownEnv(name) {
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
	for i, part := range strings.Split(raw, ",") {
		fields := strings.Split(strings.TrimSpace(part), ":")
		// The entry is named by its position, never quoted: a malformed
		// one is usually a secret with a stray colon in it, and this error
		// ends up in the log (spec 001 #12).
		if len(fields) != 3 {
			return nil, fmt.Errorf("TRACEPAD_PROJECTS entry %d: want name:public_key:secret_key, got %d fields",
				i+1, len(fields))
		}
		for f, value := range fields {
			if value == "" {
				return nil, fmt.Errorf("TRACEPAD_PROJECTS entry %d: want name:public_key:secret_key, field %d is empty",
					i+1, f+1)
			}
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
