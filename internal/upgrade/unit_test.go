package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

func TestReleaseOrder(t *testing.T) {
	ordered := []string{"0.1.0-alpha.1", "0.1.0-alpha.2", "0.1.0-beta.1", "0.1.0-rc.1", "0.1.0-rc.2", "0.1.0-rc.10", "0.1.0", "0.1.1", "0.2.0", "1.0.0"}
	for i := range ordered {
		for k := range ordered {
			got, ok := Compare(ordered[i], ordered[k])
			if !ok || got != sign(i-k) {
				t.Errorf("Compare(%s, %s) = %d, %v", ordered[i], ordered[k], got, ok)
			}
		}
	}
	for _, v := range []string{"dev", "0.1", "v0.1.0", "0.1.0-rc1", "01.0.0", "0.1.0+build", "0.1.0-pre.1", ""} {
		if IsRelease(v) {
			t.Errorf("IsRelease(%q)", v)
		}
		if _, ok := Compare(v, "0.1.0"); ok {
			t.Errorf("Compare(%q, 0.1.0) claimed an order", v)
		}
	}
	if normalizeVersion(" v0.2.0") != "0.2.0" {
		t.Error("normalizeVersion")
	}
}

// scriptBinary writes a stand-in binary that answers `version` with says.
func scriptBinary(t *testing.T, path, says string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = version ] && echo "+says+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func archiveOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// rawArchive is a gzipped tar of one entry, header as given.
func rawArchive(t *testing.T, h tar.Header, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	h.Mode = 0o600
	if h.Typeflag == tar.TypeReg {
		h.Size = int64(len(body))
	}
	if err := tw.WriteHeader(&h); err != nil {
		t.Fatal(err)
	}
	if h.Typeflag == tar.TypeReg {
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func testReleases(t *testing.T, mirror string) *Releases {
	return &Releases{Base: "file://" + mirror, Mirror: true, OS: "linux", Arch: "amd64", Version: binaryVersion}
}

// mirrorRelease publishes a release whose binary answers says.
func mirrorRelease(t *testing.T, mirror, v, says string, stable bool) {
	t.Helper()
	dir := filepath.Join(mirror, "download", "v"+v)
	_ = os.MkdirAll(dir, 0o755)
	body := archiveOf(t, map[string][]byte{"tracepad": []byte("#!/bin/sh\n[ \"$1\" = version ] && echo " + says + "\n")})
	name := "tracepad_" + v + "_linux_amd64.tar.gz"
	_ = os.WriteFile(filepath.Join(dir, name), body, 0o644)
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	_ = os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o644)
	if stable {
		_ = os.MkdirAll(filepath.Join(mirror, "latest", "download"), 0o755)
		_ = os.WriteFile(filepath.Join(mirror, "latest", "download", "checksums.txt"), []byte(sums), 0o644)
	}
}

func TestReleasesLikeTheInstallScript(t *testing.T) {
	ctx := context.Background()
	mirror := t.TempDir()
	r := testReleases(t, mirror)
	var none *NoStableError
	if _, err := r.Latest(ctx); !errors.As(err, &none) {
		t.Fatalf("no stable release: %v", err)
	}
	mirrorRelease(t, mirror, "0.2.0", "0.2.0", true)
	mirrorRelease(t, mirror, "0.3.0-rc.1", "0.3.0-rc.1", false)
	if v, err := r.Latest(ctx); err != nil || v != "0.2.0" {
		t.Fatalf("latest: %q %v", v, err)
	}
	if err := r.Exists(ctx, "0.3.0-rc.1"); err != nil {
		t.Error(err)
	}
	if err := r.Exists(ctx, "0.4.0"); err == nil {
		t.Error("a release that is not there exists")
	}

	dir := t.TempDir()
	got, err := r.Fetch(ctx, "0.2.0", dir, "tracepad-0.2.0")
	if err != nil || !strings.Contains(got.Verified, "sha256 matches") {
		t.Fatalf("fetch: %+v %v", got, err)
	}

	// A checksum that does not match, an attestation that fails, a binary
	// that answers another version, no build for this platform.
	mirrorRelease(t, mirror, "0.2.1", "0.2.1", false)
	_ = os.WriteFile(filepath.Join(mirror, "download", "v0.2.1", "checksums.txt"), []byte(strings.Repeat("0", 64)+"  tracepad_0.2.1_linux_amd64.tar.gz\n"), 0o644)
	if _, err := r.Fetch(ctx, "0.2.1", dir, "a"); err == nil || !strings.Contains(err.Error(), "does not match checksums.txt") {
		t.Errorf("wrong checksum: %v", err)
	}
	r.Attest = func(context.Context, string) (string, error) { return "", errors.New("no attestation matched") }
	if _, err := r.Fetch(ctx, "0.2.0", dir, "b"); err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Errorf("attestation: %v", err)
	}
	r.Attest = nil
	mirrorRelease(t, mirror, "0.2.2", "0.0.0-other", false)
	if _, err := r.Fetch(ctx, "0.2.2", dir, "c"); err == nil || !strings.Contains(err.Error(), "says it is") {
		t.Errorf("another version: %v", err)
	}
	r.Arch = "riscv64"
	if _, err := r.Fetch(ctx, "0.2.0", dir, "d"); err == nil || !strings.Contains(err.Error(), "no build for linux/riscv64") {
		t.Errorf("no build: %v", err)
	}

	plain := &Releases{Base: "http://example.invalid/releases", HTTP: http.DefaultClient}
	if _, err := plain.Latest(ctx); err == nil || !strings.Contains(err.Error(), "only https:// and file://") {
		t.Errorf("plain HTTP: %v", err)
	}
}

func TestTheNewestCandidateIsNamedFromGitHubsAPI(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases":
			fmt.Fprint(w, `[{"tag_name":"v0.1.0-rc.2"},{"tag_name":"v0.1.0-rc.1"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r := &Releases{Base: srv.URL + "/releases", API: srv.URL + "/api", HTTP: srv.Client()}
	_, err := r.Latest(context.Background())
	var none *NoStableError
	if !errors.As(err, &none) || none.Candidate != "0.1.0-rc.2" || !strings.Contains(err.Error(), "--to 0.1.0-rc.2") {
		t.Fatalf("%v", err)
	}
}

// serverWorld is a data directory whose lock records pid.
func lockedDataDir(t *testing.T, pid int) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "tracepad.db"+store.LockSuffix), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWhatServerIsTheCommands(t *testing.T) {
	bin, _ := filepath.EvalSymlinks(t.TempDir())
	install := filepath.Join(bin, "tracepad")
	data := lockedDataDir(t, 42)
	ours := Process{PID: 42, Exe: install, Cwd: "/", Argv: []string{"tracepad", "serve", "--listen", "localhost:4318", "--data-dir", data}}

	s, ok := classifyServer(ours, install)
	if !ok || !s.Ours || s.URL != "http://127.0.0.1:4318" || s.DataDir != data {
		t.Fatalf("ours: %+v", s)
	}
	for _, tc := range []struct {
		name   string
		change func(*Process)
		reason string
	}{
		{"another binary", func(p *Process) { p.Exe = "/opt/homebrew/bin/tracepad" }, "not the installed"},
		{"no executable", func(p *Process) { p.Exe = "" }, "could not be read"},
		{"a lock that records another", func(p *Process) { p.PID = 43 }, "does not record it"},
		{"a service", func(p *Process) { p.Manager = "the systemd unit tracepad.service" }, "may restart what the command stops"},
		{"a manager that cannot be ruled out", func(p *Process) {
			p.Manager = "its cgroup could not be read, so a service manager cannot be ruled out"
		}, "cannot be ruled out"},
		{"beyond this machine", func(p *Process) { p.Argv[3] = "0.0.0.0:4318" }, "beyond this machine"},
		{"all interfaces", func(p *Process) { p.Argv[3] = ":4318" }, "beyond this machine"},
		{"relative, no cwd", func(p *Process) { p.Argv[5] = "rel"; p.Cwd = "" }, "relative"},
	} {
		p := ours
		p.Argv = slices.Clone(ours.Argv)
		tc.change(&p)
		s, ok := classifyServer(p, install)
		if !ok || s.Ours || !strings.Contains(s.Reason, tc.reason) {
			t.Errorf("%s: %+v", tc.name, s)
		}
	}

	// The data directory and address come from the environment and the
	// default when the flags do not say them, as the server reads them.
	home, _ := filepath.EvalSymlinks(t.TempDir())
	def := filepath.Join(home, ".local", "share", "tracepad")
	_ = os.MkdirAll(def, 0o700)
	_ = os.WriteFile(filepath.Join(def, "tracepad.db"+store.LockSuffix), []byte("7\n"), 0o600)
	bare := Process{PID: 7, Exe: install, Argv: []string{"tracepad"}, Env: []string{"HOME=" + home, "TRACEPAD_LISTEN=127.0.0.1:5000"}}
	s, ok = classifyServer(bare, install)
	if !ok || !s.Ours || s.DataDir != def || s.Listen != "127.0.0.1:5000" {
		t.Errorf("bare: %+v", s)
	}
	for _, argv := range [][]string{{"tracepad", "mcp"}, {"tracepad", "traces", "ls"}} {
		if _, ok := classifyServer(Process{Exe: install, Argv: argv}, install); ok {
			t.Errorf("%q counted as a server", argv)
		}
	}
}

func TestTheStateIsDataAndOnlyThisRuns(t *testing.T) {
	root := t.TempDir()
	id := "20261005-120000-0.1.0-abc123"
	dir := filepath.Join(root, id)
	_ = os.Mkdir(dir, 0o700)
	good := &State{Run: id, Kind: kindProcess, From: "0.1.0", To: "0.2.0",
		Process: &ProcessState{PID: 10, DataDir: "/d", Listen: "localhost:4318", URL: "http://127.0.0.1:4318", Log: "/d/server.log", Old: dir + "/tracepad-0.1.0"}}
	if err := good.save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(dir); err != nil {
		t.Fatalf("a good state: %v", err)
	}
	for name, mutate := range map[string]func(*State){
		"another run's":       func(s *State) { s.Run = "20261005-120000-0.1.0-zzz999" },
		"a command in from":   func(s *State) { s.From = "1.0.0;touch /tmp/x" },
		"a substitution":      func(s *State) { s.To = "$(id)" },
		"backquotes":          func(s *State) { s.To = "`id`" },
		"beyond this machine": func(s *State) { s.Process.Listen = "0.0.0.0:4318" },
		"a URL of its own":    func(s *State) { s.Process.URL = "http://attacker.example:4318" },
		"[::1] for localhost": func(s *State) { s.Process.URL = "http://[::1]:4318" },
		"a relative path":     func(s *State) { s.Process.DataDir = "d" },
		"no kind":             func(s *State) { s.Kind = "shell" },
		"a container's policy": func(s *State) {
			s.Kind = kindContainer
			s.Container = &ContainerState{Name: "tracepad-a", ID: strings.Repeat("a", 64), Volume: "v", URL: "http://127.0.0.1:4318",
				OldRef: "ghcr.io/tracepad/tracepad:0.1.0", OldImage: "sha256:aa", NewRef: "ghcr.io/tracepad/tracepad:0.2.0", Restart: "always --privileged"}
		},
	} {
		s := *good
		p := *good.Process
		s.Process = &p
		mutate(&s)
		if err := s.save(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"run":"`+id+`","exec":"rm -rf /"}`), 0o600)
	if _, err := loadState(dir); err == nil {
		t.Error("an unknown field: accepted")
	}

	for _, arg := range []string{"../x", "newest", "", "/tmp/notarun"} {
		if _, err := resolveRun(root, arg); err == nil {
			t.Errorf("resolveRun(%q) accepted", arg)
		}
	}
	if got, err := resolveRun(root, id); err != nil || got != dir {
		t.Errorf("resolveRun(id) = %q, %v", got, err)
	}
}

func TestTheArchiveCountsOnlyWhenItReadsBackWhole(t *testing.T) {
	data := t.TempDir()
	db := bytes.Repeat([]byte("sqlite"), 50000)
	_ = os.WriteFile(filepath.Join(data, "tracepad.db"), db, 0o600)
	_ = os.MkdirAll(filepath.Join(data, "media", "ab"), 0o700)
	_ = os.WriteFile(filepath.Join(data, "media", "ab", "blob"), []byte("x"), 0o600)
	if size, err := survey(data); err != nil || size != int64(len(db))+1 {
		t.Fatalf("survey: %d %v", size, err)
	}
	out := filepath.Join(t.TempDir(), "data.tar.gz")
	a, err := writeArchive(data, out)
	if err != nil || a.DBSize != int64(len(db)) {
		t.Fatalf("write: %+v %v", a, err)
	}
	if err := verifyArchive(out, a); err != nil {
		t.Fatalf("verify: %v", err)
	}
	restore := filepath.Join(t.TempDir(), "restore")
	if err := extractArchive(out, restore, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(restore, "tracepad.db")); !bytes.Equal(got, db) {
		t.Error("the restore is not the data")
	}
	if got, _ := os.ReadFile(filepath.Join(restore, "media", "ab", "blob")); string(got) != "x" {
		t.Error("the restore lost a file")
	}

	whole, _ := os.ReadFile(out)
	for name, cut := range map[string][]byte{
		"half":           whole[:len(whole)/2],
		"no gzip footer": whole[:len(whole)-4],
		"one byte":       whole[:1],
	} {
		_ = os.WriteFile(out, cut, 0o600)
		if err := verifyArchive(out, a); err == nil {
			t.Errorf("%s: read back whole", name)
		}
	}
	_ = os.WriteFile(out, whole, 0o600)
	if err := verifyArchive(out, Archived{SHA256: a.SHA256, DBSize: a.DBSize + 1}); err == nil {
		t.Error("a database of another size: accepted")
	}
	if err := verifyArchive(out, Archived{SHA256: strings.Repeat("0", 64), DBSize: -1}); err == nil {
		t.Error("another checksum: accepted")
	}
	noDB := filepath.Join(t.TempDir(), "x.tar.gz")
	_ = os.WriteFile(noDB, archiveOf(t, map[string][]byte{"./other": []byte("x")}), 0o600)
	if err := verifyArchive(noDB, Archived{DBSize: -1}); err == nil {
		t.Error("an archive without tracepad.db: accepted")
	}

	outside := t.TempDir()
	for name, hdr := range map[string]tar.Header{
		"dot-dot":            {Name: "../escape", Typeflag: tar.TypeReg},
		"dot-dot inside":     {Name: "./a/../../escape", Typeflag: tar.TypeReg},
		"absolute":           {Name: filepath.Join(outside, "escape"), Typeflag: tar.TypeReg},
		"symlink out":        {Name: "./tracepad.db", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(outside, "escape")},
		"hardlink out":       {Name: "./tracepad.db", Typeflag: tar.TypeLink, Linkname: "../../escape"},
		"dot-dot, extractDB": {Name: "../tracepad.db", Typeflag: tar.TypeReg},
	} {
		evil := filepath.Join(t.TempDir(), "evil.tar.gz")
		_ = os.WriteFile(evil, rawArchive(t, hdr, []byte("x")), 0o600)
		target := filepath.Join(t.TempDir(), "r")
		if strings.Contains(name, "extractDB") {
			_ = os.Mkdir(target, 0o700)
			err := extractDB(evil, target)
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(target), "tracepad.db")); err == nil && statErr == nil {
				t.Errorf("%s: written outside", name)
			}
			continue
		}
		if err := extractArchive(evil, target, 0o700); err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Lstat(filepath.Join(outside, "escape")); err == nil {
			t.Errorf("%s: written outside", name)
		}
	}
	if err := extractArchive(out, restore, 0o700); err == nil {
		t.Error("extracted into a directory that exists")
	}

	_ = os.Symlink("/etc", filepath.Join(data, "link"))
	if _, err := survey(data); err == nil {
		t.Error("a link in the data directory: surveyed")
	}
}

func TestTheCheckVerdicts(t *testing.T) {
	version, traces, system := "0.2.0", int64(5), http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
		case "/api/v1/system":
			if r.Header.Get("Authorization") != "Bearer tp-sk-x" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(system)
			fmt.Fprintf(w, `{"database":{"rows":{"traces":%d}}}`, traces)
		}
	}))
	defer srv.Close()
	now := time.Unix(0, 0)
	r := &runner{deps: Deps{HTTP: srv.Client(), HealthWait: 3 * time.Second,
		Getenv: func(k string) string {
			if k == "TRACEPAD_API_KEY" {
				return "tp-sk-x"
			}
			return ""
		},
		Now:   func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { now = now.Add(time.Second); return nil }}}
	ctx := context.Background()
	up := func() bool { return true }
	n := func(v int64) *int64 { return &v }

	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictHealthy {
		t.Errorf("healthy: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", n(6), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "5 traces where there were 6") {
		t.Errorf("fewer: %+v", c)
	}
	system = http.StatusInternalServerError
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "cannot be read now") {
		t.Errorf("unreadable after: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", nil, up); c.Verdict != verdictHealthy {
		t.Errorf("not read before: %+v", c)
	}
	system = http.StatusOK
	version = "0.1.0"
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictNotHealthy || !strings.Contains(c.Why, "answers as 0.1.0") {
		t.Errorf("another version: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), func() bool { return false }); c.Verdict != verdictNotHealthy || !strings.Contains(c.Why, "exited") {
		t.Errorf("exited: %+v", c)
	}
	srv.Close()
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "has not answered") {
		t.Errorf("alive and silent: %+v", c)
	}
}

func TestTheFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--plan", "--back", "x"},
		{"--check", "x", "--back", "y"},
		{"--back", "x", "--to", "0.2.0"},
		{"--data-dir", "d", "--container", "c"},
		{"extra"},
		{"--back", ""},
		{"--check", ""},
		{"--to", ""},
		{"--to", " "},
		{"--back", "  "},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	f, err := parseFlags([]string{"--to", "v0.2.0", "--json"})
	if err != nil || f.to != "0.2.0" || !f.json {
		t.Errorf("%+v %v", f, err)
	}
}

func TestCommandsArePastedAsTheyAreMeant(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/tracepad-backups/x":                     "/home/u/tracepad-backups/x",
		"/Users/me/Library/Application Support/tracepad": "'/Users/me/Library/Application Support/tracepad'",
		"it's":  `'it'\''s'`,
		"$(id)": "'$(id)'",
		"":      "''",
	} {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
	st := &State{SetAside: []string{"/Users/me/Library/Application Support/tracepad.after-r", "container c-after-r", "volume v", "container c-before-r"}}
	got := backupsSentence("/Users/me/tracepad-backups/r", st)
	if !strings.Contains(got, "rm -r '/Users/me/Library/Application Support/tracepad.after-r'") {
		t.Errorf("an unquoted path: %s", got)
	}
	if strings.Index(got, "docker volume rm v") < strings.Index(got, "docker rm c-before-r") {
		t.Errorf("a volume before a container: %s", got)
	}
}

func TestOnlyWhatRunsOlderIsAChoice(t *testing.T) {
	r := &runner{}
	p := &plan{to: "0.2.0", f: Findings{Servers: []Server{
		{Proc: Process{PID: 1}, DataDir: "/a", Ours: true, Version: "0.2.0"},
		{Proc: Process{PID: 2}, DataDir: "/b", Ours: true, Version: "0.2.0"},
	}}}
	if err := r.pickTarget(p); err != nil || len(p.choose) != 0 || p.server != nil || p.pending() {
		t.Errorf("two up to date: choose %q, server %v", p.choose, p.server)
	}
	p.f.Servers[1].Version = "0.1.0"
	if err := r.pickTarget(p); err != nil || len(p.choose) != 0 || p.server == nil || p.server.Proc.PID != 2 {
		t.Errorf("one behind: choose %q, server %+v", p.choose, p.server)
	}
}

// A run is taken only from a directory of the person's own, directly under
// the backups directory (#30).
func TestARunIsTakenOnlyFromThePersonsOwnDirectory(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	backups := filepath.Join(root, "tracepad-backups")
	id := "20261006-120000-0.1.0-abc123"
	_ = os.MkdirAll(filepath.Join(backups, id), 0o700)
	_ = os.Chmod(backups, 0o700)
	if got, err := resolveRun(backups, filepath.Join(backups, id)); err != nil || private(got) != nil {
		t.Fatalf("the person's own: %q %v", got, err)
	}
	elsewhere := filepath.Join(root, "elsewhere", id)
	_ = os.MkdirAll(elsewhere, 0o700)
	if _, err := resolveRun(backups, elsewhere); err == nil {
		t.Error("a run outside the backups directory: taken")
	}
	_ = os.Chmod(filepath.Join(backups, id), 0o755)
	if err := private(filepath.Join(backups, id)); err == nil {
		t.Error("a run open to other users: taken")
	}
	_ = os.Chmod(filepath.Join(backups, id), 0o700)
	link := "20261006-120000-0.1.0-zzz999"
	_ = os.Symlink(elsewhere, filepath.Join(backups, link))
	if err := private(filepath.Join(backups, link)); err == nil {
		t.Error("a run that is a link: taken")
	}
	// A state whose copies are outside its run.
	st := &State{Run: id, Kind: kindBinary, From: "0.1.0", To: "0.2.0", Binary: &BinaryState{Path: "/x/tracepad", From: "0.1.0", Old: "/tmp/tracepad-0.1.0"}}
	if err := st.validate(filepath.Join(backups, id)); err == nil {
		t.Error("a copy outside its run: accepted")
	}
}
