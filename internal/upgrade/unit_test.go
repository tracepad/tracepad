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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

func TestReleaseOrder(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		"a container's run without its container": func(s *State) {
			s.Kind, s.Steps = kindContainer, []Step{{Name: stepPrepared}}
		},
	} {
		s := *good
		p := *good.Process
		s.Process = &p
		mutate(&s)
		// One rule for both: what does not load is not written either.
		if err := s.save(dir); err == nil {
			t.Errorf("%s: written", name)
		}
		if err := writeJSON(filepath.Join(dir, stateFile), &s); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A container's: every field that goes to docker, and the address the
	// key goes to, held to their shapes.
	ctr := &State{Run: id, Kind: kindContainer, From: "0.1.0", To: "0.2.0", Steps: []Step{{Name: stepPrepared}},
		Container: &ContainerState{Name: "tracepad-app", ID: strings.Repeat("a", 64), Volume: "tracepad-app", URL: "http://127.0.0.1:4318",
			OldRef: "ghcr.io/tracepad/tracepad:0.1.0", OldImage: "sha256:" + strings.Repeat("b", 64), NewRef: "ghcr.io/tracepad/tracepad:0.2.0", Restart: "on-failure:3"}}
	if err := ctr.save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(dir); err != nil {
		t.Fatalf("a good container state: %v", err)
	}
	for name, mutate := range map[string]func(*ContainerState){
		"a name with a space":     func(c *ContainerState) { c.Name = "tracepad app" },
		"an option for a name":    func(c *ContainerState) { c.Volume = "--privileged" },
		"a policy of its own":     func(c *ContainerState) { c.Restart = "always --privileged" },
		"an id that is not one":   func(c *ContainerState) { c.NewID = "$(id)" },
		"an image with a space":   func(c *ContainerState) { c.OldRef = "ghcr.io/x y" },
		"an address of its own":   func(c *ContainerState) { c.URL = "http://127.attacker.example:4318" },
		"an address with a path":  func(c *ContainerState) { c.URL = "http://127.0.0.1:4318/x" },
		"an address beyond":       func(c *ContainerState) { c.URL = "http://0.0.0.0:4318" },
		"localhost, not its IP":   func(c *ContainerState) { c.URL = "http://localhost:4318" },
		"a back id that is a ref": func(c *ContainerState) { c.BackID = "tracepad-app" },
	} {
		s := *ctr
		c := *ctr.Container
		s.Container = &c
		mutate(&c)
		if err := s.save(dir); err == nil {
			t.Errorf("a container's %s: written", name)
		}
		if err := writeJSON(filepath.Join(dir, stateFile), &s); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(dir); err == nil {
			t.Errorf("a container's %s: accepted", name)
		}
	}
	// Values docker gives, as it gives them, save and load back (the review
	// of #226: a plugin's log driver made every later --back refuse).
	for _, real := range []func(*ContainerState){
		func(c *ContainerState) { c.LogDriver = "grafana/loki-docker-driver:latest" },
		func(c *ContainerState) { c.LogDriver = "json-file" },
		func(c *ContainerState) {
			c.LogDriver = "registry.local:5000/logs/driver@sha256:" + strings.Repeat("c", 64)
		},
		func(c *ContainerState) { c.Name, c.Volume = "tracepad-my_app.2", strings.Repeat("d", 64) },
		func(c *ContainerState) {
			c.OldRef = "ghcr.io/tracepad/tracepad:0.1.0-rc.2@sha256:" + strings.Repeat("e", 64)
			c.NewRef = "docker.io/tracepad/tracepad:0.1.0-rc.3"
		},
		func(c *ContainerState) { c.URL, c.Restart = "http://[::1]:4318", "unless-stopped" },
		func(c *ContainerState) { c.Restart = "no" },
		func(c *ContainerState) { c.OldRef = "ghcr.io/tracepad/tracepad:Build_7" },
	} {
		s := *ctr
		c := *ctr.Container
		s.Container = &c
		real(&c)
		if err := s.save(dir); err != nil {
			t.Errorf("%+v: not written: %v", c, err)
			continue
		}
		if back, err := loadState(dir); err != nil || *back.Container != c {
			t.Errorf("%+v: does not load back: %v", c, err)
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
	t.Parallel()
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
		"dot-dot":        {Name: "../escape", Typeflag: tar.TypeReg},
		"dot-dot inside": {Name: "./a/../../escape", Typeflag: tar.TypeReg},
		"absolute":       {Name: filepath.Join(outside, "escape"), Typeflag: tar.TypeReg},
		"symlink out":    {Name: "./tracepad.db", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(outside, "escape")},
		"hardlink out":   {Name: "./tracepad.db", Typeflag: tar.TypeLink, Linkname: "../../escape"},
	} {
		evil := filepath.Join(t.TempDir(), "evil.tar.gz")
		_ = os.WriteFile(evil, rawArchive(t, hdr, []byte("x")), 0o600)
		target := filepath.Join(t.TempDir(), "r")
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
	t.Parallel()
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
	key := "tp-sk-x"
	r := &runner{deps: Deps{HTTP: srv.Client(), HealthWait: 3 * time.Second,
		Getenv: func(k string) string {
			if k == "TRACEPAD_API_KEY" {
				return key
			}
			return ""
		},
		Now:   func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { now = now.Add(time.Second); return nil }}}
	ctx := context.Background()
	up := func() bool { return true }
	n := func(v int64) counted { return counted{n: &v} }

	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictHealthy {
		t.Errorf("healthy: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", n(6), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "5 traces where there were 6") {
		t.Errorf("fewer: %+v", c)
	}
	system = http.StatusInternalServerError
	// Read before and not after: the one sentence too, with why (the
	// fifth review of #228).
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "cannot be read now") ||
		c.CountNote != "the trace counts were not compared: /api/v1/system answered HTTP 500" {
		t.Errorf("unreadable after: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", counted{}, up); c.Verdict != verdictHealthy {
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
	// Counts not compared are said whatever the verdict, with the reason
	// the read before found (the fourth and fifth reviews of #228): a
	// verdict that comes before any count read said nothing.
	if c := r.check(ctx, srv.URL, "0.2.0", counted{}, up); c.Verdict != verdictNotHealthy || c.CountNote != "the trace counts were not compared: the count before the upgrade was not read" {
		t.Errorf("another version, not read before: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", counted{why: noKey}, up); c.CountNote != "the trace counts were not compared: no TRACEPAD_API_KEY in the environment" {
		t.Errorf("another version, no key: %+v", c)
	}
	srv.Close()
	if c := r.check(ctx, srv.URL, "0.2.0", counted{why: "/api/v1/system answered HTTP 401"}, up); c.Verdict != verdictDecide || c.CountNote != "the trace counts were not compared: /api/v1/system answered HTTP 401" {
		t.Errorf("alive and silent, a key refused before: %+v", c)
	}
	if c := r.check(ctx, srv.URL, "0.2.0", n(5), up); c.Verdict != verdictDecide || !strings.Contains(c.Why, "has not answered") || c.CountNote != "" {
		t.Errorf("alive and silent: %+v", c)
	}
}

func TestTheFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--plan", "--back", "x"},
		{"--check", "x", "--back", "y"},
		{"--back", "x", "--to", "0.2.0"},
		{"--container", "c", "--data-dir", "d"},
		{"--back", "x", "--container", "c"},
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
	t.Parallel()
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
	st := &State{SetAside: []string{"/Users/me/Library/Application Support/tracepad.after-r"}}
	got := backupsSentence("/Users/me/tracepad-backups/r", st)
	if !strings.Contains(got, "rm -r '/Users/me/Library/Application Support/tracepad.after-r'") {
		t.Errorf("an unquoted path: %s", got)
	}
}

func TestOnlyWhatRunsOlderIsAChoice(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// The installed binary is where the running one is (the eighth review): a
// person who installed to a directory of their own runs it from there. The
// agent's bridge runs one from a temporary directory, and upgrades the
// installed one; a run's own copy is named upgrader, and does too.
func TestTheInstallDirectoryIsWhereTheBinaryRuns(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	scriptBinary(t, filepath.Join(tmp, "tracepad"), "0.1.0")
	for _, tc := range []struct{ named, self, want string }{
		{"", "/opt/tools/tracepad", "/opt/tools"},
		{"", "/home/u/.local/bin/tracepad", "/home/u/.local/bin"},
		{"", filepath.Join(tmp, "tracepad"), "/home/u/.local/bin"},
		{"", "/home/u/tracepad-backups/r/upgrader", "/home/u/.local/bin"},
		{"/srv/bin/", "/opt/tools/tracepad", "/srv/bin"},
	} {
		if got := installDirFor(tc.named, "/home/u", tc.self, tmp); got != tc.want {
			t.Errorf("%q, %s: %s, want %s", tc.named, tc.self, got, tc.want)
		}
	}
}

// The bridge is found in a temporary directory however the shell was
// started (the live run of rc.3): with no TMPDIR — env -i, cron, an agent's
// bare shell — Go names /tmp while macOS's mktemp still makes its directory
// under the user's own, and a directory mktemp named is temporary wherever
// it is. A real `env -i mktemp -d` is asked.
func TestTheBridgeIsTemporaryWithNoTMPDIR(t *testing.T) {
	t.Parallel()
	for _, self := range []string{"/var/folders/ab/cdef/T/tmp.a0qjbaFU2H/tracepad", "/scratch/tmp.Xy12Zw/tracepad"} {
		if got := installDirFor("", "/home/u", self, "/tmp", ""); got != "/home/u/.local/bin" {
			t.Errorf("%s: %s", self, got)
		}
	}
	if got := installDirFor("", "/home/u", "/opt/tmp.d/tracepad", "/tmp", ""); got != "/opt/tmp.d" {
		t.Errorf("a directory of one's own, named like none of mktemp's: %s", got)
	}
	out, err := exec.Command("/usr/bin/env", "-i", "mktemp", "-d").Output()
	if err != nil {
		t.Skip("no mktemp: ", err)
	}
	dir := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = os.Remove(dir) })
	// What the command sees in such a shell: os.TempDir() is /tmp.
	if got := installDirFor("", "/home/u", filepath.Join(dir, "tracepad"), "/tmp", systemUserTempDir()); got != "/home/u/.local/bin" {
		t.Errorf("env -i mktemp -d made %s, and the bridge there installs into %s", dir, got)
	}
}

// The skill's bridge lives under the user's cache in a directory mktemp
// makes for the run (spec 054 #63; before it, the one tmp.release of #57,
// which the skills of 0.1.1 still name): every release that knows a bridge,
// 0.1.0's included, takes a binary there for one and plans for the installed
// binary (the sixth review of #228: a name of this PR's own was a bridge
// only to the releases after it).
func TestTheBridgeInTheCacheIsABridge(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ self, want string }{
		{"/home/u/.cache/tracepad/tmp.Q7xK2p/tracepad", "/home/u/.local/bin"},
		{"/home/u/.cache/tracepad/tmp.release/tracepad", "/home/u/.local/bin"},
		{"/srv/cache/tracepad/tmp.release/tracepad", "/home/u/.local/bin"},
		{"/home/u/.cache/tracepad/tracepad", "/home/u/.cache/tracepad"},
	} {
		if got := installDirFor("", "/home/u", c.self, "/tmp"); got != c.want {
			t.Errorf("%s: %s, want %s", c.self, got, c.want)
		}
	}
}

// A binary that does not say its version says why, in the first line it
// wrote to stderr (spec 054 #63): the exit status alone named nothing.
func TestABinaryThatDoesNotSayItsVersionSaysWhy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tracepad")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'tracepad: unknown command \"version\"' >&2\necho 'run tracepad help' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := binaryVersion(context.Background(), path)
	if err == nil || !strings.HasSuffix(err.Error(), `exit status 2: tracepad: unknown command "version"`) {
		t.Errorf("%v", err)
	}
}

// The user's temporary directory is asked only when it can matter (the
// review of #225): a directory named, a binary not called tracepad, or one
// already found temporary start no getconf.
func TestTheUsersTempDirIsAskedOnlyWhenItMatters(t *testing.T) {
	saved := userTempDir
	t.Cleanup(func() { userTempDir = saved })
	asked := 0
	userTempDir = func() string { asked++; return "" }
	for _, tc := range []struct {
		named, self string
		asks        int
	}{
		{"/srv/bin", "/opt/tools/tracepad", 0},
		{"", "/home/u/tracepad-backups/r/upgrader", 0},
		{"", filepath.Join(os.TempDir(), "tmp.Ab12Cd", "tracepad"), 0},
		{"", "/opt/tools/tracepad", 1},
		{"", "/home/u/.cache/tracepad/tmp.release/tracepad", 0},
	} {
		asked = 0
		installTemps(tc.named, tc.self)
		if asked != tc.asks {
			t.Errorf("%q %s: asked %d times", tc.named, tc.self, asked)
		}
	}
}

// A package manager's binary is the person's (the ninth review), a link or
// not: Homebrew's Cellar, the Nix store, a snap, the system's directories.
func TestAPackageManagersBinaryIsThePersons(t *testing.T) {
	t.Parallel()
	for path, managed := range map[string]bool{
		"/opt/homebrew/Cellar/tracepad/0.1.0/bin/tracepad":     true,
		"/usr/local/Cellar/tracepad/0.1.0/bin/tracepad":        true,
		"/home/linuxbrew/.linuxbrew/bin/tracepad":              true,
		"/nix/store/abc-tracepad-0.1.0/bin/tracepad":           true,
		"/snap/tracepad/12/bin/tracepad":                       true,
		"/usr/bin/tracepad":                                    true,
		"/home/u/.local/bin/tracepad":                          false,
		"/opt/tools/tracepad":                                  false,
		"/usr/local/bin/tracepad":                              false,
		"/Users/u/Library/Application Support/tracepad/bin/tp": false,
	} {
		if got := packageManager(path) != ""; got != managed {
			t.Errorf("%s: managed %v", path, got)
		}
	}
}

// On Windows the command refuses, plainly, before it looks at anything (the
// ninth review): it cannot find a server's process or lock there.
func TestWindowsIsRefusedPlainly(t *testing.T) {
	saved := goos
	goos = "windows"
	t.Cleanup(func() { goos = saved })
	for _, args := range [][]string{{"--plan"}, {}, {"--back", "20261006-120000-0.1.0-abc123"}} {
		var out bytes.Buffer
		code := run(context.Background(), Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, Deps{})
		if code != exitRefused || !strings.Contains(out.String(), "does not run on Windows") {
			t.Errorf("%q: %d %s", args, code, out.String())
		}
	}
}

// A directory archived without its owner's write restores whole, its mode
// set once its contents are in (the tenth review).
func TestAReadOnlyDirectoryRestores(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	_ = os.WriteFile(filepath.Join(data, dataDBName), []byte("db"), 0o600)
	sub := filepath.Join(data, "payloads")
	_ = os.Mkdir(sub, 0o700)
	_ = os.WriteFile(filepath.Join(sub, "a"), []byte("a"), 0o600)
	_ = os.Chmod(sub, 0o500)
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	archive := filepath.Join(t.TempDir(), "data.tar.gz")
	if _, err := writeArchive(data, archive); err != nil {
		t.Fatal(err)
	}
	restore := filepath.Join(t.TempDir(), "restore")
	if err := extractArchive(archive, restore, 0o700); err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(restore, "payloads"), 0o700) })
	if b, err := os.ReadFile(filepath.Join(restore, "payloads", "a")); err != nil || string(b) != "a" {
		t.Errorf("payloads/a: %q %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(restore, "payloads")); err != nil || info.Mode().Perm() != 0o500 {
		t.Errorf("payloads' mode: %v %v", info, err)
	}
}

// A server's flag given empty is what it reads (the tenth review), as
// config.FromEnv applies it: --data-dir= is its working directory, not the
// environment's directory.
func TestAnEmptyFlagIsTheServersToo(t *testing.T) {
	t.Parallel()
	p := Process{Argv: []string{"tracepad", "serve", "--data-dir="}, Env: []string{"TRACEPAD_DATA_DIR=/x"}, Cwd: "/w"}
	if d, _, err := resolveServer(p); err != nil || d != "/w" {
		t.Errorf("%q %v", d, err)
	}
	if len(relativePaths(p)) == 0 {
		t.Error("an empty data directory is relative to where the server started")
	}
}

// An answer as another version is the verdict at once (the twelfth review):
// waiting does not turn it into the version asked for, and meanwhile what
// answers runs on the data.
func TestAWrongVersionIsNotHealthyAtOnce(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"9.9.9"}`))
	}))
	defer srv.Close()
	sleeps := 0
	r := &runner{deps: Deps{HTTP: srv.Client(), Now: time.Now, HealthWait: time.Hour, Getenv: func(string) string { return "" },
		Sleep: func(context.Context, time.Duration) error { sleeps++; return nil }}}
	c := r.check(context.Background(), srv.URL, "0.5.1", counted{}, func() bool { return true })
	if c.Verdict != verdictNotHealthy || c.Health != "9.9.9" || sleeps > 0 {
		t.Errorf("%+v after %d waits", c, sleeps)
	}
}

// ourSkillMD is a SKILL.md that names the skill, as every install writes.
const ourSkillMD = "---\nname: tracepad\n---\n"

// skillCopyAt puts a copy of the skill at version in home's
// ~/.claude/skills, by the skill's own rule (spec 037 #16): a .version beside
// a SKILL.md that names it. It answers the copy's directory.
func skillCopyAt(t *testing.T, home, version string) string {
	t.Helper()
	return skillFilesAt(t, home, version, ourSkillMD)
}

// skillFilesAt writes what is given of a copy in home's ~/.claude/skills — a
// .version with version, a SKILL.md with skillMD, either left out when "" —
// and answers its directory. A write that fails ends the test: a cell that
// expects no copy must not pass for want of a file.
func skillFilesAt(t *testing.T, home, version, skillMD string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", "tracepad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".version": version + "\n", "SKILL.md": skillMD} {
		if strings.TrimSpace(body) == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The project's copy is the user's when the command runs in the home
// directory, however the two are spelled (the second review of #232): one
// copy, listed once, so a run installs it once.
func TestTheSkillsCopyIsListedOnceHoweverItIsSpelled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	skillCopyAt(t, home, "0.5.0")
	r := &runner{deps: Deps{Home: home, Cwd: link}}
	if copies := r.skillCopies(); len(copies) != 1 || copies[0].args[0] != "--dir" {
		t.Errorf("copies %+v", copies)
	}
}

// The skill is recorded as installed only when it was (the twelfth review):
// a binary that is not the run's version any more installs nothing, says
// so, and leaves the step for a later check.
func TestASkillIsDoneOnlyWhenInstalled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	skillCopyAt(t, home, "0.5.0")
	installs := 0
	r := &runner{deps: Deps{Home: home, Cwd: home,
		Version: func(context.Context, string) (string, error) { return "0.4.0", nil },
		Skills:  func(context.Context, string, string, ...string) (string, error) { installs++; return "ok", nil }}}
	rep := &Report{}
	if r.reinstallSkill(context.Background(), rep, "/bin/tracepad", "0.5.1") || installs > 0 {
		t.Errorf("done with %d installs", installs)
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), "/bin/tracepad is 0.4.0 now, not 0.5.1") {
		t.Errorf("notes: %q", rep.Notes)
	}
}

// A download follows a redirect only to https:// (the thirteenth review):
// one to plain HTTP is refused, whatever client the command was given, and
// one to another https:// host is followed, as GitHub's releases need.
func TestADownloadIsNeverRedirectedToPlainHTTP(t *testing.T) {
	t.Parallel()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer plain.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the file"))
	}))
	defer other.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain":
			http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
		case "/other":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		}
	}))
	defer tls.Close()
	// One client that trusts both test servers' certificates, and follows any
	// redirect itself: the refusal must be the command's.
	client := tls.Client()
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(other.Certificate())
	r := &Releases{HTTP: client}
	if b, err := r.read(context.Background(), tls.URL+"/plain"); err == nil || !strings.Contains(err.Error(), "only https:// is") {
		t.Errorf("a redirect to plain HTTP: %q %v", b, err)
	}
	if b, err := r.read(context.Background(), tls.URL+"/other"); err != nil || string(b) != "the file" {
		t.Errorf("a redirect to another https host: %q %v", b, err)
	}
}
