package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command marked `(admin token)` runs on TRACEPAD_ADMIN_TOKEN when no key is
// set — the container's shell has that and nothing else (spec 004 #38) — and a
// key, when there is one, still wins. A command that is not marked never reads
// it: the fallback widens what the shell can reach by exactly the commands the
// usage text names.
func TestAdminCommandsFallBackToTheAdminToken(t *testing.T) {
	h := newAdminCLI(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(testAdminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		code int
		says string
	}{
		{"keys ls on the token alone", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken}, []string{"keys", "ls"}, ExitOK, ""},
		{"with a newline the shell left on it", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken + "\n"}, []string{"keys", "ls"}, ExitOK, ""},
		{"from the file a secret is mounted as", map[string]string{"TRACEPAD_ADMIN_TOKEN_FILE": file}, []string{"keys", "ls"}, ExitOK, ""},
		{"projects ls", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken}, []string{"projects", "ls", "--deleted"}, ExitOK, ""},
		{"accounts ls", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken}, []string{"accounts", "ls"}, ExitOK, ""},
		{"--key still wins", map[string]string{"TRACEPAD_ADMIN_TOKEN": "not-the-token"}, []string{"keys", "ls", "--key", testAdminToken}, ExitOK, ""},
		{"neither", nil, []string{"keys", "ls"}, ExitUsage, "no admin token: set TRACEPAD_ADMIN_TOKEN"},
		{"both spellings", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken, "TRACEPAD_ADMIN_TOKEN_FILE": file}, []string{"keys", "ls"}, ExitUsage, "both set"},
		{"a file that is not there", map[string]string{"TRACEPAD_ADMIN_TOKEN_FILE": file + ".missing"}, []string{"keys", "ls"}, ExitUsage, "TRACEPAD_ADMIN_TOKEN_FILE"},
		// Not marked: the token is never offered to a command the usage text
		// does not say takes it.
		{"traces ls", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken}, []string{"traces", "ls"}, ExitUsage, "no API key"},
		{"retention show", map[string]string{"TRACEPAD_ADMIN_TOKEN": testAdminToken}, []string{"retention", "show"}, ExitUsage, "no API key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := h.env
			h.env = map[string]string{"TRACEPAD_URL": h.url}
			for k, v := range tc.env {
				h.env[k] = v
			}
			defer func() { h.env = saved }()

			out := h.run(t.Context(), false, tc.args...)
			if out.code != tc.code {
				t.Fatalf("%v exited %d, want %d: %s", tc.args, out.code, tc.code, out.stderr)
			}
			if !strings.Contains(out.stderr, tc.says) {
				t.Errorf("stderr = %q, want it to say %q", out.stderr, tc.says)
			}
			if strings.Contains(out.stderr+out.stdout, testAdminToken) {
				t.Errorf("the token reached the output:\n%s%s", out.stdout, out.stderr)
			}
		})
	}
}

// `keys create --help`, and the error a mistyped `keys create` ends in, print
// the help of that command — not the sixty others (spec 004 #38).
func TestHelpIsTheCommandsOwn(t *testing.T) {
	h := newAdminCLI(t)
	h.env = map[string]string{"TRACEPAD_URL": h.url}

	help := h.run(t.Context(), false, "keys", "create", "--help")
	if help.code != ExitOK {
		t.Fatalf("keys create --help exited %d: %s", help.code, help.stderr)
	}
	if !strings.Contains(help.stdout, "tracepad keys create") || strings.Contains(help.stdout, "tracepad traces ls") {
		t.Errorf("keys create --help printed:\n%s\nwant the command's own synopsis alone", help.stdout)
	}
	if !strings.Contains(help.stdout, "TRACEPAD_ADMIN_TOKEN") {
		t.Errorf("keys create --help does not say where the admin token is read from:\n%s", help.stdout)
	}

	group := h.run(t.Context(), false, "keys", "--help")
	for _, want := range []string{"tracepad keys ls", "tracepad keys create", "tracepad keys rm"} {
		if group.code != ExitOK || !strings.Contains(group.stdout, want) {
			t.Errorf("keys --help (exit %d) lacks %q:\n%s", group.code, want, group.stdout)
		}
	}
	if strings.Contains(group.stdout, "tracepad traces ls") {
		t.Errorf("keys --help printed the whole text:\n%s", group.stdout)
	}

	wrong := h.run(t.Context(), false, "keys", "ls")
	if wrong.code != ExitUsage || !strings.Contains(wrong.stderr, "no admin token") {
		t.Fatalf("keys ls with no credential: exit %d, %s", wrong.code, wrong.stderr)
	}
	if !strings.Contains(wrong.stderr, "tracepad keys ls") || strings.Contains(wrong.stderr, "tracepad traces ls") {
		t.Errorf("the error ended in:\n%s\nwant keys ls's help alone", wrong.stderr)
	}

	// A subcommand that is not one names the group's.
	typo := h.run(t.Context(), false, "keys", "list")
	if typo.code != ExitUsage || !strings.Contains(typo.stderr, "tracepad keys rm") ||
		strings.Contains(typo.stderr, "tracepad traces ls") {
		t.Errorf("keys list: exit %d, stderr:\n%s", typo.code, typo.stderr)
	}

	// What has no block of its own still prints something that helps.
	if got := usageFor("nonsense"); got != Usage {
		t.Errorf("an unknown topic printed %q, want the whole text", got)
	}
}

// Every command a person can name has help of its own: no flag set is built
// under a name the usage text has no block for, which would send its errors
// back to the whole text without anyone noticing.
func TestEveryCommandHasHelpOfItsOwn(t *testing.T) {
	for command := range usageFlags(t) {
		if got := usageFor(command); got == Usage || !strings.Contains(got, "tracepad "+command) {
			t.Errorf("usageFor(%q) is not that command's help:\n%s", command, got)
		}
	}
}
