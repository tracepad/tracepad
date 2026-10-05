package upgrade

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tracepad/tracepad/internal/termsafe"
)

// Exit statuses (Decision 13).
const (
	exitOK       = 0
	exitRefused  = 1
	exitUsage    = 2
	exitWentBack = 3
	exitDecide   = 4
	exitStuck    = 5
	exitPending  = 10
)

// Statuses, beside the exit status, for an agent reading --json.
var statusOf = map[int]string{
	exitOK:       "ok",
	exitRefused:  "refused",
	exitUsage:    "usage",
	exitWentBack: "went_back",
	exitDecide:   "decide",
	exitStuck:    "stuck",
	exitPending:  "pending",
}

// Report is what every mode prints: sentences for a person, or this object
// with --json.
type Report struct {
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	// Summary is the outcome in one sentence.
	Summary string  `json:"summary"`
	From    string  `json:"from,omitempty"`
	To      string  `json:"to,omitempty"`
	Run     *RunRef `json:"run,omitempty"`

	Binary     *BinaryReport     `json:"binary,omitempty"`
	Servers    []ServerReport    `json:"servers"`
	Containers []ContainerReport `json:"containers"`
	Probe      *Probe            `json:"probe,omitempty"`
	Check      *Checked          `json:"check,omitempty"`
	// BackCheck is the check of the old version after a way back.
	BackCheck *Checked `json:"back_check,omitempty"`

	// Plan is what the upgrade would do, in order (--plan).
	Plan []string `json:"plan,omitempty"`
	// Done is what this invocation did, in order.
	Done []string `json:"done,omitempty"`
	// SetAside is what is left for the person to remove, with the commands
	// in Person.
	SetAside []string `json:"set_aside,omitempty"`
	// Person is what only the person can do or decide, each with its
	// commands.
	Person []string `json:"person,omitempty"`
	// Next is the commands that come next.
	Next  []string `json:"next,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

type RunRef struct {
	ID  string `json:"id"`
	Dir string `json:"dir"`
}

type BinaryReport struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Whose   string `json:"whose"`
	Reason  string `json:"reason,omitempty"`
	First   string `json:"first_on_path,omitempty"`
}

type ServerReport struct {
	PID     int      `json:"pid"`
	Command []string `json:"command"`
	Exe     string   `json:"exe"`
	DataDir string   `json:"data_dir,omitempty"`
	Listen  string   `json:"listen,omitempty"`
	Version string   `json:"version,omitempty"`
	Whose   string   `json:"whose"`
	Reason  string   `json:"reason,omitempty"`
	Target  bool     `json:"target,omitempty"`
}

type ContainerReport struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Volume  string `json:"volume,omitempty"`
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
	Whose   string `json:"whose"`
	Reason  string `json:"reason,omitempty"`
	Target  bool   `json:"target,omitempty"`
}

func whose(ours bool) string {
	if ours {
		return "command"
	}
	return "person"
}

func (rep *Report) fill(f Findings) {
	b := f.Binary
	rep.Binary = &BinaryReport{Path: b.Path, Version: b.Version, Whose: whose(b.Ours), Reason: b.Reason, First: b.First}
	rep.Servers = []ServerReport{}
	for _, s := range f.Servers {
		rep.Servers = append(rep.Servers, ServerReport{PID: s.Proc.PID, Command: s.Proc.Argv, Exe: s.Proc.Exe,
			DataDir: s.DataDir, Listen: s.Listen, Version: s.Version, Whose: whose(s.Ours), Reason: s.Reason})
	}
	rep.Containers = []ContainerReport{}
	for _, c := range f.Containers {
		rep.Containers = append(rep.Containers, ContainerReport{Name: c.Name, Image: c.Ref, Volume: c.Volume,
			URL: c.URL, Version: c.Version, Whose: whose(c.Ours), Reason: c.Reason})
	}
	if f.Probe.Version != "" {
		p := f.Probe
		rep.Probe = &p
	}
	rep.Notes = append(rep.Notes, f.Notes...)
}

// write prints the report: JSON, or text for a person with every line
// through termsafe, since a command line, a version a server reported or a
// container's name is another program's text.
func (rep *Report) write(w io.Writer, asJSON bool) {
	rep.Status = statusOf[rep.ExitCode]
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("%s", rep.Summary)
	if rep.Run != nil {
		line("  run       %s", rep.Run.Dir)
	}
	if rep.Binary != nil {
		bin := rep.Binary
		v := bin.Version
		if v == "" {
			v = "none"
		}
		line("  binary    %s (%s)%s", bin.Path, v, reasonSuffix(bin.Whose, bin.Reason))
		if bin.First != "" {
			line("            another tracepad comes first on PATH: %s", bin.First)
		}
	}
	for _, s := range rep.Servers {
		mark := ""
		if s.Target {
			mark = " ← this run"
		}
		line("  server    pid %d, %s, data %s, %s%s%s", s.PID, orNone(s.Version), s.DataDir, s.Listen, mark, reasonSuffix(s.Whose, s.Reason))
		line("            %s", strings.Join(s.Command, " "))
	}
	for _, c := range rep.Containers {
		mark := ""
		if c.Target {
			mark = " ← this run"
		}
		line("  container %s, %s, %s%s%s", c.Name, c.Image, orNone(c.Version), mark, reasonSuffix(c.Whose, c.Reason))
	}
	if p := rep.Probe; p != nil {
		line("  %s answers as %s: %s", p.URL, p.Version, p.Whose)
	}
	for _, cc := range []struct {
		title string
		c     *Checked
	}{{"Check", rep.Check}, {"Check after the way back", rep.BackCheck}} {
		c := cc.c
		if c == nil {
			continue
		}
		line("")
		line("%s: %s", cc.title, c.Why)
		if c.LogLine != "" {
			line("  first log line  %s", c.LogLine)
		}
		if c.Before != nil || c.After != nil {
			line("  traces          %s before, %s after", countText(c.Before), countText(c.After))
		}
		if c.CountNote != "" {
			line("  counts          %s", c.CountNote)
		}
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		line("")
		line("%s", title)
		for _, it := range items {
			line("  - %s", it)
		}
	}
	section("The plan:", rep.Plan)
	section("Done:", rep.Done)
	section("Set aside, kept until you remove it:", rep.SetAside)
	section("Yours:", rep.Person)
	section("Next:", rep.Next)
	section("Notes:", rep.Notes)
	_, _ = io.WriteString(w, termsafe.Text(b.String()))
}

func reasonSuffix(who, reason string) string {
	if who == "command" {
		return ""
	}
	if reason == "" {
		return " — yours"
	}
	return " — yours: " + reason
}

func orNone(v string) string {
	if v == "" {
		return "no answer"
	}
	return v
}

func countText(n *int64) string {
	if n == nil {
		return "?"
	}
	return fmt.Sprint(*n)
}
