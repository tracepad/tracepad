package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
)

// An erasure reaches every copy the store holds (spec 044): the raw scrub
// (#2–#5), the session-only scores (#7, #8) and the dataset items (#9).

// newErasureFixture is a sweep fixture whose migrations were applied long
// before any trace it writes: the stamps are the current rule's, and a test
// about a legacy stamp moves the one migration it is about.
func newErasureFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := newSweepFixture(t)
	if _, err := f.store.db.Exec(
		`UPDATE schema_migrations SET applied_at = '2000-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	return f
}

// migratedAt moves when one migration was applied, by the suffix of its file.
func (f *sweepFixture) migratedAt(t *testing.T, suffix string, at int64) {
	t.Helper()
	stamp := time.Unix(0, at).UTC().Format("2006-01-02T15:04:05.000Z")
	result, err := f.store.db.Exec(`UPDATE schema_migrations SET applied_at = ? WHERE filename LIKE ?`,
		stamp, "%"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		t.Fatalf("moved %d migrations named *%s, want one", n, suffix)
	}
}

// otlpSpan is one span of a trace, filed under a user and a session, carrying
// a text and, optionally, a picture as a data URL.
func otlpSpan(t *testing.T, trace, span int, user, session, text string, picture []byte) *tracepb.Span {
	t.Helper()
	traceID, _ := hex.DecodeString(hexTrace(trace))
	spanID, _ := hex.DecodeString(hexSpan(trace*100 + span))
	attr := func(key, value string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
			Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
	}
	attrs := []*commonpb.KeyValue{attr("user.id", user), attr("langfuse.observation.input", text)}
	if session != "" {
		attrs = append(attrs, attr("session.id", session))
	}
	if picture != nil {
		attrs = append(attrs, attr("picture", "data:image/png;base64,"+b64(picture)))
	}
	return &tracepb.Span{TraceId: traceID, SpanId: spanID, Name: fmt.Sprintf("span-%d-%d", trace, span),
		StartTimeUnixNano: 1_000_000_000, EndTimeUnixNano: 2_000_000_000, Attributes: attrs}
}

// export wraps spans in one ResourceSpans per group.
func export(groups ...[]*tracepb.Span) []*tracepb.ResourceSpans {
	var out []*tracepb.ResourceSpans
	for _, spans := range groups {
		out = append(out, &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}})
	}
	return out
}

// ingestOTLP writes an export the way the OTLP handler does — media factored
// out, mapped, the body archived — and answers the raw batch's id.
func (f *sweepFixture) ingestOTLP(t *testing.T, resourceSpans []*tracepb.ResourceSpans, asJSON bool, at int64) int64 {
	t.Helper()
	var (
		body []byte
		err  error
	)
	contentType := RawContentTypeProtobuf
	if asJSON {
		contentType = mapping.ContentTypeJSON
		body, err = mapping.EncodeExportRequestJSON(resourceSpans)
	} else {
		body, err = mapping.EncodeExportRequest(resourceSpans)
	}
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := mapping.DecodeExportBody(body, asJSON)
	if err != nil {
		t.Fatal(err)
	}
	media := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
	result := mapping.Map(decoded.ResourceSpans)
	archived := body
	if media.Any() {
		if archived, err = decoded.Encode(media.Rewrites); err != nil {
			t.Fatal(err)
		}
	}
	batch := &IngestBatch{ProjectID: f.project.ID, IngestedAt: at,
		Traces: result.Traces, Observations: result.Observations,
		Raw:       &RawBatch{ContentType: contentType, Body: archived},
		RawMedia:  media.SHAs(),
		MediaRefs: media.Refs,
	}
	for _, b := range media.Bodies {
		batch.Media = append(batch.Media, *b)
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.store.db.QueryRow(`SELECT MAX(id) FROM raw_batches`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// rawSpans lists the spans a raw batch holds, by name; nil for a batch that is
// gone.
func (f *sweepFixture) rawSpans(t *testing.T, id int64) []string {
	t.Helper()
	body, err := f.store.RawBatchBody(t.Context(), f.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		return nil
	}
	decoded, err := mapping.DecodeExportBody(body.Body, body.ContentType == mapping.ContentTypeJSON)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, rs := range decoded.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				names = append(names, span.Name)
			}
		}
	}
	return names
}

// erasedUser is what erasing a user ended with, and what the run read.
type erasedUser struct {
	*Erasure
	BatchesRead int
}

// erase records an erasure of the user and runs it to its end here, as the
// worker would, and expects it done.
func (f *sweepFixture) erase(t *testing.T, user string, opts ...func(*EraserOptions)) erasedUser {
	t.Helper()
	o := EraserOptions{Chunk: 500}
	for _, opt := range opts {
		opt(&o)
	}
	run, err := f.store.runErasure(t.Context(), f.writer, f.startErasure(t, user).ID, o)
	if err != nil {
		t.Fatal(err)
	}
	ended := f.erasureOf(t, user)
	if ended.State != ErasureDone {
		t.Fatalf("the erasure ended %s: %s", ended.State, ended.Error)
	}
	return erasedUser{Erasure: ended, BatchesRead: run.batchesRead}
}

// startErasure records an erasure of the user, as a confirmed request does.
func (f *sweepFixture) startErasure(t *testing.T, user string) *Erasure {
	t.Helper()
	e, err := f.store.StartErasure(t.Context(), f.writer, UserErasure{ProjectID: f.project.ID,
		UserID: user, Confirm: user})
	if err != nil {
		t.Fatal(err)
	}
	f.erasures = append(f.erasures, e.ID)
	return e
}

// erasureOf reads the fixture's latest erasure, which is the user's.
func (f *sweepFixture) erasureOf(t *testing.T, user string) *Erasure {
	t.Helper()
	if len(f.erasures) == 0 {
		t.Fatalf("no erasure of %s was recorded", user)
	}
	e, err := f.store.Erasure(t.Context(), f.project.ID, f.erasures[len(f.erasures)-1])
	if err != nil || e == nil {
		t.Fatalf("the erasure of %s: %v, %v", user, e, err)
	}
	return e
}

func b64(body []byte) string { return base64.StdEncoding.EncodeToString(body) }

// The scrub (#2), in both encodings: a batch mixing two users keeps exactly
// the other user's spans and is marked; one holding only the erased user's
// is deleted; one holding none is untouched and unmarked; the pictures only
// the erased spans pointed at are collected, the others stay, and the
// batch's refs are what its new body references.
func TestErasureScrubsTheRawBatches(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "protobuf", true: "json"}[asJSON], func(t *testing.T) {
			f := newErasureFixture(t)
			theirs, ours := mediaBody(1, 5000).Body, mediaBody(2, 5000).Body
			at := daysAgo(1)
			mixed := f.ingestOTLP(t, export(
				[]*tracepb.Span{
					otlpSpan(t, 1, 1, "user-a", "", "marker-a-one", theirs),
					otlpSpan(t, 2, 1, "user-b", "", "marker-b-one", ours),
				},
				[]*tracepb.Span{otlpSpan(t, 3, 1, "user-b", "", "marker-b-two", nil)},
			), asJSON, at)
			onlyA := f.ingestOTLP(t, export(
				[]*tracepb.Span{otlpSpan(t, 1, 2, "user-a", "", "marker-a-two", nil)}), asJSON, at+1)
			onlyB := f.ingestOTLP(t, export(
				[]*tracepb.Span{otlpSpan(t, 2, 2, "user-b", "", "marker-b-three", nil)}), asJSON, at+2)

			preview, err := f.store.UserDataPreview(t.Context(), f.project.ID, "user-a", sweepNow.UnixNano())
			if err != nil {
				t.Fatal(err)
			}
			if preview.Raw.BatchesToScan != 2 {
				t.Errorf("batches to scan = %d, want the two inside the trace's window", preview.Raw.BatchesToScan)
			}

			result := f.erase(t, "user-a")
			if c := result.Counts; c.RawSpans != 2 || c.RawBatchesRewritten != 1 || c.RawBatchesDeleted != 1 {
				t.Errorf("raw counts = %d spans, %d rewritten, %d deleted; want 2, 1, 1",
					c.RawSpans, c.RawBatchesRewritten, c.RawBatchesDeleted)
			}
			if result.Compaction == 0 {
				t.Error("the erasure asked for no compaction")
			}
			if got := f.rawSpans(t, mixed); !slices.Equal(got, []string{"span-2-1", "span-3-1"}) {
				t.Errorf("the mixed batch holds %v, want the other user's two spans", got)
			}
			if got := f.rawSpans(t, onlyA); got != nil {
				t.Errorf("the batch holding only the erased user's span is still there: %v", got)
			}
			if got := f.rawSpans(t, onlyB); !slices.Equal(got, []string{"span-2-2"}) {
				t.Errorf("the other user's batch holds %v", got)
			}
			rows, err := f.store.RawBatches(t.Context(), f.project.ID, RawFilter{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if (row.ScrubbedAt != nil) != (row.ID == mixed) {
					t.Errorf("batch %d scrubbed_at = %v; only the rewritten one is marked", row.ID, row.ScrubbedAt)
				}
			}
			if body, _ := f.store.RawBatchBody(t.Context(), f.project.ID, mixed); body == nil || body.ScrubbedAt == nil {
				t.Error("the rewritten body does not say it was scrubbed")
			}
			for _, body := range f.rawBodies(t) {
				if bytes.Contains(body, []byte("marker-a")) || bytes.Contains(body, []byte("user-a")) {
					t.Errorf("a raw body still holds the erased user:\n%.300s", body)
				}
			}

			// Media: the erased span's picture is collected, the kept
			// span's stays, and the batch's refs are its new body's.
			if n := f.count(t, `SELECT COUNT(*) FROM media WHERE sha256 = ?`, shaHex(theirs)); n != 0 {
				t.Error("the picture only the erased user's span pointed at is still stored")
			}
			if n := f.count(t, `SELECT COUNT(*) FROM media WHERE sha256 = ?`, shaHex(ours)); n != 1 {
				t.Error("the picture a kept span points at went")
			}
			refs, err := queryColumn[string](f.store.db,
				`SELECT sha256 FROM media_raw_refs WHERE raw_batch_id = ?`, mixed)
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 1 || refs[0] != shaHex(ours) {
				t.Errorf("the rewritten batch's refs = %v, want only the kept span's picture", refs)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM traces WHERE user_id = 'user-a'`); n != 0 {
				t.Errorf("%d of the user's traces are left", n)
			}
		})
	}
}

// rawBodies is every raw body the store holds, decoded.
func (f *sweepFixture) rawBodies(t *testing.T) [][]byte {
	t.Helper()
	stored, err := queryColumn[[]byte](f.store.db, `SELECT body FROM raw_batches`)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, s := range stored {
		body, err := Decompress(CompressionZstd, s.([]byte))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

func shaHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// A rewrite that fails deletes the batch whole and counts it: the spans must
// not stay for want of an encoder (#2).
func TestAFailedRewriteDeletesTheBatch(t *testing.T) {
	f := newErasureFixture(t)
	mixed := f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 1, 1, "user-a", "", "marker-a", nil),
		otlpSpan(t, 2, 1, "user-b", "", "marker-b", nil),
	}), false, daysAgo(1))
	saved := scrubRewrite
	scrubRewrite = func(*mapping.ExportBody, map[string]bool) ([]byte, error) {
		return nil, errors.New("no encoder")
	}
	t.Cleanup(func() { scrubRewrite = saved })

	result := f.erase(t, "user-a")
	if result.Counts.RawBatchesDeleted != 1 || result.Counts.RawBatchesRewritten != 0 {
		t.Errorf("deleted %d, rewritten %d; want the failed batch deleted and counted",
			result.Counts.RawBatchesDeleted, result.Counts.RawBatchesRewritten)
	}
	if got := f.rawSpans(t, mixed); got != nil {
		t.Errorf("the batch whose rewrite failed is still there: %v", got)
	}
}

// Two erasures scrubbing one batch at once: the one whose body was computed
// before the other's rewrite landed is refused, re-reads, and the result
// holds neither user's spans (#4).
func TestConcurrentScrubsOfOneBatch(t *testing.T) {
	f := newErasureFixture(t)
	batch := f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 1, 1, "user-a", "", "marker-a", nil),
		otlpSpan(t, 2, 1, "user-b", "", "marker-b", nil),
		otlpSpan(t, 3, 1, "user-c", "", "marker-c", nil),
	}), false, daysAgo(1))
	a := map[string]bool{hexTrace(1): true}
	c := map[string]bool{hexTrace(3): true}

	// The first computes its body, then the second lands first.
	early, err := f.store.planScrub(t.Context(), f.project.ID, batch, a)
	if err != nil || early == nil {
		t.Fatalf("plan: %v, %v", early, err)
	}
	if err := f.store.scrubBatches(t.Context(), f.writer, f.project.ID, []int64{batch}, c, ""); err != nil {
		t.Fatal(err)
	}
	err = f.writer.Submit(t.Context(), early.job)
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectConflict {
		t.Fatalf("a body computed before the other rewrite was not refused: %v", err)
	}
	// Refused, it recomputes from what is there now.
	if err := f.store.scrubBatches(t.Context(), f.writer, f.project.ID, []int64{batch}, a, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.rawSpans(t, batch); !slices.Equal(got, []string{"span-2-1"}) {
		t.Errorf("the batch holds %v, want only the one neither erasure took", got)
	}
}

// The windows (#3): with one clock reading a batch 1 ns before the trace's
// arrival is not read and one at it is; each legacy stamp opens the side it
// cannot vouch for.
func TestTheArrivalWindows(t *testing.T) {
	f := newErasureFixture(t)
	arrival := daysAgo(3)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "x", nil)}), false, arrival)
	later := arrival + int64(time.Hour)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 2, "user-a", "", "y", nil)}), false, later)
	seed := func(at int64) int64 {
		seedRawBatch(t, f.store, f.project.ID, &RawBatch{ReceivedAt: at, Body: []byte("other")})
		var id int64
		if err := f.store.db.QueryRow(`SELECT MAX(id) FROM raw_batches`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := seed(arrival - 1)
	margin := seed(arrival - int64(60*time.Second))
	outside := seed(arrival - int64(61*time.Second))
	after := seed(later + 1)
	ancient := seed(1)

	candidates := func() []int64 {
		t.Helper()
		traces, err := f.store.userTraces(t.Context(), f.project.ID, "user-a")
		if err != nil {
			t.Fatal(err)
		}
		windows, err := f.store.userWindows(t.Context(), traces)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := f.store.candidateBatches(t.Context(), f.project.ID, windows)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	has := func(ids []int64, id int64) bool { return slices.Contains(ids, id) }

	now := candidates()
	if len(now) != 2 || has(now, before) || has(now, after) {
		t.Errorf("one clock: candidates %v, want the trace's two batches and nothing 1 ns outside", now)
	}

	// Arrived before the erasure migration: the batch stamp was the
	// handler's, earlier by the queue wait.
	f.migratedAt(t, migrationErasureSuffix, arrival+int64(time.Millisecond))
	legacy := candidates()
	if !has(legacy, before) || !has(legacy, margin) || has(legacy, outside) {
		t.Errorf("legacy stamp: candidates %v, want the 60 s margin and not a nanosecond more", legacy)
	}

	// Arrived before 0009: `updated_at` said nothing, the window closes
	// when 0009 was applied.
	f.migratedAt(t, migrationUpdatedAt, later+int64(time.Millisecond))
	if got := candidates(); !has(got, after) {
		t.Errorf("before 0009: candidates %v, want the batch after updated_at", got)
	}

	// Arrived before 0005: `ingested_at` was the client's, the window
	// opens at the oldest batch.
	f.migratedAt(t, migrationIngestedAt, arrival+int64(time.Millisecond))
	if got := candidates(); !has(got, ancient) {
		t.Errorf("before 0005: candidates %v, want the project's oldest batch", got)
	}
}

// Ingest stamps a batch with the reading its traces get (#3).
func TestABatchIsStampedWithItsTracesArrival(t *testing.T) {
	f := newErasureFixture(t)
	id := f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "u", "", "x", nil)}), false, 0)
	var received, ingested int64
	if err := f.store.db.QueryRow(`SELECT received_at FROM raw_batches WHERE id = ?`, id).Scan(&received); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRow(`SELECT ingested_at FROM traces`).Scan(&ingested); err != nil {
		t.Fatal(err)
	}
	if received != ingested || received == 0 {
		t.Errorf("received_at %d, ingested_at %d: want one reading", received, ingested)
	}
}

// The order (#4), and the resume (spec 047 #12): an erasure stopped in any
// phase — the worker's stop, or the writer closing under it — resumes from
// that phase on the next start and ends where an uninterrupted run does, with
// the same counts; the batch that arrived for a trace deleted before the stop
// is scrubbed by the resumed tail, which a repeat of the request never could.
func TestAStoppedErasureResumesFromItsPhase(t *testing.T) {
	build := func(t *testing.T) *sweepFixture {
		f := newErasureFixture(t)
		for i := range 3 {
			f.ingestOTLP(t, export([]*tracepb.Span{
				otlpSpan(t, 1+i, 1, "user-a", "s-a", "marker-a", nil),
				otlpSpan(t, 10+i, 1, "user-b", "s-b", "marker-b", nil),
			}), i%2 == 1, daysAgo(3-i))
		}
		return f
	}
	// A late span of each of the user's traces, after step 1 read them.
	var late int64
	arrive := func(t *testing.T, f *sweepFixture) {
		late = f.ingestOTLP(t, export([]*tracepb.Span{
			otlpSpan(t, 1, 9, "user-a", "s-a", "marker-a-late", nil),
			otlpSpan(t, 2, 9, "user-a", "s-a", "marker-a-late", nil),
			otlpSpan(t, 3, 9, "user-a", "s-a", "marker-a-late", nil),
			otlpSpan(t, 10, 9, "user-b", "s-b", "marker-b-late", nil),
		}), false, 0)
	}
	state := func(t *testing.T, f *sweepFixture) string {
		t.Helper()
		ids, err := queryColumn[int64](f.store.db, `SELECT id FROM raw_batches ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, id := range ids {
			out = append(out, fmt.Sprint(id, f.rawSpans(t, id.(int64))))
		}
		return fmt.Sprint(out, f.count(t, `SELECT COUNT(*) FROM traces`),
			f.count(t, `SELECT COUNT(*) FROM observations`))
	}
	// One trace a chunk, so that a stop can fall between two of them.
	whole := build(t)
	counts := whole.erase(t, "user-a", func(o *EraserOptions) {
		o.Chunk = 1
		o.after = func(step int) error {
			if step == 1 {
				arrive(t, whole)
			}
			return nil
		}
	}).Counts
	want := state(t, whole)
	if strings.Contains(want, "span-1-") || strings.Contains(want, "span-2-") || strings.Contains(want, "span-3-") {
		t.Fatalf("an uninterrupted erasure left the user's spans in the archive: %s", want)
	}

	stop := errors.New("the server is stopping")
	for _, tc := range []struct {
		name, phase string
		at, chunk   int
		tail        bool
	}{
		{name: "after step 1", phase: phaseRaw, at: 1},
		{name: "after step 2", phase: phaseParsed, at: 2},
		{name: "between two chunks", phase: phaseParsed, chunk: 2},
		{name: "in the tail", phase: phaseTail, tail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t)
			writer := &stoppingWriter{jobSubmitter: f.writer, chunk: tc.chunk, tail: tc.tail}
			e := f.startErasure(t, "user-a")
			_, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{Chunk: 1,
				after: func(step int) error {
					if step == 1 {
						arrive(t, f)
					}
					if step == tc.at {
						return stop
					}
					return nil
				}})
			if !errors.Is(err, stop) && !errors.Is(err, ErrWriterClosed) {
				t.Fatalf("the run was not stopped: %v", err)
			}
			if got := f.erasureOf(t, "user-a"); got.State != ErasureRunning || got.Phase != tc.phase {
				t.Fatalf("stopped, the erasure is %s in %q, want running in %q", got.State, got.Phase, tc.phase)
			}

			if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{Chunk: 1}); err != nil {
				t.Fatal(err)
			}
			got := f.erasureOf(t, "user-a")
			if got.State != ErasureDone {
				t.Fatalf("resumed, the erasure ended %s: %s", got.State, got.Error)
			}
			if got.Counts != counts {
				t.Errorf("resumed, it counts %+v, want an uninterrupted run's %+v", got.Counts, counts)
			}
			if after := state(t, f); after != want {
				t.Errorf("after the resume:\n%s\nwant\n%s", after, want)
			}
			if spans := f.rawSpans(t, late); !slices.Equal(spans, []string{"span-10-9"}) {
				t.Errorf("the batch that arrived during the erasure holds %v, want the other user's span", spans)
			}
		})
	}

	// A start that a restart interrupts counts (spec 047 #12): three are
	// all an erasure gets, so one that brings the server down each time
	// does not do so for ever.
	t.Run("three interrupted starts", func(t *testing.T) {
		f := build(t)
		e := f.startErasure(t, "user-a")
		for range erasureAttempts {
			_, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{
				after: func(int) error { return stop }})
			if !errors.Is(err, stop) {
				t.Fatalf("the run was not stopped: %v", err)
			}
		}
		if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
			t.Fatal(err)
		}
		got := f.erasureOf(t, "user-a")
		if got.State != ErasureFailed || got.Error != "3 starts ended before the erasure did" || got.UserID != "" {
			t.Errorf("after three interrupted starts the erasure is %s (%q) of %q, want failed, "+
				"interrupted, and of no one", got.State, got.Error, got.UserID)
		}
	})

	// The start after the third crash is the last and does only the tail
	// (spec 047 #27): the chunks that committed deleted traces whose late
	// batch no repeat can find, so it is scrubbed before the erasure fails.
	// A tail that crashes too is dropped at the start after it.
	for _, crashTail := range []bool{false, true} {
		t.Run(fmt.Sprintf("the last start, its tail crashing %v", crashTail), func(t *testing.T) {
			f := build(t)
			e := f.startErasure(t, "user-a")
			// Crash one: after the first chunk. Two and three: at the
			// first chunk of the resumed run, which deletes nothing.
			for crash := 1; crash <= erasureAttempts; crash++ {
				writer := &stoppingWriter{jobSubmitter: f.writer, chunk: min(crash, 2)}
				_, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{Chunk: 1,
					after: func(step int) error {
						if step == 1 {
							arrive(t, f)
						}
						return nil
					}})
				if !errors.Is(err, ErrWriterClosed) {
					t.Fatalf("crash %d did not stop the run: %v", crash, err)
				}
			}
			deleted := 0
			for trace := 1; trace <= 3; trace++ {
				if f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(trace)) == 0 {
					deleted = trace
				}
			}
			if deleted == 0 || !slices.Contains(f.rawSpans(t, late), fmt.Sprintf("span-%d-9", deleted)) {
				t.Fatalf("the crashes left trace %d deleted and the late batch %v; the test needs both",
					deleted, f.rawSpans(t, late))
			}

			var last jobSubmitter = f.writer
			if crashTail {
				last = &failingJobs{jobSubmitter: f.writer, err: ErrWriterClosed, fails: func(job WriteJob) bool {
					_, scrub := job.(*RawScrub)
					return scrub
				}}
			}
			_, err := f.store.runErasure(t.Context(), last, e.ID, EraserOptions{Chunk: 1})
			if crashTail {
				if !errors.Is(err, ErrWriterClosed) {
					t.Fatalf("the tail's crash did not stop the run: %v", err)
				}
				if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{Chunk: 1}); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got := f.erasureOf(t, "user-a")
			if got.State != ErasureFailed || got.Error != "3 starts ended before the erasure did" || got.UserID != "" {
				t.Errorf("the erasure is %s (%q) of %q, want failed, interrupted, and of no one",
					got.State, got.Error, got.UserID)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM erasure_tail`); n != 0 {
				t.Errorf("%d tail rows are left", n)
			}
			if left := slices.Contains(f.rawSpans(t, late), fmt.Sprintf("span-%d-9", deleted)); left != crashTail {
				t.Errorf("the deleted trace's late span is left %v, want %v", left, crashTail)
			}
		})
	}

	t.Run("a batch arriving while it runs", func(t *testing.T) {
		f := build(t)
		f.erase(t, "user-a", func(o *EraserOptions) {
			o.after = func(step int) error {
				if step == 1 {
					arrive(t, f)
				}
				return nil
			}
		})
		if got := f.rawSpans(t, late); !slices.Equal(got, []string{"span-10-9"}) {
			t.Errorf("the batch that arrived during the erasure holds %v, want the other user's span", got)
		}
		// Stamped when it was rewritten, which is after it arrived — not
		// when the erasure began, which is before.
		body, err := f.store.RawBatchBody(t.Context(), f.project.ID, late)
		if err != nil || body == nil || body.ScrubbedAt == nil {
			t.Fatalf("the late batch's body: %+v, %v", body, err)
		}
		if *body.ScrubbedAt < body.ReceivedAt {
			t.Errorf("scrubbed_at %d is before received_at %d", *body.ScrubbedAt, body.ReceivedAt)
		}
	})

	// A chunk of step 3 that fails ends the erasure failed (spec 047 #16),
	// after step 4 has run for the chunks before it: their traces are
	// gone, so a repeat cannot find the batches that arrived for them
	// while it ran. The repeat finishes the rest.
	t.Run("a chunk that fails", func(t *testing.T) {
		f := build(t)
		broken := errors.New("the disk is broken")
		writer := &failingChunk{jobSubmitter: f.writer, at: 2, err: broken}
		e := f.startErasure(t, "user-a")
		if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{Chunk: 1,
			after: func(step int) error {
				if step == 1 {
					arrive(t, f)
				}
				return nil
			}}); err != nil {
			t.Fatalf("a failed erasure is not a stopped run: %v", err)
		}
		if writer.chunks < 2 {
			t.Fatalf("%d chunks: the erasure needs one to succeed before the one that fails", writer.chunks)
		}
		if got := f.erasureOf(t, "user-a"); got.State != ErasureFailed || got.Error != broken.Error() {
			t.Fatalf("the erasure is %s (%q), want failed with the chunk's error", got.State, got.Error)
		}
		spans := f.rawSpans(t, late)
		for trace := 1; trace <= 3; trace++ {
			left := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(trace)) == 1
			if slices.Contains(spans, fmt.Sprintf("span-%d-9", trace)) != left {
				t.Errorf("trace %d is left %v, and the late batch holds %v", trace, left, spans)
			}
		}
		f.erase(t, "user-a")
		if got := f.rawSpans(t, late); !slices.Equal(got, []string{"span-10-9"}) {
			t.Errorf("after the repeat the late batch holds %v, want the other user's span", got)
		}
	})
}

// stoppingWriter passes every job on but the chunk-th chunk of the parsed
// phase, or with tail the first scrub after the last chunk, which it answers
// as a writer closed under it does.
type stoppingWriter struct {
	jobSubmitter
	chunk          int
	tail           bool
	chunks         int
	parsed, closed bool
}

func (w *stoppingWriter) Submit(ctx context.Context, job WriteJob) error {
	chunk, isChunk := job.(*UserDataErase)
	if isChunk {
		w.chunks++
	}
	_, isScrub := job.(*RawScrub)
	if w.closed || isChunk && w.chunks == w.chunk || isScrub && w.tail && w.parsed {
		w.closed = true
		return ErrWriterClosed
	}
	err := w.jobSubmitter.Submit(ctx, job)
	if isChunk && err == nil && !chunk.More {
		w.parsed = true
	}
	return err
}

// A trace whose first spans came without the user id, and whose id came in a
// batch after step 1 read the user's traces: step 1 did not know it, step 3
// deletes it, and every batch of its window goes — the early one received
// long before the request, not only what arrived while it ran.
func TestATraceThatBecomesTheUsersMidErasure(t *testing.T) {
	f := newErasureFixture(t)
	known := f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 1, 1, "user-a", "", "marker-a-known", nil)}), false, daysAgo(3))
	early := f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 2, 1, "", "", "marker-a-unnamed", nil),
		otlpSpan(t, 3, 1, "user-b", "", "marker-b", nil),
	}), false, daysAgo(2))
	if n := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ? AND user_id IS NULL`, hexTrace(2)); n != 1 {
		t.Fatalf("the unnamed trace has a user before its root arrived (%d)", n)
	}
	var named int64
	f.erase(t, "user-a", func(o *EraserOptions) {
		o.after = func(step int) error {
			if step == 1 {
				// The root, with the user id, after the traces were read.
				named = f.ingestOTLP(t, export([]*tracepb.Span{
					otlpSpan(t, 2, 2, "user-a", "", "marker-a-root", nil),
					otlpSpan(t, 3, 2, "user-b", "", "marker-b-later", nil),
				}), false, 0)
			}
			return nil
		}
	})
	if got := f.rawSpans(t, known); got != nil {
		t.Errorf("the known trace's batch holds %v", got)
	}
	if got := f.rawSpans(t, early); !slices.Equal(got, []string{"span-3-1"}) {
		t.Errorf("the batch received before the request holds %v, want only the other user's span", got)
	}
	if got := f.rawSpans(t, named); !slices.Equal(got, []string{"span-3-2"}) {
		t.Errorf("the batch that named the user holds %v, want only the other user's span", got)
	}
}

// A pass whose session-score sweep fails still sweeps the raw archive: the
// newer job must not cost the privacy-sensitive store its turn.
func TestAFailedSessionScoreSweepStillSweepsTheRawArchive(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "u", "s", "x", nil)}), false, daysAgo(40))
	if _, err := f.store.db.Exec(`UPDATE projects SET retention_days = 30 WHERE id = ?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the disk is full")
	writer := &failingJobs{jobSubmitter: f.writer, fails: func(job WriteJob) bool {
		_, ok := job.(*sessionScoreSweep)
		return ok
	}, err: broken}
	sweeper := f.store.NewSweeper(writer, SweepOptions{Now: func() time.Time { return sweepNow }})
	if err := sweeper.Pass(t.Context()); !errors.Is(err, broken) {
		t.Fatalf("the pass answered %v, want the session-score sweep's failure", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM raw_batches`); n != 0 {
		t.Errorf("%d raw batches past the window are left after the pass", n)
	}
}

// failingJobs passes every job on but the ones fails picks, which it answers
// with err.
type failingJobs struct {
	jobSubmitter
	fails func(WriteJob) bool
	err   error
}

func (w *failingJobs) Submit(ctx context.Context, job WriteJob) error {
	if w.fails(job) {
		return w.err
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// failingChunk passes every job on but the at-th chunk of an erasure's parsed
// phase, which it answers with err.
type failingChunk struct {
	jobSubmitter
	at, chunks int
	err        error
}

func (w *failingChunk) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*UserDataErase); ok {
		if w.chunks++; w.chunks == w.at {
			return w.err
		}
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// The session-only scores (#7): an erasure takes those of the user's
// sessions, a session shared with another user included, and leaves other
// sessions' and the other user's trace scores.
func TestErasureTakesTheSessionScores(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 1, 1, "user-a", "shared", "a", nil),
		otlpSpan(t, 2, 1, "user-a", "a-only", "a", nil),
		otlpSpan(t, 3, 1, "user-b", "shared", "b", nil),
		otlpSpan(t, 4, 1, "user-b", "b-only", "b", nil),
	}), false, daysAgo(1))
	one := 1.0
	scores := []*Score{
		{ID: "on-shared", SessionID: "shared", Name: "q", DataType: "numeric", Value: &one, Comment: "verdict"},
		{ID: "on-a-only", SessionID: "a-only", Name: "q", DataType: "numeric", Value: &one},
		{ID: "on-b-only", SessionID: "b-only", Name: "q", DataType: "numeric", Value: &one},
		{ID: "on-b-trace", TraceID: hexTrace(3), Name: "q", DataType: "numeric", Value: &one},
		{ID: "on-a-trace-and-session", TraceID: hexTrace(1), SessionID: "b-only", Name: "q",
			DataType: "numeric", Value: &one},
	}
	for _, s := range scores {
		s.Timestamp = daysAgo(1)
	}
	if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: scores}); err != nil {
		t.Fatal(err)
	}

	preview, err := f.store.UserDataPreview(t.Context(), f.project.ID, "user-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts.SessionScores != 2 {
		t.Errorf("the dry run counts %d session scores, want 2", preview.Counts.SessionScores)
	}
	result := f.erase(t, "user-a")
	if result.Counts.SessionScores != 2 || result.Counts.Scores != 1 {
		t.Errorf("deleted %d session scores and %d trace scores, want 2 and 1",
			result.Counts.SessionScores, result.Counts.Scores)
	}
	left, err := queryColumn[string](f.store.db, `SELECT id FROM scores ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(left); got != "[on-b-only on-b-trace]" {
		t.Errorf("scores left = %s, want the other session's and the other user's trace score", got)
	}
}

// The sweep (#8): a session-only score past the window goes when no trace
// carries its session — including one whose last trace goes in the same pass
// — and stays while one does; the retention dry run counts what the pass
// takes.
func TestRetentionTakesSessionOnlyScores(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "u", "alive", "x", nil)}), false, daysAgo(1))
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 2, 1, "u", "expiring", "x", nil)}), false, daysAgo(40))
	one := 1.0
	for _, s := range []struct {
		id, session string
		age         int
	}{
		{"orphan-old", "never-arrived", 40},
		{"orphan-new", "never-arrived", 1},
		{"alive-old", "alive", 40},
		{"expiring-old", "expiring", 40},
	} {
		score := &Score{ID: s.id, SessionID: s.session, Name: "q", DataType: "numeric", Value: &one,
			Timestamp: daysAgo(s.age)}
		if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{score}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`UPDATE scores SET created_at = ? WHERE id = ?`, daysAgo(s.age), s.id); err != nil {
			t.Fatal(err)
		}
	}
	thirty := 30
	if _, err := f.store.db.Exec(`UPDATE projects SET retention_days = 30 WHERE id = ?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := f.store.RetentionPreview(t.Context(), f.project.ID, &thirty, nil, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if preview.SessionScores != 2 {
		t.Errorf("the retention dry run counts %d session scores, want 2", preview.SessionScores)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	left, err := queryColumn[string](f.store.db, `SELECT id FROM scores ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(left); got != "[alive-old orphan-new]" {
		t.Errorf("scores left = %s, want the one whose session lives and the one inside the window", got)
	}
}

// The sweep's session scores walk from a cursor: each chunk starts where the
// pass's previous one stopped, so the scores a live session keeps — here the
// oldest ones, which every chunk used to walk again — are passed over once a
// pass; and what is past the window goes, chunk by chunk, whatever it is kept
// behind.
func TestSessionScoreSweepResumesWhereItsLastChunkStopped(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "u", "alive", "x", nil)}), false, daysAgo(1))
	one := 1.0
	score := func(id, session string, age int) {
		t.Helper()
		s := &Score{ID: id, SessionID: session, Name: "q", DataType: "numeric", Value: &one, Timestamp: daysAgo(age)}
		if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{s}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`UPDATE scores SET created_at = ? WHERE id = ?`, daysAgo(age), id); err != nil {
			t.Fatal(err)
		}
	}
	for age := 60; age > 50; age-- {
		score(fmt.Sprintf("kept-%d", age), "alive", age)
	}
	for age := 50; age > 43; age-- {
		score(fmt.Sprintf("gone-%d", age), "never-arrived", age)
	}
	score("kept-43", "alive", 43)
	if _, err := f.store.db.Exec(`UPDATE projects SET retention_days = 30 WHERE id = ?`, f.project.ID); err != nil {
		t.Fatal(err)
	}

	var chunks []sessionScoreSweep
	writer := &watchedJobs{jobSubmitter: f.writer, done: func(job WriteJob) {
		if chunk, ok := job.(*sessionScoreSweep); ok {
			chunks = append(chunks, *chunk)
		}
	}}
	sweeper := f.store.NewSweeper(writer, SweepOptions{Chunk: 2, Now: func() time.Time { return sweepNow }})
	if err := sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("%d chunks for seven scores two at a time, want 4", len(chunks))
	}
	from := startOfScores
	for i, chunk := range chunks {
		if chunk.After != from {
			t.Errorf("chunk %d starts at %+v, want where chunk %d stopped, %+v", i+1, chunk.After, i, from)
		}
		if chunk.Next == chunk.After {
			t.Errorf("chunk %d did not move the cursor", i+1)
		}
		from = chunk.Next
	}
	left, err := queryColumn[string](f.store.db, `SELECT id FROM scores WHERE id LIKE 'gone-%'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("scores past the window left: %v", left)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM scores WHERE id LIKE 'kept-%'`); n != 11 {
		t.Errorf("%d of the live session's 11 scores are left", n)
	}
}

// TestSessionScoreSweepSeeksItsCursor is the EXPLAIN half of the test above:
// the cursor is part of the index seek, not a filter over the rows before it,
// and the order is the index's, so a chunk reads from where the last stopped.
func TestSessionScoreSweepSeeksItsCursor(t *testing.T) {
	f := newErasureFixture(t)
	plan, err := f.store.explainQueryPlan(expiredSessionScores, f.project.ID, daysAgo(30), daysAgo(50), 7, 100)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_scores_session_only (project_id=? AND created_at>? AND created_at<?)") {
		t.Errorf("the cursor is not part of the index seek:\n%s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the chunk sorts rather than walking the index:\n%s", joined)
	}
}

// watchedJobs passes every job on and shows each one to done once it has
// been applied.
type watchedJobs struct {
	jobSubmitter
	done func(WriteJob)
}

func (w *watchedJobs) Submit(ctx context.Context, job WriteJob) error {
	if err := w.jobSubmitter.Submit(ctx, job); err != nil {
		return err
	}
	w.done(job)
	return nil
}

// The dataset items (#9): every row of an item cut from an erased trace goes,
// the one whose later version dropped the source included; the version ticks
// once per chunk; an item with no source, or another's, stays; a run of an
// older version counts the answering traces under unknown; the dry run names
// the datasets.
func TestErasureTakesTheDatasetItems(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{
		otlpSpan(t, 1, 1, "user-a", "", "a", nil),
		otlpSpan(t, 2, 1, "user-a", "", "a", nil),
		otlpSpan(t, 3, 1, "user-b", "", "b", nil),
	}), false, daysAgo(2))
	item := func(n int) string { return strings.Repeat(fmt.Sprint(n), 32) }
	write := func(dataset string, items ...*DatasetItemInput) {
		t.Helper()
		if err := f.writer.Submit(t.Context(), &DatasetItemsWrite{ProjectID: f.project.ID, Dataset: dataset,
			Now: 1, Items: items}); err != nil {
			t.Fatal(err)
		}
	}
	write("golden",
		&DatasetItemInput{ID: item(1), Input: []byte(`{"q":"from a"}`), SourceTraceID: hexTrace(1)},
		&DatasetItemInput{ID: item(2), Input: []byte(`{"q":"from a too"}`), SourceTraceID: hexTrace(2)},
		&DatasetItemInput{ID: item(3), Input: []byte(`{"q":"from b"}`), SourceTraceID: hexTrace(3)},
		&DatasetItemInput{ID: item(4), Input: []byte(`{"q":"typed in"}`)})
	// A curator anonymised item 1 and dropped the source: its earlier row
	// still names the trace.
	write("golden", &DatasetItemInput{ID: item(1), Input: []byte(`{"q":"anonymised"}`)})
	write("other", &DatasetItemInput{ID: item(5), Input: []byte(`{"q":"a again"}`), SourceTraceID: hexTrace(2)})
	golden, err := f.store.Dataset(t.Context(), f.project.ID, "golden")
	if err != nil {
		t.Fatal(err)
	}
	// A run at this version, answered by the other user's trace for item 1.
	create := &RunCreate{ProjectID: f.project.ID, Dataset: "golden", ID: strings.Repeat("e", 32), Now: 1}
	if err := f.writer.Submit(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, f.project.ID, hexTrace(9), daysAgo(1), func(tr *model.Trace) {
		tr.UserID, tr.RunID, tr.ItemID = "user-b", create.Run.ID, item(1)
	})

	preview, err := f.store.UserDataPreview(t.Context(), f.project.ID, "user-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts.DatasetItems != 3 || fmt.Sprint(preview.Datasets) != "[{golden 2} {other 1}]" {
		t.Errorf("the dry run counts %d items in %v, want 3 in golden (2) and other (1)",
			preview.Counts.DatasetItems, preview.Datasets)
	}
	result := f.erase(t, "user-a")
	if result.Counts.DatasetItems != 3 {
		t.Errorf("deleted %d items, want 3", result.Counts.DatasetItems)
	}
	left, err := queryColumn[string](f.store.db,
		`SELECT dataset || ':' || substr(item_id, 1, 1) FROM dataset_items ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(left); got != "[golden:3 golden:4]" {
		t.Errorf("items left = %s, want the other user's and the typed-in one, no row of the rest", got)
	}
	after, err := f.store.Dataset(t.Context(), f.project.ID, "golden")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != golden.Version+1 {
		t.Errorf("golden's version went %d → %d, want one tick for the chunk", golden.Version, after.Version)
	}
	summary, err := f.store.RunSummary(t.Context(), f.project.ID, create.Run)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Items.Unknown != 1 {
		t.Errorf("the run counts %d unknown traces, want the one that answered the erased item", summary.Items.Unknown)
	}
}

// A trace no span has reached has no start time; erasing its user must not
// fail on it (the case trace deletion already handles).
func TestErasingATraceWithNoStartTime(t *testing.T) {
	f := newErasureFixture(t)
	if err := f.writer.Submit(t.Context(), &IngestBatch{ProjectID: f.project.ID, IngestedAt: daysAgo(1),
		Traces: []*model.Trace{{ID: hexTrace(1), UserID: "user-a"}}}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM traces WHERE timestamp IS NULL`); n != 1 {
		t.Fatalf("%d traces without a start time, want the one", n)
	}
	result := f.erase(t, "user-a")
	if result.Counts.Traces != 1 {
		t.Errorf("erased %d traces, want 1", result.Counts.Traces)
	}
}

// The echo is checked before anything is destroyed: the raw phase comes
// first, and must not run for a request the parsed phase would refuse.
func TestAWrongEchoScrubsNothing(t *testing.T) {
	f := newErasureFixture(t)
	batch := f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	_, err := f.store.StartErasure(t.Context(), f.writer, UserErasure{ProjectID: f.project.ID,
		UserID: "user-a", Confirm: "user-b"})
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectInvalid {
		t.Fatalf("err = %v, want the echo refused", err)
	}
	if got := f.rawSpans(t, batch); len(got) != 1 {
		t.Errorf("a refused erasure scrubbed the archive: %v", got)
	}
	// A refusal records nothing (spec 047 #6).
	if n := f.count(t, `SELECT COUNT(*) FROM erasures`); n != 0 {
		t.Errorf("a refused erasure left %d records", n)
	}
}

// A batch in the window whose body no longer decodes cannot say whose spans
// it holds, and is left as it is rather than deleted with everyone else's
// spans in it (#5 b).
func TestAnUnreadableBatchIsLeftAlone(t *testing.T) {
	f := newErasureFixture(t)
	at := daysAgo(1)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, at)
	seedRawBatch(t, f.store, f.project.ID, &RawBatch{ReceivedAt: at, ContentType: mapping.ContentTypeJSON,
		Body: []byte("{not an export")})
	unreadable := f.count(t, `SELECT MAX(id) FROM raw_batches`)

	f.erase(t, "user-a")
	if n := f.count(t, `SELECT COUNT(*) FROM raw_batches WHERE id = ?`, unreadable); n != 1 {
		t.Error("a batch that could not be read was deleted")
	}
}

// The tail reads only what arrived for the erased traces while the request
// ran: other users' recent traffic is not decoded (#4).
func TestTheTailReadsOnlyTheErasedTracesLateBatches(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(2))
	// Other users' traffic of the last minute: inside the old tail's reach.
	for i := range 3 {
		f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 10+i, 1, "user-b", "", "b", nil)}), false, 0)
	}
	result := f.erase(t, "user-a")
	if result.BatchesRead != 1 {
		t.Errorf("the erasure read %d batches, want only the one its trace arrived in", result.BatchesRead)
	}
}

// Windows are asked about in groups, and a group's answer is every group's.
func TestManyWindowsAreAskedInGroups(t *testing.T) {
	f := newErasureFixture(t)
	var windows []arrivalWindow
	for i := range 2*windowGroup + 7 {
		at := int64(1_000_000 * (i + 1))
		seedRawBatch(t, f.store, f.project.ID, &RawBatch{ReceivedAt: at, Body: []byte("x")})
		windows = append(windows, arrivalWindow{from: at, to: at})
	}
	ids, err := f.store.candidateBatches(t.Context(), f.project.ID, windows)
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.store.countBatches(t.Context(), f.project.ID, windows)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(windows) || n != int64(len(windows)) {
		t.Errorf("%d candidates, count %d; want %d", len(ids), n, len(windows))
	}
}

// busySubmitter answers a full queue a few times before taking the job.
type busySubmitter struct {
	busy  int
	calls int
}

func (b *busySubmitter) Submit(context.Context, WriteJob) error {
	b.calls++
	if b.calls <= b.busy {
		return ErrWriterBusy
	}
	return nil
}

// A full writer queue makes an erasure's job wait, not the erasure fail: it
// runs to its end (spec 047 #16).
func TestAnErasureJobWaitsOutAFullQueue(t *testing.T) {
	writer := &busySubmitter{busy: 3}
	if err := submitErasureJob(t.Context(), writer, &RawScrub{}); err != nil {
		t.Fatalf("a queue full three times failed the job: %v", err)
	}
	if writer.calls != 4 {
		t.Errorf("%d submissions, want 4", writer.calls)
	}
}

// flakySubmitter answers a busy database for a while, and a full queue after.
type flakySubmitter struct {
	began time.Time
	calls int
}

func (f *flakySubmitter) Submit(context.Context, WriteJob) error {
	f.calls++
	if time.Since(f.began) < 200*time.Millisecond {
		return fmt.Errorf("commit write transaction: %w", codedError{5})
	}
	return ErrWriterBusy
}

// A job that meets a condition and then a full queue is retried within one
// bound, not a bound for each (spec 047 #29): a loop for the queue inside a
// loop for the conditions retried for up to twice as long as #16 says.
func TestAnErasureJobIsRetriedWithinOneBound(t *testing.T) {
	was := busyWait
	busyWait = 300 * time.Millisecond
	t.Cleanup(func() { busyWait = was })
	writer := &flakySubmitter{began: time.Now()}
	err := submitErasureJob(t.Context(), writer, &RawScrub{})
	if !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("gave up with %v, want the full queue it met last", err)
	}
	if took := time.Since(writer.began); took > busyWait+200*time.Millisecond {
		t.Errorf("gave up after %s, want about %s", took, busyWait)
	}
}

// TestErasureQueriesSeekTheirIndexes is spec 023's EXPLAIN check for the
// erasure's lookups: the store never runs ANALYZE, so an index that is not
// chosen is an index that is not there. The windows seek
// `idx_raw_batches_received` once each rather than walking the project's
// whole index; the dataset items go through the partial index #9 promises —
// a lookup that must not be a walk over every item of the project; the
// session scores start from the user's traces, not every session.
func TestErasureQueriesSeekTheirIndexes(t *testing.T) {
	f := newErasureFixture(t)
	windows := []arrivalWindow{{from: 1, to: 2}, {from: 5, to: 9}}
	candidates, candidateArgs := windowQuery(f.project.ID, "r.id", windows)
	count, countArgs := windowQuery(f.project.ID, "COUNT(*)", windows)
	sourced := sourcedItems(2)
	sourcedArgs := []any{f.project.ID, "t1", "t2"}

	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  []string
	}{
		{"the candidate batches", candidates + ` ORDER BY r.received_at, r.id`, candidateArgs,
			[]string{"SEARCH r USING COVERING INDEX idx_raw_batches_received (project_id=? AND received_at>? AND received_at<?)"}},
		{"the batches to scan", count, countArgs,
			[]string{"SEARCH r USING COVERING INDEX idx_raw_batches_received (project_id=? AND received_at>? AND received_at<?)"}},
		{"the preview's session scores", userSessionScores, []any{f.project.ID, f.project.ID, "user-a"},
			[]string{"idx_traces_user (project_id=? AND user_id=?)", "idx_scores_session (project_id=? AND session_id=?)"}},
		{"the preview's dataset items", userSourcedItems, []any{f.project.ID, f.project.ID, "user-a"},
			[]string{"idx_traces_user (project_id=? AND user_id=?)", "idx_dataset_items_source (project_id=? AND source_trace_id=?)"}},
		{"a chunk's sessions", traceSessions(2), []any{f.project.ID, "t1", "t2"},
			[]string{"SEARCH traces USING"}},
		{"a chunk's dataset item delete", `DELETE FROM dataset_items WHERE project_id = ? AND (dataset, item_id) IN (` +
			sourced + `)`, append([]any{f.project.ID}, sourcedArgs...),
			[]string{"idx_dataset_items_source (project_id=? AND source_trace_id=?)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := f.store.explainQueryPlan(tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("the plan does not hold %q:\n%s", want, joined)
				}
			}
			for _, line := range plan {
				scan := strings.HasPrefix(line, "SCAN") && !strings.Contains(line, "CONSTANT ROW") &&
					!strings.HasPrefix(line, "SCAN windows") && !strings.HasPrefix(line, "SCAN sourced")
				// A seek on the project alone is a walk over all of it.
				if scan || strings.HasSuffix(line, "(project_id=?)") {
					t.Errorf("walks rather than seeks: %q\n%s", line, joined)
				}
			}
		})
	}
}
