package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AdminToken is how a client reads the token the way the server does (spec 004
// #38): inline or from the file, trimmed, never both.
func TestAdminTokenReadsTheEnvironmentTheServerDoes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"nothing set", nil, "", ""},
		{"inline, trimmed", map[string]string{"TRACEPAD_ADMIN_TOKEN": "  inline\n"}, "inline", ""},
		{"a file", map[string]string{"TRACEPAD_ADMIN_TOKEN_FILE": file}, "from-the-file", ""},
		{"both is a refusal", map[string]string{"TRACEPAD_ADMIN_TOKEN": "x", "TRACEPAD_ADMIN_TOKEN_FILE": file}, "", "both set"},
		{"a file that is not there", map[string]string{"TRACEPAD_ADMIN_TOKEN_FILE": file + ".missing"}, "", "TRACEPAD_ADMIN_TOKEN_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AdminToken(env(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to say %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("AdminToken = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}
