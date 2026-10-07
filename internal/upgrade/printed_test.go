package upgrade

import (
	"bytes"
	"encoding/csv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The commands the plan prints are pasted into a person's shell, and the
// values in them come from elsewhere: a version a server answered, a
// container's labels, a unit's name, a path (the second review of #228).
// Each is run here by sh, with stand-ins for the programs it calls, and with
// values a shell would read as code: what each program gets must be exactly
// the value, and nothing in a value may run.

// hostile is a value with everything a shell reads: quotes, a substitution,
// a backquote, a space, a newline, a glob, a separator. What it would run
// makes a file called canary in the directory it runs in.
func hostile(base string) string {
	return base + `'it's "$(touch canary)" ` + "`touch canary`" + ` * ; a\b` + "\nline"
}

// printedRun runs line with sh in a directory of its own — named with a colon
// and a comma, which docker's -v and --mount would read as separators (the
// ninth review of #228) — with stand-ins for docker, systemctl, sudo,
// launchctl and curl that record their arguments, and answers each call's
// argv in order.
func printedRun(t *testing.T, line string, env ...string) [][]string {
	t.Helper()
	stubs, log, cwd := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "backups:2026,a")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	record := `#!/bin/sh
n=$(ls "$LOG" | wc -l | tr -d ' ')
{ printf '%s\0' "$(basename "$0")"; for a; do printf '%s\0' "$a"; done; } > "$LOG/$n"
`
	for _, name := range []string{"docker", "systemctl", "launchctl", "curl"} {
		script := record
		switch name {
		case "docker":
			// What `docker inspect --format …Env…` answers.
			script += `[ "$1" = inspect ] && printf 'TRACEPAD_URL=http://x\n'` + "\n"
		case "curl":
			// An install script that records the environment it runs in.
			script += `printf '%s\n' 'printf "%s\0" "$TRACEPAD_VERSION" "$TRACEPAD_INSTALL_DIR" > "$LOG/999"'` + "\n"
		}
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script+"exit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// sudo runs what it is given, recorded as itself.
	if err := os.WriteFile(filepath.Join(stubs, "sudo"), []byte(record+`exec "$@"`+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = cwd
	cmd.Env = append([]string{"PATH=" + stubs + ":/usr/bin:/bin", "LOG=" + log, "HOME=" + cwd}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s\n%s", err, out, line)
	}
	if _, err := os.Stat(filepath.Join(cwd, "canary")); err == nil {
		t.Errorf("a printed command ran a value as code: %s", line)
	}
	entries, _ := os.ReadDir(log)
	sort.Slice(entries, func(i, j int) bool {
		a, _ := strconv.Atoi(entries[i].Name())
		b, _ := strconv.Atoi(entries[j].Name())
		return a < b
	})
	var calls [][]string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(log, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"))
	}
	return calls
}

func sameArgs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestPrintedCommandsPassValuesWhole(t *testing.T) {
	t.Parallel()
	v := hostile

	t.Run("a container's", func(t *testing.T) {
		c := Container{Name: v("name"), Repo: "ghcr.io/tracepad/tracepad", Version: v("0.1.0"),
			DataMount: dataMount("bind", v("/srv/data")+",b"),
			Run:       []string{"-p", "127.0.0.1:4318:4318", "--label", v("label="), imageSlot, "serve", v("arg")},
			EnvNames:  []string{"TRACEPAD_URL", "OTHER_1"}}
		chain, _ := containerSteps(c, "0.2.0")
		calls := printedRun(t, chain)
		if len(calls) != 6 {
			t.Fatalf("%d calls: %q", len(calls), calls)
		}
		sameArgs(t, "stop", calls[0], []string{"docker", "stop", c.Name})
		// A version that is not a release's is not in the archive's name.
		// No directory of the person's is mounted: the archive is their
		// shell's file, wherever it is run from.
		sameArgs(t, "backup", calls[1], []string{"docker", "run", "--rm", "--mount", c.DataMount, busybox, "tar", "czf", "-", "-C", "/data", "."})
		// Docker reads the value as CSV: the source, a comma and quotes in
		// it, is one field.
		if f, err := csv.NewReader(strings.NewReader(calls[1][4])).Read(); err != nil || len(f) != 4 || f[1] != "src="+v("/srv/data")+",b" {
			t.Errorf("the mount reads as %q (%v)", f, err)
		}
		sameArgs(t, "pull", calls[2], []string{"docker", "pull", "ghcr.io/tracepad/tracepad:0.2.0"})
		sameArgs(t, "rename", calls[3], []string{"docker", "rename", c.Name, c.Name + "-old"})
		sameArgs(t, "inspect", calls[4], []string{"docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", c.Name + "-old"})
		sameArgs(t, "run", calls[5], []string{"docker", "run", "-d", "--name", c.Name, "--env-file", c.Name + ".upgrade.env",
			"-p", "127.0.0.1:4318:4318", "--label", c.Run[3], "ghcr.io/tracepad/tracepad:0.2.0", "serve", c.Run[6]})
		// A variable whose name an env file cannot carry is not written into
		// a run.
		ic := inspectContainer{}
		ic.Config.Env = []string{v("ENV") + "=x"}
		if _, _, err := createdAs(ic, imageConfig{}, ""); err == nil || !strings.Contains(err.Error(), "an env file cannot carry") {
			t.Errorf("a variable named %q: %v", v("ENV"), err)
		}
		sameArgs(t, "the archive of a release's version", []string{backupStep(Container{Name: "n", Version: "0.1.0", DataMount: dataMount("volume", "v")})},
			[]string{`(umask 077 && set -C && if [ -e n-0.1.0.tar.gz ]; then echo 'an archive of that name is there already' >&2; exit 1; fi && rm -f n-0.1.0.tar.gz.part && docker run --rm --mount type=volume,src=v,dst=/data,readonly ` + busybox + ` tar czf - -C /data . > n-0.1.0.tar.gz.part && mv n-0.1.0.tar.gz.part n-0.1.0.tar.gz || { rm -f n-0.1.0.tar.gz.part; exit 1; })`})
	})

	t.Run("a Compose project's", func(t *testing.T) {
		c := Container{Name: v("name"), Repo: "ghcr.io/tracepad/tracepad", Version: "0.1.0", DataMount: dataMount("volume", v("vol")),
			Compose: v("proj"), Service: v("svc"), ComposeDir: v("/srv/dir"),
			ComposeFiles: []string{v("/etc/a.yml"), v("/etc/b.yml")}, ComposeEnv: []string{v("/srv/a.env")}}
		chain, up, start := composeSteps(c, "0.2.0")
		calls := printedRun(t, chain+" && "+up+" && "+start)
		compose := []string{"docker", "compose", "-p", c.Compose, "--project-directory", c.ComposeDir, "--env-file", c.ComposeEnv[0], "-f", c.ComposeFiles[0], "-f", c.ComposeFiles[1]}
		if len(calls) != 5 {
			t.Fatalf("%d calls: %q", len(calls), calls)
		}
		sameArgs(t, "stop", calls[0], append(compose, "stop", c.Service))
		sameArgs(t, "backup's mount", calls[1][3:5], []string{"--mount", c.DataMount})
		sameArgs(t, "backup's command", calls[1][5:], []string{busybox, "tar", "czf", "-", "-C", "/data", "."})
		sameArgs(t, "up", calls[3], append(compose, "up", "-d", c.Service))
		sameArgs(t, "start", calls[4], append(compose, "start", c.Service))
	})

	t.Run("a service's", func(t *testing.T) {
		unit := v("tracepad.service")
		uid, err := exec.Command("id", "-u").Output()
		if err != nil {
			t.Fatal(err)
		}
		for manager, want := range map[string][]string{
			"the user systemd unit " + unit: {"systemctl", "--user", "restart", unit},
			"the systemd unit " + unit:      {"systemctl", "restart", unit},
			"the launchd job " + unit:       {"launchctl", "kickstart", "-k", "gui/" + strings.TrimSpace(string(uid)) + "/" + unit},
		} {
			calls := printedRun(t, serviceRestart(manager))
			sameArgs(t, manager, calls[len(calls)-1], want)
		}
	})

	t.Run("the install script's", func(t *testing.T) {
		dir := v("/opt/my bin")
		r := &runner{deps: Deps{Home: "/home/u"}}
		// The stand-in curl answers a script that records what sh gets.
		calls := printedRun(t, r.installLine(dir, "0.2.0"))
		sameArgs(t, "the install line's environment", calls[len(calls)-1], []string{"0.2.0", dir})
	})
}

// shq is what every printed value goes through: sh gives back the word.
func TestShqIsAWord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, w := range []string{"", "plain", "a b", hostile("x"), "'", "''", `\'`, "-n", "\n"} {
		cmd := exec.Command("sh", "-c", "printf '%s' "+shq(w))
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil || !bytes.Equal(out, []byte(w)) {
			t.Errorf("shq(%q): sh gave back %q (%v)", w, out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "canary")); err == nil {
		t.Errorf("shq let a value run")
	}
}

// outsideQuotes is a command with each quoted word replaced by Q: what sh
// reads as syntax.
func outsideQuotes(cmd string) string {
	var b strings.Builder
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; c {
		case '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return b.String() + "<unclosed>"
			}
			b.WriteByte('Q')
			i += j + 1
		case '"':
			j := i + 1
			for ; j < len(cmd) && cmd[j] != '"'; j++ {
				if cmd[j] == '\\' {
					j++
				}
			}
			if j >= len(cmd) {
				return b.String() + "<unclosed>"
			}
			b.WriteByte('Q')
			i = j
		case '\\':
			b.WriteByte('Q')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// No printed command has a placeholder the shell would read (the third
// review of #228): outside quotes there is no `<` at all, and a `>` only
// redirects into a name, quoted or of safe characters. Every command is built with its values
// missing, where a placeholder stands in.
func TestPrintedCommandsHaveNoBarePlaceholder(t *testing.T) {
	t.Parallel()
	bare := Container{Name: "tracepad-app", Repo: "ghcr.io/tracepad/tracepad", Compose: "obs",
		Run: []string{"-p", "127.0.0.1:4318:4318", imageSlot}, EnvNames: []string{"TRACEPAD_URL"}}
	chain, _ := containerSteps(bare, "0.2.0")
	cchain, up, start := composeSteps(bare, "0.2.0")
	commands := map[string]string{
		"a container's chain": chain, "a Compose chain": cchain, "Compose's up": up, "Compose's start": start,
		"a backup": backupStep(bare), "an install line": (&runner{deps: Deps{Home: "/home/u"}}).installLine("/opt/bin", "0.2.0"),
		"a service's restart": serviceRestart("the launchd job dev.tracepad"),
	}
	for name, cmd := range commands {
		syntax := outsideQuotes(cmd)
		for i := strings.IndexAny(syntax, "<>"); i >= 0; i = strings.IndexAny(syntax, "<>") {
			after := strings.TrimLeft(syntax[i+1:], " ")
			// A name, quoted or of safe characters, or a descriptor (>&2).
			if syntax[i] == '<' || after == "" || !strings.ContainsRune("Qabcdefghijklmnopqrstuvwxyz0123456789_./-&", rune(after[0])) {
				t.Errorf("%s: a bare %q in %s", name, syntax[i:], cmd)
				break
			}
			syntax = syntax[i+1:]
		}
	}
	if d := downgrade("0.1.0", "server pid 1", "0.2.0"); strings.ContainsAny(d, "<>") {
		t.Errorf("the way back's command: %s", d)
	}
	if got := outsideQuotes(`a 'b<c' "d>e" f\<g`); got != "a Q Q fQg" {
		t.Fatalf("outsideQuotes: %q", got)
	}
}
