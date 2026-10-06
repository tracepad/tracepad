package upgrade

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Verdicts (Decision 9).
const (
	verdictHealthy    = "healthy"
	verdictNotHealthy = "not_healthy"
	verdictDecide     = "decide"
)

// Checked is a check's verdict and its evidence.
type Checked struct {
	Verdict string `json:"verdict"`
	// Why is the verdict in a sentence.
	Why     string `json:"why"`
	Health  string `json:"health_version,omitempty"`
	LogLine string `json:"log_first_line,omitempty"`
	Before  *int64 `json:"traces_before,omitempty"`
	After   *int64 `json:"traces_after,omitempty"`
	// CountNote says why the counts were not compared, when they were not.
	CountNote string `json:"count_note,omitempty"`
}

// check asks the server at base, while alive says it runs, whether it is
// version want, and compares its trace count with before (Decision 9).
func (r *runner) check(ctx context.Context, base, want string, before *int64, alive func() bool) Checked {
	c := Checked{Before: before}
	if before == nil {
		// Said whatever the verdict, in as many words: a check that
		// returns before it reads a count says it too (the fourth review
		// of #228), and the live run of 0.1.0 found "no TRACEPAD_API_KEY
		// in the environment" alone unclear.
		why := "the count before the upgrade was not read"
		if !r.hasKey() {
			why = noKey
		}
		c.CountNote = "the trace counts were not compared: " + why
	}
	deadline := r.deps.Now().Add(r.deps.HealthWait)
	answered := false
	for {
		if !alive() {
			c.Verdict, c.Why = verdictNotHealthy, "the server exited"
			if v, err := health(ctx, r.deps.HTTP, base); err == nil {
				c.Health = v
				c.Why += fmt.Sprintf(", and something else answers at %s as %s", base, v)
			}
			return c
		}
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		v, err := health(hctx, r.deps.HTTP, base)
		cancel()
		if err == nil {
			// Any answer is the verdict's: one as another version does not
			// turn into want by waiting, and meanwhile what answers runs on
			// the data (the twelfth review).
			c.Health, answered = v, true
			break
		}
		if !r.deps.Now().Before(deadline) {
			break
		}
		if err := r.deps.Sleep(ctx, time.Second); err != nil {
			c.Verdict, c.Why = verdictDecide, "the check was interrupted"
			return c
		}
	}
	switch {
	case answered && c.Health != want:
		c.Verdict, c.Why = verdictNotHealthy, fmt.Sprintf("%s answers as %s, not %s", base, c.Health, want)
		return c
	case !answered:
		c.Verdict = verdictDecide
		c.Why = fmt.Sprintf("the server runs but has not answered at %s in %s; a migration of a large database runs before it listens", base, r.deps.HealthWait)
		return c
	}
	after, note := r.traceCount(ctx, base)
	c.After = after
	switch {
	case before == nil:
		c.Verdict, c.Why = verdictHealthy, "it answers as "+want
	case c.After == nil:
		c.Verdict, c.CountNote = verdictDecide, note
		c.Why = "it answers as " + want + ", but its trace count, read before the upgrade, cannot be read now (" + note + ")"
	case *c.After < *before:
		c.Verdict = verdictDecide
		c.Why = fmt.Sprintf("it answers as %s, but it counts %d traces where there were %d", want, *c.After, *before)
	default:
		c.Verdict, c.Why = verdictHealthy, fmt.Sprintf("it answers as %s, with %d traces where there were %d", want, *c.After, *before)
	}
	return c
}

// noKey is why there is no count when no key was given.
const noKey = "no TRACEPAD_API_KEY in the environment"

// hasKey says whether a trace count can be asked for: whoever needs to know
// asks this, not traceCount's prose.
func (r *runner) hasKey() bool { return r.deps.Getenv("TRACEPAD_API_KEY") != "" }

// traceCount reads `database.rows.traces` of /api/v1/system with the key in
// TRACEPAD_API_KEY, which is read from the environment and never put on a
// command line. note says why there is no count.
func (r *runner) traceCount(ctx context.Context, base string) (*int64, string) {
	if !r.hasKey() {
		return nil, noKey
	}
	key := r.deps.Getenv("TRACEPAD_API_KEY")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/system", nil)
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := r.deps.HTTP.Do(req)
	if err != nil {
		return nil, "/api/v1/system did not answer"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("/api/v1/system answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		Database struct {
			Rows struct {
				Traces *int64 `json:"traces"`
			} `json:"rows"`
		} `json:"database"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.Database.Rows.Traces == nil {
		return nil, "/api/v1/system says no trace count for this key"
	}
	return body.Database.Rows.Traces, ""
}

// firstLogLine is the first line a server wrote to its log after offset: the
// start's label, `tracepad X (commit)` (spec 001 #25).
func firstLogLine(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	line, _ := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n') // ignored: a line cut short by the end of the log is the line there is
	return strings.TrimSpace(line)
}
