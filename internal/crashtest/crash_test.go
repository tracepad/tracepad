// Package crashtest kills the real binary in the middle of its writes and
// asks what the database is afterwards (spec 002 #15 and #23, spec 043 #42,
// spec 047 #12; Decision 46 of spec 043).
//
// The writer's guarantees — one goroutine, one transaction per window, a 200
// only after the commit — are stated everywhere and were exercised nowhere by
// the one event they exist for. This starts a process on a temporary data
// directory, drives it with ingest on both OTLP doors, scores, trace deletions,
// a user erasure and readers, with the retention sweeper and the statistics
// aggregator on their shortest cadence, and at a random moment sends it
// SIGKILL — its own PID, never a pattern. Then it asks, of the image the kill
// left and again of the server that comes back:
//
//   - the file is sound (`integrity_check`, `foreign_key_check`);
//   - no transaction is half of itself: a trace's counts are its
//     observations', nothing that went with a deleted trace stayed, the search
//     index and its side table agree;
//   - an answer given before the kill is true after it: a 2xx on a write is
//     there, a 200 on a delete is gone, an erasure answered 202 has run to its
//     end by the time the restart has settled;
//   - the aggregator has caught up: the rolled hours count what a scan counts.
//
// It needs a binary. TRACEPAD_BINARY names it; `make crash-test` builds one
// and sets it, and without it the package skips, so `go test ./...` and the
// gate stay what they were. It is not in the gate: it runs a server for a
// minute or more. TRACEPAD_CRASH_ROUNDS is how many kills (default 5),
// TRACEPAD_CRASH_SEED fixes the random choices of a run that failed, and
// TRACEPAD_CRASH_KEEP names a directory a failed run leaves its evidence in.
package crashtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func envInt(name string, fallback int64) int64 {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func TestKillMidWrite(t *testing.T) {
	binary := os.Getenv("TRACEPAD_BINARY")
	if binary == "" {
		t.Skip("TRACEPAD_BINARY is not set; `make crash-test` builds a binary and runs this")
	}
	rounds := int(envInt("TRACEPAD_CRASH_ROUNDS", 5))
	seed := envInt("TRACEPAD_CRASH_SEED", time.Now().UnixNano())
	t.Logf("seed %d, %d rounds (TRACEPAD_CRASH_SEED=%d to repeat the choices)", seed, rounds, seed)
	rng := rand.New(rand.NewSource(seed))

	root, err := os.MkdirTemp("", "tracepad-crash-")
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	logPath := filepath.Join(root, "server.log")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			os.RemoveAll(root)
			return
		}
		if keep := os.Getenv("TRACEPAD_CRASH_KEEP"); keep != "" {
			dest := filepath.Join(keep, filepath.Base(root))
			_ = os.MkdirAll(keep, 0o700)
			if err := os.Rename(root, dest); err == nil {
				t.Logf("evidence kept in %s", dest)
				return
			}
		}
		t.Logf("evidence kept in %s", root)
	})

	led := newLedger()
	var mainID string

	// The first start of a database is a write too: the migrations, the
	// bootstrap. A kill in its middle must leave a database the next start
	// finishes. Short kills a few times before anything has been asked of it.
	for i := 0; i < 2; i++ {
		srv := startServer(t, binary, dataDir, logPath)
		time.Sleep(time.Duration(rng.Intn(350)) * time.Millisecond)
		srv.kill()
		t.Logf("startup kill %d, %v after the start", i+1, time.Since(srv.started).Round(time.Millisecond))
		checkSnapshot(t, dataDir, filepath.Join(root, fmt.Sprintf("snap-s%d", i+1)), nil)
	}

	var srv *server
	for round := 1; round <= rounds; round++ {
		// Between processes, never beside one: a second writer on the file
		// would be the very thing the server's lock exists to refuse. Every
		// other round only, because opening the file recovers its WAL — the
		// rounds in between hand the server the image the kill left.
		if round%2 == 0 {
			backdateRetained(t, dataDir)
		}
		srv = startServer(t, binary, dataDir, logPath)
		srv.waitHealthy(60 * time.Second)

		if mainID == "" {
			mainID = projectID(t, srv, mainSecret)
			setRetention(t, srv, projectID(t, srv, retSecret))
		}
		if round > 1 {
			// The server that came back has to finish what the last one
			// left: its erasures, its rolls.
			settle(t, srv, dataDir, led, mainID, fmt.Sprintf("restart %d", round-1))
		}
		w := &workload{
			host: srv.host, led: led, rng: rand.New(rand.NewSource(rng.Int63())),
			client:     &http.Client{Timeout: 10 * time.Second},
			round:      round,
			regular:    []string{"u1", "u2", "u3", "u4"},
			projectIDs: map[string]string{"main": mainID},
		}
		led.mu.Lock()
		for i := 0; i < 3; i++ {
			led.eraseUsers = append(led.eraseUsers, fmt.Sprintf("e-%d-%d", round, i))
		}
		led.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		wait := w.run(ctx)
		// Most kills come early, where a window or an erasure chunk is in
		// flight; one in four lets the sweeper and the aggregator have
		// several of their own passes first.
		delay := time.Duration(150+rng.Intn(1500)) * time.Millisecond
		if rng.Intn(4) == 0 {
			delay += time.Duration(2+rng.Intn(3)) * time.Second
		}
		time.Sleep(delay)
		acked, sent := w.acked.Load(), w.sent.Load()
		srv.kill()
		cancel()
		wait()
		t.Logf("round %d: SIGKILL after %v; %d exports acknowledged, %d requests sent",
			round, delay.Round(time.Millisecond), acked, sent)

		checkSnapshot(t, dataDir, filepath.Join(root, fmt.Sprintf("snap-%d", round)), led)
	}

	// One more start, to the end: what the last kill left has to come up and
	// settle, and an operator's stop has to be a clean one.
	srv = startServer(t, binary, dataDir, logPath)
	srv.waitHealthy(60 * time.Second)
	settle(t, srv, dataDir, led, mainID, "the final start")
	if err := srv.stop(); err != nil {
		t.Errorf("SIGTERM after the last restart: %v\n%s", err, tail(logPath, 3000))
	}
	t.Logf("%d traces acknowledged over the run, %d scores, %d deletions answered, %d erasures answered",
		len(led.traces), len(led.scores), len(led.deleteAcked), len(led.erasureID))
}

// checkSnapshot copies the data directory the kill left, opens the copy —
// SQLite recovers the WAL as it opens — and asks what must hold of it at once,
// with no server to finish anything.
func checkSnapshot(t *testing.T, dataDir, into string, led *ledger) {
	t.Helper()
	path := snapshot(t, dataDir, into)
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	defer db.Close()
	var v violations
	v = append(v, structural(db)...)
	if led != nil {
		v = append(v, durable(db, led, false)...)
		t.Logf("  the kill left %s", whatWasInterrupted(db))
	}
	if len(v) > 0 {
		t.Fatalf("after the kill (snapshot %s):\n  %s", into, strings.Join(v.cap(), "\n  "))
	}
}

// settle waits for the restarted server to finish what it resumed and then
// asks the questions that need it to have: erasures run to their end, and the
// rollup caught up with the rows. It reads the live file, which a server in
// WAL mode lets a reader do.
func settle(t *testing.T, srv *server, dataDir string, led *ledger, mainID, when string) {
	t.Helper()
	db, err := openDB(filepath.Join(dataDir, "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	deadline := time.Now().Add(90 * time.Second)
	for {
		running := erasuresRunning(t, srv, mainID)
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d erasures are still queued or running 90s after the restart:\n%s",
				when, running, tail(filepath.Join(filepath.Dir(dataDir), "server.log"), 3000))
		}
		time.Sleep(250 * time.Millisecond)
	}
	failedErasures(t, srv, mainID, when)

	// The aggregator's own clock says when it has been round since the
	// restart: a pass whose cutoff is later than the start has seen every
	// row the kill left.
	waitPass(t, db, mainID, srv.started, when)

	var v violations
	v = append(v, structural(db)...)
	v = append(v, durable(db, led, true)...)
	v = append(v, settled(db, mainID)...)
	if len(v) > 0 {
		t.Fatalf("%s:\n  %s", when, strings.Join(v.cap(), "\n  "))
	}
}

func waitPass(t *testing.T, db *sql.DB, projectID string, since time.Time, when string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var last sql.NullInt64
		err := db.QueryRow(`SELECT last_pass FROM stats_rollup WHERE project_id = ?`, projectID).Scan(&last)
		if err == nil && last.Valid && last.Int64 > since.UnixNano() {
			// One more cadence: a pass that started before the last
			// row of the restart's own settling is not the last word.
			time.Sleep(1500 * time.Millisecond)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: the aggregator has made no pass in 45s since the restart", when)
}

// --- the API, as the test uses it ----------------------------------------

func apiGet(t *testing.T, srv *server, path, secret string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", srv.host+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func projectID(t *testing.T, srv *server, secret string) string {
	t.Helper()
	status, data := apiGet(t, srv, "/api/v1/projects", secret)
	var out struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if status != 200 || json.Unmarshal(data, &out) != nil || len(out.Projects) != 1 {
		t.Fatalf("GET /api/v1/projects: %d %s", status, data)
	}
	return out.Projects[0].ID
}

// setRetention gives `ret` a one-day window. Its rows are backdated past it
// by the test, so the sweeper has something to take.
func setRetention(t *testing.T, srv *server, id string) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", srv.host+"/api/v1/projects/"+id+"?confirm=ret",
		strings.NewReader(`{"retention_days": 1}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PATCH retention: %d %s", resp.StatusCode, body)
	}
}

// backdateRetained moves the rows of `ret` three days into the past, which is
// done to the file between processes, and only by the test.
func backdateRetained(t *testing.T, dataDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "tracepad.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE traces SET ingested_at = ingested_at - ?
	                       WHERE project_id = (SELECT id FROM projects WHERE name = 'ret')
	                         AND ingested_at > ?`,
		int64(3*24*time.Hour), time.Now().Add(-24*time.Hour).UnixNano()); err != nil {
		t.Logf("backdate: %v (a busy database is not a finding)", err)
	}
}

type erasureList struct {
	Erasures []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Error any    `json:"error"`
	} `json:"erasures"`
}

func listErasures(t *testing.T, srv *server, mainID string) erasureList {
	t.Helper()
	status, data := apiGet(t, srv, "/api/v1/projects/"+mainID+"/erasures", mainSecret)
	var out erasureList
	if status != 200 || json.Unmarshal(data, &out) != nil {
		t.Fatalf("list erasures: %d %s", status, data)
	}
	return out
}

func erasuresRunning(t *testing.T, srv *server, mainID string) int {
	n := 0
	for _, e := range listErasures(t, srv, mainID).Erasures {
		if e.State == "queued" || e.State == "running" {
			n++
		}
	}
	return n
}

func failedErasures(t *testing.T, srv *server, mainID, when string) {
	t.Helper()
	for _, e := range listErasures(t, srv, mainID).Erasures {
		if e.State == "failed" {
			t.Errorf("%s: erasure %s ended failed: %v", when, e.ID, e.Error)
		}
	}
}

// whatWasInterrupted says, for the log, what the file holds that a kill in
// the middle of work leaves behind: erasures not finished, a retained project
// not yet swept. A run whose kills never find any is a run that proved little,
// and this is how that is seen.
func whatWasInterrupted(db *sql.DB) string {
	var open, attempts int
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(attempts), 0) FROM erasures
	                  WHERE state IN ('queued', 'running')`).Scan(&open, &attempts)
	var retained int
	_ = db.QueryRow(`SELECT COUNT(*) FROM traces
	                  WHERE project_id = (SELECT id FROM projects WHERE name = 'ret')`).Scan(&retained)
	var pending int
	_ = db.QueryRow(`SELECT COUNT(*) FROM traces
	                  WHERE project_id = (SELECT id FROM projects WHERE name = 'ret')
	                    AND ingested_at < ?`, time.Now().Add(-48*time.Hour).UnixNano()).Scan(&pending)
	return fmt.Sprintf("%d erasures unfinished, %d retained traces of which %d past their window", open, retained, pending)
}
