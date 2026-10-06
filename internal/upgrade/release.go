package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// repo is where the releases are.
const repo = "tracepad/tracepad"

// githubReleases is the releases page the install script reads by default;
// TRACEPAD_DOWNLOAD_URL replaces it, for both (spec 053 #5).
const githubReleases = "https://github.com/" + repo + "/releases"

// Releases reads releases the way the install script does (Decision 3): the
// archives and `checksums.txt` under `<Base>/download/v<version>/`, and the
// latest stable release through `<Base>/latest/download/`.
type Releases struct {
	// Base is TRACEPAD_DOWNLOAD_URL, or GitHub's releases page.
	Base string
	// Mirror is whether Base is TRACEPAD_DOWNLOAD_URL: GitHub's API is asked
	// for the newest candidate only when the releases are GitHub's.
	Mirror bool
	// API is GitHub's REST root, for the newest candidate's name alone.
	API      string
	HTTP     *http.Client
	OS, Arch string
	// Attest checks a downloaded archive's build attestation. It answers what
	// it did for the report ("build attestation verified (gh)", or why it was
	// not checked), or an error when a check ran and failed.
	Attest func(ctx context.Context, archive string) (string, error)
	// Version runs a binary and answers what it says it is.
	Version func(ctx context.Context, path string) (string, error)
}

// NoStableError is the one failure everyone meets before the first stable
// release: there is none, and a candidate is taken only when named.
type NoStableError struct{ Candidate string }

func (e *NoStableError) Error() string {
	if e.Candidate != "" {
		return fmt.Sprintf("there is no stable release of Tracepad yet; the newest release is %s: name it with --to %s", e.Candidate, e.Candidate)
	}
	return "there is no stable release of Tracepad yet; name the release to go to with --to (the releases are listed at " + githubReleases + ")"
}

// errNotFound is a 404, or a file:// path that is not there.
var errNotFound = errors.New("not found")

// open reads an https:// or file:// address. A plain-HTTP mirror is refused
// rather than trusted, as the install script refuses it.
func (r *Releases) open(ctx context.Context, address string) (io.ReadCloser, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "file":
		f, err := os.Open(u.Path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		return f, err
	case "https":
	default:
		return nil, fmt.Errorf("%s: only https:// and file:// addresses are read", address)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	// A redirect is followed only to https:// (the thirteenth review): one
	// to plain HTTP would hand the download, and the checksums.txt that
	// vouches for it, to whoever is on the path. Another host is followed —
	// GitHub serves its releases' files from one — as the install script's
	// curl follows it, and the checksum is what the file is held to.
	client := *r.HTTP
	client.CheckRedirect = httpsOnly
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close() // ignored: a response not read
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close() // ignored: a response not read
		return nil, fmt.Errorf("%s answered HTTP %d", address, resp.StatusCode)
	}
	return resp.Body, nil
}

// httpsOnly refuses a redirect to anything but https://, and stops after
// ten, as the HTTP client's own rule does.
func httpsOnly(req *http.Request, via []*http.Request) error {
	switch {
	case req.URL.Scheme != "https":
		return fmt.Errorf("a redirect to %s is not followed: only https:// is", req.URL.Redacted())
	case len(via) >= 10:
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// maxSmall bounds what is read into memory: checksums.txt and the API's list.
const maxSmall = 1 << 20

func (r *Releases) read(ctx context.Context, address string) ([]byte, error) {
	body, err := r.open(ctx, address)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, maxSmall))
}

func (r *Releases) base() string { return strings.TrimRight(r.Base, "/") }

// Latest is the newest stable release: the version in the names of the
// archives `latest/download/checksums.txt` lists.
func (r *Releases) Latest(ctx context.Context) (string, error) {
	sums, err := r.read(ctx, r.base()+"/latest/download/checksums.txt")
	if errors.Is(err, errNotFound) {
		return "", &NoStableError{Candidate: r.newestCandidate(ctx)}
	}
	if err != nil {
		return "", fmt.Errorf("could not read the latest release's checksums.txt: %w", err)
	}
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if v, ok := strings.CutPrefix(name, "tracepad_"); ok {
			if v, _, ok := strings.Cut(v, "_"); ok && IsRelease(v) {
				return v, nil
			}
		}
	}
	return "", errors.New("could not read a version from the latest release's checksums.txt")
}

// newestCandidate names the newest release on GitHub, for the refusal only:
// the API decides nothing else.
func (r *Releases) newestCandidate(ctx context.Context) string {
	if r.Mirror || r.API == "" {
		return ""
	}
	body, err := r.read(ctx, r.API+"/releases?per_page=10")
	if err != nil {
		return ""
	}
	var list []struct {
		Tag string `json:"tag_name"`
	}
	if json.Unmarshal(body, &list) != nil {
		return ""
	}
	for _, rel := range list {
		if v := strings.TrimPrefix(rel.Tag, "v"); IsRelease(v) {
			return v
		}
	}
	return ""
}

// Exists reports whether release v is published: its checksums.txt reads.
func (r *Releases) Exists(ctx context.Context, v string) error {
	_, err := r.read(ctx, fmt.Sprintf("%s/download/v%s/checksums.txt", r.base(), v))
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("there is no release v%s at %s (its checksums.txt is not there); the releases are listed at %s", v, r.base(), githubReleases)
	}
	return err
}

// Fetched is a release's binary on disk, and how it was verified.
type Fetched struct {
	Path     string
	Verified string
}

// Fetch downloads release v's archive for this machine into dir, checks it
// against checksums.txt and its attestation, extracts the binary as
// dir/name, and runs it: it must say it is v (Decision 3).
func (r *Releases) Fetch(ctx context.Context, v, dir, name string) (Fetched, error) {
	release := fmt.Sprintf("%s/download/v%s", r.base(), v)
	sums, err := r.read(ctx, release+"/checksums.txt")
	if errors.Is(err, errNotFound) {
		return Fetched{}, fmt.Errorf("there is no release v%s at %s (its checksums.txt is not there)", v, r.base())
	}
	if err != nil {
		return Fetched{}, fmt.Errorf("could not read v%s's checksums.txt: %w", v, err)
	}
	archive := fmt.Sprintf("tracepad_%s_%s_%s.tar.gz", v, r.OS, r.Arch)
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archive {
			want = strings.ToLower(fields[0])
		}
	}
	if want == "" {
		return Fetched{}, fmt.Errorf("release v%s has no %s in its checksums.txt: no build for %s/%s", v, archive, r.OS, r.Arch)
	}

	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return Fetched{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	body, err := r.open(ctx, release+"/"+archive)
	if err != nil {
		return Fetched{}, fmt.Errorf("could not download %s: %w", archive, err)
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, hash), body)
	body.Close() // ignored: read whole, or not, as io.Copy says; the checksum decides
	if err != nil {
		return Fetched{}, fmt.Errorf("could not download %s: %w", archive, err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return Fetched{}, fmt.Errorf("%s does not match checksums.txt (expected %s, got %s)", archive, want, got)
	}
	verified := "sha256 matches checksums.txt"
	if r.Attest != nil {
		note, err := r.Attest(ctx, tmp.Name())
		if err != nil {
			return Fetched{}, fmt.Errorf("the build attestation of %s did not verify: %w", archive, err)
		}
		verified += "; " + note
	}

	path := filepath.Join(dir, name)
	if err := extractBinary(tmp.Name(), path); err != nil {
		return Fetched{}, fmt.Errorf("%s: %w", archive, err)
	}
	got, err := r.Version(ctx, path)
	switch {
	case err != nil:
		return Fetched{}, fmt.Errorf("the binary in %s does not run here (%v): another architecture, or a noexec mount?", archive, err)
	case got != v:
		return Fetched{}, fmt.Errorf("the binary in %s says it is %q, not %s", archive, got, v)
	}
	return Fetched{Path: path, Verified: verified}, nil
}

// maxBinary bounds the binary extracted from an archive; a release's is tens
// of megabytes.
const maxBinary = 512 << 20

// extractBinary writes the archive's top-level `tracepad` to path, mode 0755.
func extractBinary(archive, path string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("no tracepad in it")
		}
		if err != nil {
			return err
		}
		if strings.TrimPrefix(h.Name, "./") != "tracepad" || h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, maxBinary))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
}

// binaryVersion runs `path version`, for ten seconds at most.
func binaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := retryBusy(func() error {
		out.Reset()
		cmd := child(ctx, path, "version")
		cmd.Env = []string{}
		cmd.Stdout = &out
		return cmd.Run()
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// ghAttest is Releases.Attest with the `gh` on PATH, as the install script
// checks: when it is installed and logged in, a failure refuses the archive.
func ghAttest(ctx context.Context, archive string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "attestation not checked: gh is not installed", nil
	}
	// Logged out is what the install script's rule skips for (#3); a gh that
	// could not be asked is said as that (the audit of #223).
	if err := child(ctx, gh, "auth", "status").Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "attestation not checked: gh is not logged in", nil
		}
		return fmt.Sprintf("attestation not checked: gh could not be asked whether it is logged in (%v)", err), nil
	}
	out, err := child(ctx, gh, "attestation", "verify", archive, "--repo", repo).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return "build attestation verified (gh)", nil
}
