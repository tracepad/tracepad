package upgrade

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Steps a run records, in the order they happen. The way back reads them to
// know what to undo (Decision 11).
const (
	stepPrepared       = "prepared"        // nothing changed yet
	stepStopSent       = "stop_sent"       // SIGTERM sent, or docker stop asked
	stepStopped        = "stopped"         // the old server is down
	stepArchived       = "archived"        // data.tar.gz read back whole
	stepRenamedOld     = "renamed_old"     // container: <name>-before-<run>
	stepBinaryReplaced = "binary_replaced" // the installed binary is the new one
	stepStarted        = "started"         // the new server or container runs
	stepChecked        = "checked"         // the verdict is in `verdict`
	stepSkill          = "skill"           // the skill's copies reinstalled
	stepBackBegun      = "back_begun"      // the way back changed what runs
	stepBackAside      = "back_set_aside"  // what the new version left is set aside
	stepBackMoved      = "back_moved"      // the restore is in the data's place
	stepBackVolume     = "back_volume"     // container: the archive restored into <vol>-<run>
	stepBackBinary     = "back_binary"     // the old binary is back
	stepBackStarted    = "back_started"    // the old version runs again
	stepBackDone       = "back_done"       // the way back finished
)

// Kinds of run.
const (
	kindProcess   = "process"
	kindContainer = "container"
	kindBinary    = "binary"
)

// State is a run's record, state.json in its directory: data, read into this
// struct and never executed (Decision 6).
type State struct {
	Run     string    `json:"run"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created"`
	// From is the running version of the server or container, or the
	// binary's for a run with neither; To is the version gone to.
	From string `json:"from"`
	To   string `json:"to"`

	Binary    *BinaryState    `json:"binary,omitempty"`
	Process   *ProcessState   `json:"process,omitempty"`
	Container *ContainerState `json:"container,omitempty"`

	// CountBefore is the trace count read before the stop; nil when it was
	// not read (no key, or it could not be).
	CountBefore *int64    `json:"count_before,omitempty"`
	Archive     *Archived `json:"archive,omitempty"`

	Steps   []Step `json:"steps"`
	Verdict string `json:"verdict,omitempty"`
	// SetAside is every path and name the run or its way back left for the
	// person to remove.
	SetAside []string `json:"set_aside,omitempty"`
}

// BinaryState is the installed binary's part of a run.
type BinaryState struct {
	Path string `json:"path"`
	// From is what it answered before the run; empty when there was none.
	From string `json:"from"`
	// Old is the copy of it in the run directory, when there is one.
	Old string `json:"old,omitempty"`
}

// ProcessState is a server process's part of a run. Its arguments and
// environment are in server.json beside it, mode 0600, because the
// environment can hold secrets.
type ProcessState struct {
	PID     int    `json:"pid"`
	DataDir string `json:"data_dir"`
	Listen  string `json:"listen"`
	URL     string `json:"url"`
	Log     string `json:"log"`
	// LogOffset is the log's size when the new server started: its first
	// line is the one after it.
	LogOffset int64 `json:"log_offset"`
	// PIDFile is whether <data>/server.pid named the old PID and was moved
	// to the new one.
	PIDFile bool `json:"pid_file"`
	NewPID  int  `json:"new_pid,omitempty"`
	// BackPID is the old version a way back started, or found running.
	BackPID int `json:"back_pid,omitempty"`
	// Old is the copy of the running version in the run directory.
	Old string `json:"old"`
}

// ContainerState is a container's part of a run.
type ContainerState struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Volume   string `json:"volume"`
	URL      string `json:"url"`
	OldRef   string `json:"old_ref"`
	OldImage string `json:"old_image"`
	NewRef   string `json:"new_ref"`
	Restart  string `json:"restart"`
	NewID    string `json:"new_id,omitempty"`
	// BackID is the old image's container a way back ran.
	BackID string `json:"back_id,omitempty"`
}

// Step is one thing a run did, and when.
type Step struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// ServerSpec is server.json: how the old server was started, so it can be
// started again exactly so.
type ServerSpec struct {
	Exe  string   `json:"exe"`
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

func (s *State) has(step string) bool {
	return slices.ContainsFunc(s.Steps, func(st Step) bool { return st.Name == step })
}

// runID is a run directory's name: when, from what, and six random
// characters, so two runs in one second are two directories.
var runID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9A-Za-z.-]+-[a-z0-9]{6}$`)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newRunID(now time.Time, from string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return now.Format("20060102-150405") + "-" + from + "-" + string(b)
}

// stateFile is a run's state on disk.
const stateFile = "state.json"

// save writes the state by rename, so a crash leaves the last whole one.
func (s *State) save(dir string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, stateFile), append(b, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// resolveRun takes `--check` or `--back`'s argument: a run's id, under the
// backups directory, or its directory's path.
func resolveRun(backups, arg string) (string, error) {
	arg = strings.TrimRight(arg, "/")
	if arg == "" {
		return "", errors.New("name the run: its id, or the directory the upgrade printed")
	}
	if !strings.Contains(arg, "/") {
		if !runID.MatchString(arg) {
			return "", fmt.Errorf("%q is not a run's id", arg)
		}
		return filepath.Join(backups, arg), nil
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	if !runID.MatchString(filepath.Base(abs)) {
		return "", fmt.Errorf("%s is not a run's directory", arg)
	}
	return abs, nil
}

// loadState reads a run's state and refuses one that is not this run's or
// whose fields are not what a run writes: a value of another shape stops
// everything, and nothing in the file is ever run.
func loadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, fmt.Errorf("no run at %s: %w", dir, err)
	}
	var s State
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s/%s is not a run's state: %w", dir, stateFile, err)
	}
	if err := s.validate(filepath.Base(dir)); err != nil {
		return nil, fmt.Errorf("%s/%s is not this run's: %w", dir, stateFile, err)
	}
	return &s, nil
}

var (
	restartPolicy = regexp.MustCompile(`^(no|always|unless-stopped|on-failure(:[1-9][0-9]*)?)$`)
	containerID   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dockerName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	imageRef      = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]*$`)
	sha256Hex     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (s *State) validate(dirName string) error {
	switch {
	case s.Run != dirName:
		return fmt.Errorf("its run is %q, and its directory %q", s.Run, dirName)
	case !runID.MatchString(s.Run):
		return fmt.Errorf("its run %q is not a run's id", s.Run)
	case !IsRelease(s.From) || !IsRelease(s.To):
		return fmt.Errorf("its versions %q and %q are not releases'", s.From, s.To)
	case s.Archive != nil && s.Archive.SHA256 != "" && !sha256Hex.MatchString(s.Archive.SHA256):
		return errors.New("its archive's checksum is not one")
	}
	if b := s.Binary; b != nil {
		if !filepath.IsAbs(b.Path) || (b.From != "" && !IsRelease(b.From)) || (b.Old != "" && !filepath.IsAbs(b.Old)) {
			return errors.New("its binary is not one a run records")
		}
	}
	switch s.Kind {
	case kindProcess:
		p := s.Process
		if p == nil || p.PID <= 0 || !filepath.IsAbs(p.DataDir) || !filepath.IsAbs(p.Log) || !filepath.IsAbs(p.Old) {
			return errors.New("its server is not one a run records")
		}
		// The address asked, with the key, is the one the listen address
		// gives, never a field of its own (the security review of #1).
		if url, ok := loopbackURL(p.Listen); !ok || p.URL != url {
			return fmt.Errorf("its server's address %q is not this machine's %q", p.URL, p.Listen)
		}
	case kindContainer:
		c := s.Container
		if c == nil || !dockerName.MatchString(c.Name) || !dockerName.MatchString(c.Volume) ||
			!imageRef.MatchString(c.OldRef) || !imageRef.MatchString(c.NewRef) || !imageRef.MatchString(c.OldImage) ||
			!restartPolicy.MatchString(c.Restart) || !containerID.MatchString(c.ID) || (c.NewID != "" && !containerID.MatchString(c.NewID)) || (c.BackID != "" && !containerID.MatchString(c.BackID)) {
			return errors.New("its container is not one a run records")
		}
		if !isLoopbackBase(c.URL) {
			return fmt.Errorf("its container's address %q is not this machine's", c.URL)
		}
	case kindBinary:
		if s.Binary == nil {
			return errors.New("it has no binary")
		}
	default:
		return fmt.Errorf("its kind %q is not one", s.Kind)
	}
	return nil
}

// isLoopbackBase says whether u is exactly what loopbackBase makes: http, a
// loopback IP and a port, and nothing else — no name, no user, no path.
func isLoopbackBase(u string) bool {
	rest, ok := strings.CutPrefix(u, "http://")
	if !ok {
		return false
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return false
	}
	want, ok := loopbackBase(host, port)
	return ok && want == u
}
