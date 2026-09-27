package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// A project's own view of a body (spec 041 #25, #26, #27): the bytes are
// shared, and the type, the start of the hold, whether the project holds the
// body at all and what a deletion releases are each the project's own.

// holderSets answers the (sha256, project) pairs with a holder row, and those
// with a ref of the project in either ref table.
func (f *sweepFixture) holderSets(t *testing.T) (holders, refs []string) {
	t.Helper()
	read := func(query string) []string {
		rows, err := f.store.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var sha, project string
			if err := rows.Scan(&sha, &project); err != nil {
				t.Fatal(err)
			}
			out = append(out, sha[:8]+"/"+project)
		}
		sort.Strings(out)
		return out
	}
	return read(`SELECT sha256, project_id FROM media_holders`),
		read(`SELECT sha256, project_id FROM media_refs
		      UNION SELECT sha256, project_id FROM media_raw_refs`)
}

// checkHolders fails unless every pair with a ref has a holder row and every
// holder row has a ref.
func (f *sweepFixture) checkHolders(t *testing.T, after string) {
	t.Helper()
	holders, refs := f.holderSets(t)
	if strings.Join(holders, ",") != strings.Join(refs, ",") {
		t.Errorf("after %s: holders %v, refs %v", after, holders, refs)
	}
}

// Project A stores X as image/png; later project B stores the same bytes as
// image/webp. Each is answered its own type and its own time (#25).
func TestMediaTypePerProject(t *testing.T) {
	f := newSweepFixture(t)
	other := f.secondProject(t, "other")
	x := mediaBody(40, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(3), x, false)
	asWebp := x
	asWebp.MimeType = "image/webp"
	f.arriveWithMedia(t, other.ID, hexTrace(2), daysAgo(1), asWebp, false)

	for _, c := range []struct {
		project string
		mime    string
		at      int64
	}{
		{f.project.ID, "image/png", daysAgo(3)},
		{other.ID, "image/webp", daysAgo(1)},
	} {
		file, err := f.store.MediaFor(c.project, x.SHA256)
		if err != nil || file == nil || file.MimeType != c.mime || len(file.Body) != 4096 {
			t.Errorf("MediaFor(%s) = %+v, %v; want %s", c.project, file, err, c.mime)
		}
		info, err := f.store.MediaByLangfuseID(c.project, MediaIDFor(x.SHA256))
		if err != nil || info == nil {
			t.Fatalf("MediaByLangfuseID(%s) = %+v, %v", c.project, info, err)
		}
		if info.MimeType != c.mime || info.CreatedAt != c.at || info.Size != 4096 {
			t.Errorf("MediaByLangfuseID(%s) = %s at %d, want %s at %d",
				c.project, info.MimeType, info.CreatedAt, c.mime, c.at)
		}
	}
	f.checkHolders(t, "two projects storing one body")
}

// Every path that writes or deletes refs keeps the holder rows equal to the
// pairs with a ref (#25): ingest, inline and resolved; the upload; the null
// answer; the pending sweep; retention of traces and of raw batches;
// erasure; trace deletion; a purge.
func TestMediaHoldersFollowRefs(t *testing.T) {
	f := newSweepFixture(t)
	other := f.secondProject(t, "other")
	shared := mediaBody(41, 4096)
	rawOnly := mediaBody(42, 4096)
	uploaded := mediaBody(43, 4096)
	orphan := mediaBody(44, 4096)

	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(40), shared, true)
	f.arriveWithMedia(t, other.ID, hexTrace(2), daysAgo(1), shared, true)
	f.checkHolders(t, "inline ingest")

	// Resolved: a Langfuse id this project holds, written with no body.
	resolved := &IngestBatch{
		ProjectID: f.project.ID, IngestedAt: daysAgo(1),
		Traces: []*model.Trace{{ID: hexTrace(3), UserID: "u2"}},
		Observations: []*model.Observation{{
			TraceID: hexTrace(3), ID: hexTrace(3)[:16], Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: daysAgo(1), EndTime: daysAgo(1) + 1,
		}},
		MediaRefs: []MediaRef{{SHA256: shared.SHA256, TraceID: hexTrace(3)}},
		Resolved:  []string{shared.SHA256},
	}
	if err := f.writer.Submit(t.Context(), resolved); err != nil {
		t.Fatal(err)
	}
	f.checkHolders(t, "a resolved ingest")

	for _, upload := range []*MediaUpload{
		uploadOf(f.project.ID, hexTrace(4), "tp-pk-test", uploaded, daysAgo(1)),
		uploadOf(other.ID, hexTrace(5), "tp-pk-other", orphan, daysAgo(3)),
	} {
		if err := f.writer.Submit(t.Context(), upload); err != nil {
			t.Fatal(err)
		}
	}
	f.checkHolders(t, "the upload")
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: uploaded.SHA256, TraceID: hexTrace(6)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("null answer = %v, %v", add.Held, err)
	}
	f.checkHolders(t, "the null answer")

	// A raw-only hold: a batch with no trace ref.
	rawBatch := &IngestBatch{
		ProjectID: other.ID, IngestedAt: daysAgo(40),
		Media:    []MediaBody{rawOnly},
		Raw:      &RawBatch{ReceivedAt: daysAgo(40), Body: []byte("factored")},
		RawMedia: []string{rawOnly.SHA256},
	}
	if err := f.writer.Submit(t.Context(), rawBatch); err != nil {
		t.Fatal(err)
	}
	f.checkHolders(t, "a raw-only batch")

	// The pending sweep drops other's ref for a trace that never came;
	// retention (thirty days, both windows) takes this project's first
	// trace and batch, and other's raw-only batch.
	thirty := 30
	f.setRetention(t, f.project.ID, &thirty, &thirty)
	f.setRetention(t, other.ID, &thirty, &thirty)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM media_raw_refs WHERE sha256 = ?`, rawOnly.SHA256) != 0 {
		t.Fatal("the fixture's raw batch outlived its window")
	}
	f.checkHolders(t, "the sweep")

	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "u2", Confirm: "u2", Limit: 500}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	f.checkHolders(t, "an erasure")

	del := &TraceDelete{ProjectID: other.ID, IDs: []string{hexTrace(2)}, Confirm: hexTrace(2)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	f.checkHolders(t, "a trace deletion")

	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`, daysAgo(10), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.store.ProjectByID(context.Background(), f.project.ID); p != nil {
		t.Fatal("the project was not purged")
	}
	f.checkHolders(t, "a purge")
	if got := f.count(t, `SELECT COUNT(*) FROM media_holders WHERE project_id = ?`, f.project.ID); got != 0 {
		t.Errorf("%d holder rows of the purged project survive", got)
	}
}

// The scope is seeks on (sha256, project_id) in the holder and ref tables and
// nothing else: never the caller's raw batches, never the body (#26).
func TestMediaScopeSeeksOnly(t *testing.T) {
	f := newSweepFixture(t)
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := f.store.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
		}
		return strings.Join(lines, "\n")
	}
	// Each step is named by its table alias and what it must seek on,
	// not by SQLite's exact wording, which moves between versions: the
	// holder and both ref tables by the pair, the body by its hash.
	type seek struct{ alias, index, key string }
	pair := []seek{
		{"r", "", "project_id=?"},
		{"rr", "idx_media_raw_refs_holder", "project_id=?"},
		{"m", "", "sha256=?"},
	}
	for _, c := range []struct {
		name  string
		plan  string
		seeks []seek
	}{
		{"the scope", plan(mediaScope, strings.Repeat("a", 64), "p"),
			append([]seek{{"h", "", "project_id=?"}}, pair...)},
		{"the read", plan(mediaRead, strings.Repeat("a", 64), "p"),
			append([]seek{{"h", "", "project_id=?"}}, pair...)},
		// Within the project's own rows: a hash only other projects hold
		// reads none of theirs.
		{"a Langfuse id", plan(mediaByID, "p", "aa", "aag"),
			append([]seek{{"h", "idx_media_holders_project", "project_id=?"}}, pair...)},
	} {
		lines := strings.Split(c.plan, "\n")
		for _, want := range c.seeks {
			found := false
			for _, line := range lines {
				if strings.HasPrefix(line, "SEARCH "+want.alias+" ") &&
					strings.Contains(line, want.index) && strings.Contains(line, want.key) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no seek of %s on %s %s:\n%s", c.name, want.alias, want.index, want.key, c.plan)
			}
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "SCAN") || strings.Contains(line, "raw_batches") {
				t.Errorf("%s: plan step %q is not a seek on the pair", c.name, line)
			}
		}
	}
}

// A ref with no hold behind it — only a hand-edited database leaves one — is
// refused by every read, and the hourly look restores the hold as
// octet-stream, never another project's type (#26).
func TestMediaMissingHoldIsRestored(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(56, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), x, false)
	f.arriveWithMedia(t, b.ID, hexTrace(2), daysAgo(1), x, false)
	if _, err := f.store.db.Exec(`DELETE FROM media_holders WHERE project_id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if file, err := f.store.MediaFor(b.ID, x.SHA256); err != nil || file != nil {
		t.Errorf("a ref with no hold read the body: %+v, %v", file, err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	file, err := f.store.MediaFor(b.ID, x.SHA256)
	if err != nil || file == nil || file.MimeType != "application/octet-stream" {
		t.Errorf("after the sweep B reads %+v, %v; want its body as application/octet-stream", file, err)
	}
	f.checkHolders(t, "the sweep")
}

// A hold written with no type of its own in hand is never another project's
// type (#25): the body's stored type is A's, and B's hold is octet-stream.
func TestMediaHoldWithoutATypeIsNotAnothers(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(49, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), x, false)
	hold := writeJob(func(tx *sql.Tx) error { return holdMedia(tx, b.ID, x.SHA256, "", daysAgo(1)) })
	if err := f.writer.Submit(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	var mime string
	if err := f.store.db.QueryRow(`SELECT mime_type FROM media_holders WHERE sha256 = ? AND project_id = ?`,
		x.SHA256, b.ID).Scan(&mime); err != nil {
		t.Fatal(err)
	}
	if mime != "application/octet-stream" {
		t.Errorf("B's hold without a type = %q, want application/octet-stream, never A's image/png", mime)
	}
}

// A holder row with no ref of its project behind it — only a hand-edited
// database leaves one — is refused by every read and counted by nothing, and
// the hourly look takes it (#26); the other project's hold stays.
func TestMediaStaleHoldIsSwept(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(53, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), x, false)
	if _, err := f.store.db.Exec(`INSERT INTO media_holders (sha256, project_id, mime_type, first_at)
		VALUES (?, ?, 'image/png', 1)`, x.SHA256, b.ID); err != nil {
		t.Fatal(err)
	}
	if file, err := f.store.MediaFor(b.ID, x.SHA256); err != nil || file != nil {
		t.Errorf("a hold with no ref read the body: %+v, %v", file, err)
	}
	if summary, err := f.store.MediaSummary(b.ID); err != nil || summary.Count != 0 {
		t.Errorf("a hold with no ref is counted: %+v, %v", summary, err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM media_holders WHERE project_id = ?`, b.ID); got != 0 {
		t.Errorf("%d stale holds survive the sweep", got)
	}
	if file, err := f.store.MediaFor(f.project.ID, x.SHA256); err != nil || file == nil {
		t.Fatalf("the sweep took the holding project's body: %v", err)
	}
	f.checkHolders(t, "the sweep")
}

// A raw ref written without its project is refused by the database (#26):
// the column's default is only there because SQLite adds a NOT NULL column
// with one.
func TestMediaRawRefNeedsAProject(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(54, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), x, true)
	var batch int64
	if err := f.store.db.QueryRow(`SELECT id FROM raw_batches LIMIT 1`).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	other := mediaBody(55, 4096)
	if _, err := f.store.db.Exec(`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES (?, 'image/png', 1, X'00', 0)`,
		other.SHA256); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.db.Exec(`INSERT INTO media_raw_refs (sha256, raw_batch_id) VALUES (?, ?)`, other.SHA256, batch)
	if err == nil || !strings.Contains(err.Error(), "project_id is required") {
		t.Errorf("a raw ref without a project = %v, want refused", err)
	}
}

// writeJob runs a function as a writer job.
type writeJob func(tx *sql.Tx) error

func (j writeJob) apply(tx *sql.Tx) error { return j(tx) }

// A body only a raw batch of B holds is B's (#12); to A, which holds nothing,
// it looks like a hash nobody holds.
func TestMediaRawOnlyHoldIsScoped(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(45, 4096)
	f.arriveWithMedia(t, b.ID, hexTrace(1), daysAgo(1), x, true)
	erase := &UserDataErase{ProjectID: b.ID, UserID: "u1", Confirm: "u1", Limit: 500}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM media_refs`) != 0 {
		t.Fatal("the trace's ref survived the erasure")
	}
	if file, err := f.store.MediaFor(b.ID, x.SHA256); err != nil || file == nil {
		t.Fatalf("B lost its raw-only body: %v", err)
	}
	for _, sha := range []string{x.SHA256, mediaBody(46, 10).SHA256} {
		if file, err := f.store.MediaFor(f.project.ID, sha); err != nil || file != nil {
			t.Errorf("A read %s: %+v, %v", sha[:8], file, err)
		}
		if info, err := f.store.MediaByLangfuseID(f.project.ID, MediaIDFor(sha)); err != nil || info != nil {
			t.Errorf("A resolved %s: %+v, %v", sha[:8], info, err)
		}
	}
}

// A Langfuse string resolved while B held X, written after B's last ref went
// while A still holds X, is refused: B would otherwise hold X again without
// having sent the bytes (#26).
func TestMediaResolvedHoldGone(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(47, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), x, false)
	f.arriveWithMedia(t, b.ID, hexTrace(2), daysAgo(1), x, false)
	if info, err := f.store.MediaByLangfuseID(b.ID, MediaIDFor(x.SHA256)); err != nil || info == nil {
		t.Fatalf("B does not hold X before the race: %v", err)
	}
	del := &TraceDelete{ProjectID: b.ID, IDs: []string{hexTrace(2)}, Confirm: hexTrace(2)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	batch := &IngestBatch{
		ProjectID: b.ID,
		Traces:    []*model.Trace{{ID: hexTrace(3)}},
		MediaRefs: []MediaRef{{SHA256: x.SHA256, TraceID: hexTrace(3)}},
		Resolved:  []string{x.SHA256},
	}
	if err := f.writer.Submit(t.Context(), batch); !errors.Is(err, ErrMediaGone) {
		t.Fatalf("submit = %v, want ErrMediaGone", err)
	}
	if file, _ := f.store.MediaFor(b.ID, x.SHA256); file != nil {
		t.Error("B reads X again without having sent it")
	}
	// The same race on the null answer: no ref, and the handler asks for
	// the bytes.
	add := &MediaRefAdd{ProjectID: b.ID, SHA256: x.SHA256, TraceID: hexTrace(4)}
	if err := f.writer.Submit(t.Context(), add); err != nil || add.Held {
		t.Fatalf("null answer after B's hold went = %v, %v; want the bytes asked for", add.Held, err)
	}
	f.checkHolders(t, "the refused writes")
}

// A and B both hold X. What B's deletions answer is what B stops holding,
// whatever A keeps (#27): every dry run and every confirmed answer.
func TestMediaDeletionCountsAreTheProjects(t *testing.T) {
	f := newSweepFixture(t)
	b := f.secondProject(t, "b")
	x := mediaBody(48, 5000)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(40), x, false)
	f.arriveWithMedia(t, b.ID, hexTrace(2), daysAgo(40), x, false)
	f.arriveWithMedia(t, b.ID, hexTrace(3), daysAgo(40), x, false)
	want := func(what string, counts DeleteCounts) {
		t.Helper()
		if counts.Media != 1 || counts.MediaBytes != 5000 {
			t.Errorf("%s = %d bodies, %d bytes; want the one B stops holding", what, counts.Media, counts.MediaBytes)
		}
	}

	// Retention: B's window would take both its traces.
	thirty := 30
	preview, err := f.store.RetentionPreview(b.ID, &thirty, nil, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	want("retention preview", preview)

	// Erasure: u1 filed both.
	erasure, _, err := f.store.UserDataPreview(b.ID, "u1")
	if err != nil {
		t.Fatal(err)
	}
	want("erasure preview", erasure)

	// One trace of two: B still holds X through the other.
	one, _, err := f.store.TracePreview(b.ID, hexTrace(2))
	if err != nil {
		t.Fatal(err)
	}
	if one.Media != 0 {
		t.Errorf("preview of one of B's two traces = %d bodies, want 0", one.Media)
	}
	del := &TraceDelete{ProjectID: b.ID, IDs: []string{hexTrace(2)}, Confirm: hexTrace(2)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Media != 0 {
		t.Errorf("deleting one of B's two traces = %+v, want no body", del.Counts)
	}

	last, _, err := f.store.TracePreview(b.ID, hexTrace(3))
	if err != nil {
		t.Fatal(err)
	}
	want("trace deletion preview", last)
	erase := &UserDataErase{ProjectID: b.ID, UserID: "u1", Confirm: "u1", Limit: 500}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	want("confirmed erasure", erase.Counts)

	// A still reads X, and the bytes are still there.
	if file, err := f.store.MediaFor(f.project.ID, x.SHA256); err != nil || file == nil {
		t.Fatalf("A lost X: %v", err)
	}
	if f.mediaRows(t) != 1 {
		t.Error("the body left the disk while A holds it")
	}

	// Guard: a body only one project holds is counted and collected.
	del = &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)}, Confirm: hexTrace(1)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Media != 1 || del.Counts.MediaBytes != 5000 || f.mediaRows(t) != 0 {
		t.Errorf("the last holder's deletion = %+v, %d bodies left", del.Counts, f.mediaRows(t))
	}
}

// Migration 0022 on a database schema 0021 left: a body two projects hold, a
// raw-only hold and a pending ref get one holder each, under the body's
// stored type, from the earliest of the pair's refs and batches; every raw
// ref names its batch's project (#25, #26).
func TestMigration0022FillsTheHolders(t *testing.T) {
	var names []string
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() < "0022" {
			names = append(names, e.Name())
		}
	}
	path := openAtSchemas(t, names...)
	x, y, z := mediaBody(50, 100), mediaBody(51, 100), mediaBody(52, 100)
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := db.Exec(query, args...); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
		exec(`INSERT INTO projects (id, name) VALUES ('p1', 'one'), ('p2', 'two')`)
		for sha, mime := range map[string]string{x.SHA256: "image/png", y.SHA256: "audio/wav", z.SHA256: "image/gif"} {
			exec(`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES (?, ?, 100, X'00', 1)`, sha, mime)
		}
		exec(`INSERT INTO raw_batches (id, project_id, received_at, body) VALUES (1, 'p1', 50, X'00'), (2, 'p2', 400, X'00')`)
		exec(`INSERT INTO media_raw_refs (sha256, raw_batch_id) VALUES (?, 1), (?, 2)`, x.SHA256, y.SHA256)
		exec(`INSERT INTO media_refs (sha256, project_id, trace_id, created_at, pending) VALUES
		        (?, 'p1', 't1', 100, 0), (?, 'p2', 't2', 300, 0), (?, 'p2', 't3', 350, 0), (?, 'p1', 't4', 500, 1)`,
			x.SHA256, x.SHA256, x.SHA256, z.SHA256)
	}()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(`SELECT sha256, project_id, mime_type, first_at FROM media_holders ORDER BY first_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var sha, project, mime string
		var at int64
		if err := rows.Scan(&sha, &project, &mime, &at); err != nil {
			t.Fatal(err)
		}
		name := map[string]string{x.SHA256: "x", y.SHA256: "y", z.SHA256: "z"}[sha]
		got = append(got, fmt.Sprintf("%s/%s %s %d", name, project, mime, at))
	}
	want := []string{"x/p1 image/png 50", "x/p2 image/png 300", "y/p2 audio/wav 400", "z/p1 image/gif 500"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("holders = %v, want %v", got, want)
	}
	var wrong int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM media_raw_refs rr JOIN raw_batches b ON b.id = rr.raw_batch_id
	                          WHERE rr.project_id != b.project_id`).Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if wrong != 0 {
		t.Errorf("%d raw refs name another project than their batch's", wrong)
	}
	if file, err := s.MediaFor("p2", y.SHA256); err != nil || file == nil || file.MimeType != "audio/wav" {
		t.Errorf("the raw-only hold after the upgrade = %+v, %v", file, err)
	}
	checkIntegrity(t, s)
}
