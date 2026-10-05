package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The upgrade's `state` is data the procedure wrote, and one of its values,
// the version, is whatever the server's /health answered (spec 053 #23 (h)).
// The check and the way back read it field by field, each against the shape
// it was written in, and never run it: these run both blocks, as the
// reference has them, on states that carry a command in a value, and on one
// that is another run's.
func TestUpgradeStateIsReadAsData(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("tracepad", "references", "upgrade.md"))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, m := range regexp.MustCompile("(?s)```sh\n(.*?)```").FindAllStringSubmatch(string(src), -1) {
		if strings.Contains(m[1], "\nget() {") {
			blocks = append(blocks, m[1])
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("%d blocks read the state with get(), want the check and the way back", len(blocks))
	}
	getLine := regexp.MustCompile(`(?m)^get\(\) \{.*$`)
	if a, b := getLine.FindString(blocks[0]), getLine.FindString(blocks[1]); a != b {
		t.Fatalf("the check and the way back read the state differently:\n%s\n%s", a, b)
	}

	const run = "20261005-000000-0.1.0-AbCdEf"
	valid := map[string]string{
		"run": run, "how": "binary", "data": "/srv/trace pad", "listen": "localhost:9",
		"from": "0.1.0-rc.1", "to": "0.1.0", "archived": "yes",
	}
	// Each case goes to the blocks that read its field: the check reads no
	// `from`, the way back no `to`. The address is a closed port, so nothing
	// here reaches a server.
	cases := []struct {
		name, field, value string
		blocks             []int
	}{
		{"a command after the version", "from", "1.0.0;touch MARKER", []int{1}},
		{"a substitution in the version", "to", "$(touch MARKER)", []int{0}},
		{"backquotes in the address", "listen", "localhost:`touch MARKER`", []int{0, 1}},
		{"a command after the data directory", "data", "/srv/x;touch MARKER", []int{0}},
		{"a substitution in the run", "run", run + "$(touch MARKER)", []int{0, 1}},
		{"another run's state", "run", "20260901-000000-0.1.0-ZzZzZz", []int{0, 1}},
	}
	for _, tc := range cases {
		for _, i := range tc.blocks {
			block := blocks[i]
			t.Run(tc.name, func(t *testing.T) {
				home := t.TempDir()
				bk := filepath.Join(home, "tracepad-backups", run)
				if err := os.MkdirAll(bk, 0o700); err != nil {
					t.Fatal(err)
				}
				var state strings.Builder
				for _, k := range []string{"run", "how", "data", "listen", "from", "to", "archived"} {
					v := valid[k]
					if k == tc.field {
						v = tc.value
					}
					state.WriteString(k + "=" + v + "\n")
				}
				if err := os.WriteFile(filepath.Join(bk, "state"), []byte(state.String()), 0o600); err != nil {
					t.Fatal(err)
				}
				code := strings.Replace(block, "bk=/path/printed/by/step/3;", "bk="+bk+";", 1)
				cmd := exec.Command("sh", "-c", code)
				cmd.Dir, cmd.Env = home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "STOP") {
					t.Errorf("block %d went on (%v):\n%s", i, err, out)
				}
				if _, err := os.Stat(filepath.Join(home, "MARKER")); err == nil {
					t.Errorf("block %d ran the command in %s", i, tc.field)
				}
			})
		}
	}

	// The control: a state of the right shape gets past the reading, to the
	// way back's first check of the archive, which this run never wrote.
	home := t.TempDir()
	bk := filepath.Join(home, "tracepad-backups", run)
	if err := os.MkdirAll(bk, 0o700); err != nil {
		t.Fatal(err)
	}
	var state strings.Builder
	for _, k := range []string{"run", "how", "data", "listen", "from", "to", "archived"} {
		state.WriteString(k + "=" + valid[k] + "\n")
	}
	if err := os.WriteFile(filepath.Join(bk, "state"), []byte(state.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", strings.Replace(blocks[1], "bk=/path/printed/by/step/3;", "bk="+bk+";", 1))
	cmd.Dir, cmd.Env = home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "data.tar.gz does not read") {
		t.Errorf("a valid state did not reach the archive check:\n%s", out)
	}
}
