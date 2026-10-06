package config

import (
	"fmt"
	"strings"
)

// EnvKind says who reads a variable.
type EnvKind int

const (
	// EnvServer: read by the server, and set by a person.
	EnvServer EnvKind = iota
	// EnvInternal: read by the server, set by the image — not for a person.
	EnvInternal
	// EnvClient: read by the CLI and the MCP server, not by the server.
	EnvClient
	// EnvPackage: read by the Python, Node and Go packages, not by the server.
	EnvPackage
	// EnvInstaller: read by scripts/install.sh (spec 053 #10), and two of them,
	// TRACEPAD_INSTALL_DIR and TRACEPAD_DOWNLOAD_URL, by `tracepad upgrade`,
	// which installs as the script does (spec 054 #3). Listed so that one left
	// exported is not called a typo.
	EnvInstaller
)

// EnvVar is one TRACEPAD_* variable.
type EnvVar struct {
	Name string
	Kind EnvKind
	// Default and Help are what `tracepad help` prints for a server variable;
	// a newline in Help starts a continuation line.
	Default string
	Help    string
	// Flag is the flag that beats the variable, for the two that have one. Help
	// prints those with their flags, not in the list of variables.
	Flag string
}

// Env is every TRACEPAD_* variable the binaries and the packages read, and the
// one place that says so (spec 001 #24). The start-up warning for a variable
// nobody reads, the variable list of `tracepad help` and the tests that hold
// docs/configuration.md and the packages' sources to it are all derived from
// this table; a variable that is read and is not here is a variable nobody
// finds.
var Env = []EnvVar{
	{Name: "TRACEPAD_LISTEN", Flag: "--listen", Default: "localhost:4318", Help: "HTTP listen address"},
	{Name: "TRACEPAD_DATA_DIR", Flag: "--data-dir", Help: "data directory"},
	{Name: "TRACEPAD_STORE_RAW", Default: "on", Help: "keep raw OTLP bodies for export"},
	{Name: "TRACEPAD_PROJECTS", Default: "unset", Help: "projects to declare: name:pk:sk,…"},
	// The server reads TRACEPAD_URL too since spec 028 #11 — as the host of
	// the links it prints — but it is still the CLI's "which server", which
	// is the whole reason there is one variable and not two.
	{Name: "TRACEPAD_URL", Default: "unset", Help: "public address (links; CLI target)"},
	{Name: "TRACEPAD_MAX_BODY_BYTES", Default: "20971520", Help: "request body cap in bytes"},
	{Name: "TRACEPAD_MAX_SPANS_PER_REQUEST", Default: "20000", Help: "spans one export may carry"},
	{Name: "TRACEPAD_BODY_BUDGET_BYTES", Default: "4x the body cap", Help: "request bodies held in memory at once"},
	{Name: "TRACEPAD_RESPONSE_BUDGET_BYTES", Default: "51200", Help: "default read response budget"},
	{Name: "TRACEPAD_READ_TIMEOUT", Default: "20s", Help: "deadline of one read request"},
	{Name: "TRACEPAD_READ_CONCURRENCY", Default: "2 per CPU, at least 4", Help: "reads served at once"},
	{Name: "TRACEPAD_MAX_CONNECTIONS", Default: "1024", Help: "client connections held at once"},
	{Name: "TRACEPAD_MAX_CONNECTIONS_PER_SOURCE", Default: "a quarter of the above", Help: "connections one address may hold\na trusted proxy is exempt"},
	{Name: "TRACEPAD_MCP", Default: "on", Help: "serve MCP at /mcp"},
	{Name: "TRACEPAD_SWEEP_INTERVAL", Default: "1h", Help: "retention sweep cadence"},
	{Name: "TRACEPAD_ROLLUP_INTERVAL", Default: "5m", Help: "statistics rollup cadence"},
	{Name: "TRACEPAD_ADMIN_TOKEN", Default: "unset", Help: "bearer token for cross-project admin\nat least 32 characters: openssl rand -hex 32"},
	{Name: "TRACEPAD_ADMIN_TOKEN_FILE", Default: "unset", Help: "read the admin token from this file"},
	{Name: "TRACEPAD_SETUP", Default: "on", Help: "mint and print the setup link"},
	{Name: "TRACEPAD_SESSION_DAYS", Default: "30", Help: "days a browser sign-in lasts"},
	{Name: "TRACEPAD_TRUSTED_PROXIES", Default: "loopback", Help: "proxies whose X-Forwarded-* headers count\naddresses and CIDR ranges, or none"},
	{Name: "TRACEPAD_IN_CONTAINER", Kind: EnvInternal},

	{Name: "TRACEPAD_API_KEY", Kind: EnvClient},

	{Name: "TRACEPAD_ENVIRONMENT", Kind: EnvPackage},
	{Name: "TRACEPAD_RELEASE", Kind: EnvPackage},
	{Name: "TRACEPAD_EXPORT_TIMEOUT", Kind: EnvPackage},

	{Name: "TRACEPAD_VERSION", Kind: EnvInstaller},
	{Name: "TRACEPAD_INSTALL_DIR", Kind: EnvInstaller},
	{Name: "TRACEPAD_NO_SKILL", Kind: EnvInstaller},
	{Name: "TRACEPAD_NO_PLAN", Kind: EnvInstaller},
	{Name: "TRACEPAD_DOWNLOAD_URL", Kind: EnvInstaller},
}

// IsKnownEnv reports whether anything reads the variable — the server, the
// CLI or the packages. A shared .env carries all of them, so none is a typo.
func IsKnownEnv(name string) bool {
	for _, v := range Env {
		if v.Name == name {
			return true
		}
	}
	return false
}

// EnvHelp is the "Server environment" block of `tracepad help`: every
// variable a person sets on the server, except the two that print with their
// flags.
func EnvHelp() string {
	var b strings.Builder
	b.WriteString("Server environment:\n")
	for _, v := range Env {
		if v.Kind != EnvServer || v.Flag != "" {
			continue
		}
		first, rest, _ := strings.Cut(v.Help, "\n")
		fmt.Fprintf(&b, "  %-31s %-37s (default %s)\n", v.Name, first, v.Default)
		if rest != "" {
			fmt.Fprintf(&b, "  %-31s %s\n", "", rest)
		}
	}
	return b.String()
}
