package upgrade

import (
	"bytes"
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

// printedRun runs line with sh in a directory of its own, with stand-ins for
// docker, systemctl, sudo, launchctl and curl that record their arguments,
// and answers each call's argv in order.
func printedRun(t *testing.T, line string, env ...string) [][]string {
	t.Helper()
	stubs, log, cwd := t.TempDir(), t.TempDir(), t.TempDir()
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
			DataMount: "type=bind,src=" + v("/srv/data"),
			Run:       []string{"-p", "127.0.0.1:4318:4318", "--label", v("label="), imageSlot, "serve", v("arg")},
			EnvNames:  []string{"TRACEPAD_URL", "OTHER_1"}}
		chain, _ := containerSteps(c, "0.2.0")
		calls := printedRun(t, chain)
		if len(calls) != 6 {
			t.Fatalf("%d calls: %q", len(calls), calls)
		}
		sameArgs(t, "stop", calls[0], []string{"docker", "stop", c.Name})
		// A version that is not a release's is not in the archive's name.
		sameArgs(t, "backup", calls[1], []string{"docker", "run", "--rm", "--mount", c.DataMount + ",dst=/data,readonly", "-v", calls[1][6], busybox,
			"sh", "-c", `umask 077 && set -C && tar czf - -C /data . > "/backup/$1"`, "sh", c.Name + "-backup.tar.gz"})
		sameArgs(t, "pull", calls[2], []string{"docker", "pull", "ghcr.io/tracepad/tracepad:0.2.0"})
		sameArgs(t, "rename", calls[3], []string{"docker", "rename", c.Name, c.Name + "-old"})
		sameArgs(t, "inspect", calls[4], []string{"docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", c.Name + "-old"})
		sameArgs(t, "run", calls[5], []string{"docker", "run", "-d", "--name", c.Name, "--env-file", c.Name + ".upgrade.env",
			"-p", "127.0.0.1:4318:4318", "--label", c.Run[3], "ghcr.io/tracepad/tracepad:0.2.0", "serve", c.Run[6]})
		// A variable whose name is not a shell's is not written into a run.
		ic := inspectContainer{}
		ic.Config.Env = []string{v("ENV") + "=x"}
		if _, _, err := createdAs(ic, imageConfig{}, ""); err == nil || !strings.Contains(err.Error(), "not a shell variable's") {
			t.Errorf("a variable named %q: %v", v("ENV"), err)
		}
		sameArgs(t, "the archive of a release's version", []string{backupStep(Container{Name: "n", Version: "0.1.0", DataMount: "type=volume,src=v"})},
			[]string{`docker run --rm --mount type=volume,src=v,dst=/data,readonly -v "$PWD:/backup" ` + busybox + ` sh -c 'umask 077 && set -C && tar czf - -C /data . > "/backup/$1"' sh n-0.1.0.tar.gz`})
	})

	t.Run("a Compose project's", func(t *testing.T) {
		c := Container{Name: v("name"), Repo: "ghcr.io/tracepad/tracepad", Version: "0.1.0", DataMount: "type=volume,src=" + v("vol"),
			Compose: v("proj"), Service: v("svc"), ComposeDir: v("/srv/dir"),
			ComposeFiles: []string{v("/etc/a.yml"), v("/etc/b.yml")}, ComposeEnv: []string{v("/srv/a.env")}}
		chain, up, start := composeSteps(c, "0.2.0")
		calls := printedRun(t, chain+" && "+up+" && "+start)
		compose := []string{"docker", "compose", "-p", c.Compose, "--project-directory", c.ComposeDir, "--env-file", c.ComposeEnv[0], "-f", c.ComposeFiles[0], "-f", c.ComposeFiles[1]}
		if len(calls) != 5 {
			t.Fatalf("%d calls: %q", len(calls), calls)
		}
		sameArgs(t, "stop", calls[0], append(compose, "stop", c.Service))
		sameArgs(t, "backup's mount", calls[1][3:5], []string{"--mount", c.DataMount + ",dst=/data,readonly"})
		sameArgs(t, "backup's name", calls[1][len(calls[1])-1:], []string{c.Name + "-0.1.0.tar.gz"})
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
