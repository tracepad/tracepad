package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// What the wire already carries, at the store (spec 012): the trace's TTFT
// aggregate, the four new filters and the plans behind them, the widened type
// column, and the upgrade of a database written before any of it.

// wireTrace and wireObservation build the rows these tests seed through the
// ingest path, so the aggregates under test are the ones ingest maintains.
func wireTrace(id string, options ...func(*model.Trace)) *model.Trace {
	trace := &model.Trace{ID: id, Name: "wire"}
	for _, option := range options {
		option(trace)
	}
	return trace
}

func wireObservation(traceID, id, kind string, options ...func(*model.Observation)) *model.Observation {
	observation := &model.Observation{
		TraceID: traceID, ID: id, Type: kind, Name: kind,
		Level: model.LevelDefault, StartTime: day, EndTime: day + 500_000_000,
	}
	for _, option := range options {
		option(observation)
	}
	return observation
}

// The trace's TTFT is the earliest completion start minus the moment the
// user's wait began (spec 012 #3), recomputed on every delivery like every
// other aggregate (spec 002 #22).
func TestTraceTTFTAggregate(t *testing.T) {
	s, project := readStore(t)

	t.Run("the earliest completion start of the trace", func(t *testing.T) {
		id := hexTrace(1)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(1), model.TypeGeneration, func(o *model.Observation) {
				o.StartTime, o.EndTime = day, day+900_000_000
				o.CompletionStartTime = day + 400_000_000
			}),
			// Started later, answered sooner: the earliest completion
			// start is this one, but the wait began with the trace.
			wireObservation(id, hexSpan(2), model.TypeGeneration, func(o *model.Observation) {
				o.StartTime, o.EndTime = day+100_000_000, day+600_000_000
				o.CompletionStartTime = day + 250_000_000
			}),
		)

		trace, err := s.Trace(project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if trace.TTFTMs == nil || *trace.TTFTMs != 250 {
			t.Errorf("ttft_ms = %v, want 250 — the earliest completion start minus the trace's start",
				trace.TTFTMs)
		}
	})

	t.Run("no observation carried one", func(t *testing.T) {
		id := hexTrace(2)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(3), model.TypeSpan))

		trace, err := s.Trace(project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if trace.TTFTMs != nil {
			t.Errorf("ttft_ms = %d, want NULL: no observation said when the first token came",
				*trace.TTFTMs)
		}
	})

	// A wait is measured from a moment, and a span that never said when it
	// started did not give one. The trace's `timestamp` falls back to the
	// minimum start of any span, zero included (spec 004 #26), but zero is
	// the epoch rather than a start, and subtracting it would answer a wait
	// of some 10^12 milliseconds. `latency_ms` is NULL on the same rows for
	// the same reason (spec 012 #14, found in review of PR #19).
	t.Run("no observation said when it started", func(t *testing.T) {
		id := hexTrace(5)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(7), model.TypeGeneration, func(o *model.Observation) {
				o.StartTime, o.EndTime = 0, 0
				o.CompletionStartTime = day + 250_000_000
			}))

		trace, err := s.Trace(project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if trace.TTFTMs != nil {
			t.Errorf("ttft_ms = %d, want NULL: there is no moment to measure the wait from",
				*trace.TTFTMs)
		}
		if trace.LatencyMs != nil {
			t.Errorf("latency_ms = %d, want NULL — the two must agree about such a trace",
				*trace.LatencyMs)
		}
	})

	t.Run("a completion earlier than the span is stored unclamped", func(t *testing.T) {
		id := hexTrace(3)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(4), model.TypeGeneration, func(o *model.Observation) {
				o.CompletionStartTime = o.StartTime - 200_000_000
			}))

		trace, err := s.Trace(project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if trace.TTFTMs == nil || *trace.TTFTMs != -200 {
			t.Errorf("ttft_ms = %v, want -200: no clamping and no clock correction (spec 002 #4)",
				trace.TTFTMs)
		}
	})

	// A trace whose generations arrive in several batches converges on the
	// earliest completion start seen so far (spec 012, edge cases).
	t.Run("recomputed on re-delivery", func(t *testing.T) {
		id := hexTrace(4)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(5), model.TypeGeneration, func(o *model.Observation) {
				o.CompletionStartTime = day + 800_000_000
			}))
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(6), model.TypeGeneration, func(o *model.Observation) {
				o.CompletionStartTime = day + 300_000_000
			}))

		trace, err := s.Trace(project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if trace.TTFTMs == nil || *trace.TTFTMs != 300 {
			t.Errorf("ttft_ms = %v after the second batch, want 300", trace.TTFTMs)
		}
	})
}

// The observation carries the completion start and the prompt through to the
// row, and its payload sizes come from the payload table rather than from a
// column of their own (spec 012 #6).
func TestObservationWireFields(t *testing.T) {
	s, project := readStore(t)
	id := hexTrace(10)

	seedTrace(t, s, project.ID, wireTrace(id),
		wireObservation(id, hexSpan(10), model.TypeGeneration, func(o *model.Observation) {
			o.CompletionStartTime = day + 120_000_000
			o.PromptName, o.PromptVersion = "support-answer", ptr(int64(7))
			o.Input = map[string]any{"role": "user"}
			o.Output = "ok"
		}),
		wireObservation(id, hexSpan(11), model.TypeTool))

	observations, err := s.Observations(project.ID, id, SkipIO)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("read %d observations, want 2", len(observations))
	}
	generation, tool := observations[0], observations[1]

	if generation.CompletionStartTime != day+120_000_000 {
		t.Errorf("completion_start_time = %d", generation.CompletionStartTime)
	}
	if generation.PromptName != "support-answer" ||
		generation.PromptVersion == nil || *generation.PromptVersion != 7 {
		t.Errorf("prompt = %q@%v", generation.PromptName, generation.PromptVersion)
	}
	// The sizes ride the row without an expansion: the panel shows them
	// beside the payload headings, and the tree does not read payloads.
	if generation.InputBytes == nil || *generation.InputBytes != int64(len(`{"role":"user"}`)) {
		t.Errorf("input_bytes = %v, want the uncompressed size of the payload", generation.InputBytes)
	}
	if generation.OutputBytes == nil || *generation.OutputBytes != int64(len(`"ok"`)) {
		t.Errorf("output_bytes = %v", generation.OutputBytes)
	}
	// Absent, not zero: a span that logged nothing is not a span that
	// logged an empty payload.
	if tool.InputBytes != nil || tool.OutputBytes != nil {
		t.Errorf("sizes of a span with no payloads = %v/%v, want none",
			tool.InputBytes, tool.OutputBytes)
	}
	if tool.Type != model.TypeTool {
		t.Errorf("type = %q, want the widened vocabulary stored as sent", tool.Type)
	}
}

func TestObservationSizesReachTheSingleRead(t *testing.T) {
	s, project := readStore(t)
	id := hexTrace(11)
	seedTrace(t, s, project.ID, wireTrace(id),
		wireObservation(id, hexSpan(12), model.TypeGeneration, func(o *model.Observation) {
			o.Input = "hello"
		}))

	observation, err := s.Observation(project.ID, id, hexSpan(12))
	if err != nil {
		t.Fatal(err)
	}
	if observation.InputBytes == nil || *observation.InputBytes != int64(len(`"hello"`)) {
		t.Errorf("input_bytes = %v on the single read too", observation.InputBytes)
	}
}

// The four new filters, alone and beside the keyset (spec 012, API contract).
func TestWireFilters(t *testing.T) {
	s, project := readStore(t)

	// Two traces of the same shape but different deployments, each with a
	// tool call and a generation that ran a prompt.
	for i, release := range []string{"2026.8.30", "2026.8.31"} {
		id := hexTrace(20 + i)
		seedTrace(t, s, project.ID,
			wireTrace(id, func(tr *model.Trace) {
				tr.Release, tr.Version = release, "checkout-v9"
			}),
			wireObservation(id, hexSpan(20+i*2), model.TypeGeneration, func(o *model.Observation) {
				o.StartTime, o.EndTime = int64(i+1)*day, int64(i+1)*day+1
				o.PromptName, o.PromptVersion = "support-answer", ptr(int64(6+int64(i)))
			}),
			wireObservation(id, hexSpan(21+i*2), model.TypeTool, func(o *model.Observation) {
				o.StartTime, o.EndTime = int64(i+1)*day+1, int64(i+1)*day+2
			}),
		)
	}
	// A third trace with neither a release nor a tool call.
	plain := hexTrace(30)
	seedTrace(t, s, project.ID, wireTrace(plain),
		wireObservation(plain, hexSpan(30), model.TypeSpan, func(o *model.Observation) {
			o.StartTime, o.EndTime = 3*day, 3*day+1
		}))

	cases := []struct {
		name   string
		filter TraceFilter
		want   []string
	}{
		{"release", TraceFilter{Release: []string{"2026.8.30"}}, []string{hexTrace(20)}},
		{"release that nothing carries", TraceFilter{Release: []string{"nope"}}, nil},
		{"version", TraceFilter{Version: "checkout-v9"}, []string{hexTrace(21), hexTrace(20)}},
		{"type", TraceFilter{Type: model.TypeTool}, []string{hexTrace(21), hexTrace(20)}},
		{"type nothing carries", TraceFilter{Type: model.TypeGuardrail}, nil},
		{
			name:   "prompt, any version",
			filter: TraceFilter{Prompt: &PromptFilter{Name: "support-answer"}},
			want:   []string{hexTrace(21), hexTrace(20)},
		},
		{
			name:   "prompt at a version",
			filter: TraceFilter{Prompt: &PromptFilter{Name: "support-answer", Version: ptr(int64(7))}},
			want:   []string{hexTrace(21)},
		},
		{
			name:   "prompt at a version nobody ran",
			filter: TraceFilter{Prompt: &PromptFilter{Name: "support-answer", Version: ptr(int64(5))}},
			want:   nil,
		},
		{
			name:   "a filter over observations beside one over the trace",
			filter: TraceFilter{Type: model.TypeTool, Release: []string{"2026.8.31"}},
			want:   []string{hexTrace(21)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter := c.filter
			filter.Limit = 50
			rows, err := s.Traces(project.ID, filter)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.ID)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("traces = %v, want %v", got, c.want)
			}

			// The count answers over the same conditions, which is
			// the half of the pair spec 009 #4 caps.
			count, err := s.CountTraces(project.ID, filter, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if count != len(c.want) {
				t.Errorf("count = %d, want %d", count, len(c.want))
			}
		})
	}

	// `type=generation` matches generations only: the filter is exact, and
	// it is the aggregates that treat an embedding as a model call
	// (spec 012, edge cases).
	t.Run("the type filter is exact", func(t *testing.T) {
		id := hexTrace(40)
		seedTrace(t, s, project.ID, wireTrace(id),
			wireObservation(id, hexSpan(40), model.TypeEmbedding, func(o *model.Observation) {
				o.StartTime, o.EndTime = 4*day, 4*day+1
			}))

		rows, err := s.Traces(project.ID, TraceFilter{Type: model.TypeGeneration, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == id {
				t.Error("type=generation matched a trace whose only model call is an embedding")
			}
		}
		rows, err = s.Traces(project.ID, TraceFilter{Type: model.TypeEmbedding, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != id {
			t.Errorf("type=embedding = %v, want just the embedding trace", rows)
		}
	})
}

// The new filters ride indexes of their own, and the outer scan still seeks
// the keyset (spec 012 #8). Without this the count of spec 009 #4 walks a
// trace's observations per candidate row.
func TestWireFilterPlans(t *testing.T) {
	s, project := readStore(t)

	cases := []struct {
		name   string
		filter TraceFilter
		index  string
	}{
		{"type", TraceFilter{Type: model.TypeTool}, "idx_observations_type"},
		{
			name:   "prompt",
			filter: TraceFilter{Prompt: &PromptFilter{Name: "support-answer", Version: ptr(int64(7))}},
			index:  "idx_observations_prompt",
		},
		{
			name:   "prompt without a version, on the index's prefix",
			filter: TraceFilter{Prompt: &PromptFilter{Name: "support-answer"}},
			index:  "idx_observations_prompt",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter := c.filter
			filter.Limit = 50
			filter.After = &TraceCursor{Timestamp: day, ID: hexTrace(1)}
			query, args := traceQuery(project.ID, filter)
			plan, err := s.explainQueryPlan(query, args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")

			if !strings.Contains(joined, c.index) {
				t.Errorf("plan does not use %s:\n%s", c.index, joined)
			}
			if strings.Contains(joined, "TEMP B-TREE") {
				t.Errorf("plan sorts through a temporary B-tree:\n%s", joined)
			}
		})
	}

	// The keyset survives the EXISTS: the outer scan is still a seek on
	// idx_traces_timestamp, which is the whole reason paging is keyset.
	t.Run("the keyset still seeks with an observation filter", func(t *testing.T) {
		query, args := traceQuery(project.ID, TraceFilter{
			Type:  model.TypeTool,
			Limit: 50,
			After: &TraceCursor{Timestamp: day, ID: hexTrace(1)},
		})
		plan, err := s.explainQueryPlan(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "idx_traces_timestamp") {
			t.Errorf("the outer scan left the keyset index:\n%s", joined)
		}
		if !strings.Contains(strings.ReplaceAll(joined, " ", ""), "(timestamp,id)<(?,?)") {
			t.Errorf("the cursor is not part of the index seek:\n%s", joined)
		}
	})

	// The release index pays off in the count rather than in the listing:
	// the listing has an ORDER BY that only idx_traces_timestamp can
	// satisfy without a sort, so SQLite rightly keeps the keyset index
	// there. The count has no ordering to preserve, and it is the read
	// spec 009 #4 caps precisely because it is the expensive one.
	t.Run("the release filter has an index where it can use one", func(t *testing.T) {
		query, args := traceCountQuery(project.ID, TraceFilter{Release: []string{"2026.8.30"}}, 1000)
		plan, err := s.explainQueryPlan(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "idx_traces_release") {
			t.Errorf("counting a release scans the project:\n%s", joined)
		}

		query, args = traceQuery(project.ID, TraceFilter{
			Release: []string{"2026.8.30"},
			Limit:   50,
			After:   &TraceCursor{Timestamp: day, ID: hexTrace(1)},
		})
		plan, err = s.explainQueryPlan(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "idx_traces_timestamp") || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("a release filter cost the listing its keyset seek:\n%s", joined)
		}
	})
}

// Widening the type column must not lose the embedding from any aggregate
// that sums model calls (spec 012 #2). No reader ever selected
// `type = 'generation'` — cost, usage and the model breakdown all key on the
// model column and on `provided_cost` — and this is what keeps that true.
func TestEmbeddingCountsAsAModelCall(t *testing.T) {
	s, project := readStore(t)
	id := hexTrace(50)

	seedTrace(t, s, project.ID, wireTrace(id),
		wireObservation(id, hexSpan(50), model.TypeGeneration, func(o *model.Observation) {
			o.Model = "claude-sonnet-5"
			o.CostDetails = map[string]any{"total": 0.007}
			o.Usage = map[string]any{"total": 2144}
		}),
		wireObservation(id, hexSpan(51), model.TypeEmbedding, func(o *model.Observation) {
			o.Model = "text-embedding-3-large"
			o.CostDetails = map[string]any{"total": 0.003}
			o.Usage = map[string]any{"total": 18}
		}),
	)

	trace, err := s.Trace(project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if trace.TotalCost == nil || *trace.TotalCost < 0.0099 || *trace.TotalCost > 0.0101 {
		t.Errorf("total_cost = %v, want both model calls summed", trace.TotalCost)
	}

	models := map[string]bool{}
	err = s.StatsSamples(project.ID, StatsFilter{GroupBy: GroupByModel}, func(sample StatsSample) {
		models[sample.Key] = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !models["text-embedding-3-large"] {
		t.Errorf("the model breakdown = %v, want the embedding's model in it", models)
	}
}

// `group_by=release` is the chart the release column exists for; a trace that
// named no release is a bucket rather than an omission (spec 012 #4).
func TestStatsGroupByRelease(t *testing.T) {
	s, project := readStore(t)

	seedTrace(t, s, project.ID,
		wireTrace(hexTrace(60), func(tr *model.Trace) { tr.Release = "2026.8.30" }),
		wireObservation(hexTrace(60), hexSpan(60), model.TypeSpan))
	seedTrace(t, s, project.ID, wireTrace(hexTrace(61)),
		wireObservation(hexTrace(61), hexSpan(61), model.TypeSpan))

	buckets := map[string]int{}
	if err := s.StatsSamples(project.ID, StatsFilter{GroupBy: GroupByRelease},
		func(sample StatsSample) { buckets[sample.Key]++ }); err != nil {
		t.Fatal(err)
	}
	if buckets["2026.8.30"] != 1 {
		t.Errorf("buckets = %v, want the released trace under its release", buckets)
	}
	if buckets[""] != 1 {
		t.Errorf("buckets = %v, want the trace with no release under the empty key", buckets)
	}
	if unit := StatsUnit(GroupByRelease); unit != "trace" {
		t.Errorf("unit = %q, want a release to count traces", unit)
	}
}

// The type column is closed by a CHECK, which is the reason schema 0008
// rebuilds the table rather than altering it (spec 012 #10).
func TestObservationTypeCheck(t *testing.T) {
	s, project := readStore(t)
	if _, err := s.db.Exec(
		`INSERT INTO traces (project_id, id, ingested_at) VALUES (?, ?, 1)`,
		project.ID, hexTrace(70)); err != nil {
		t.Fatal(err)
	}
	for i, kind := range model.ObservationTypes {
		if _, err := s.db.Exec(
			`INSERT INTO observations (project_id, trace_id, id, type) VALUES (?, ?, ?, ?)`,
			project.ID, hexTrace(70), hexSpan(700+i), kind); err != nil {
			t.Errorf("the column refused %q, which is one of the ten: %v", kind, err)
		}
	}
	_, err := s.db.Exec(
		`INSERT INTO observations (project_id, trace_id, id, type) VALUES (?, ?, ?, 'workflow-step')`,
		project.ID, hexTrace(70), hexSpan(99))
	if err == nil {
		t.Error("a spelling outside the vocabulary was stored; the CHECK is what keeps the column honest")
	}
}

// Prompt versions count from one, and the column says so rather than trusting
// the mapper to be the only writer. A version below one is one no `prompt=`
// string can ask for, since the filter reads a version as a run of digits
// (spec 012 #15, found in review of PR #19).
func TestPromptVersionCheck(t *testing.T) {
	s, project := readStore(t)
	if _, err := s.db.Exec(
		`INSERT INTO traces (project_id, id, ingested_at) VALUES (?, ?, 1)`,
		project.ID, hexTrace(71)); err != nil {
		t.Fatal(err)
	}
	insert := func(id string, version any) error {
		_, err := s.db.Exec(
			`INSERT INTO observations (project_id, trace_id, id, type, prompt_name, prompt_version)
			 VALUES (?, ?, ?, 'generation', 'svc', ?)`,
			project.ID, hexTrace(71), id, version)
		return err
	}
	if err := insert(hexSpan(710), 1); err != nil {
		t.Errorf("the column refused version 1: %v", err)
	}
	if err := insert(hexSpan(711), nil); err != nil {
		t.Errorf("the column refused an absent version: %v", err)
	}
	for _, version := range []int64{0, -1} {
		if err := insert(hexSpan(712), version); err == nil {
			t.Errorf("version %d was stored; nothing can filter for it", version)
		}
	}
}

// openAtSchemas builds a database as an earlier release left it: the named
// migrations applied and recorded, and nothing after them.
func openAtSchemas(t *testing.T, names ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tracepad.db")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func openAtSchema0006(t *testing.T) string {
	return openAtSchemas(t, "0001_init.sql", "0002_traces.sql", "0003_scores_prompts.sql",
		"0004_read_indexes.sql", "0005_retention_admin.sql", "0006_search.sql")
}

// Schema 0008 rebuilds `observations`, which is the shape of migration that
// can quietly lose rows, indexes or references. A database written before it
// keeps its collapsed types and answers the new fields as NULL — there is no
// backfill from raw bodies, by decision (spec 012 #1).
func TestMigration0008UpgradesAPopulated0006Database(t *testing.T) {
	path := openAtSchema0006(t)

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
		exec(`INSERT INTO projects (id, name) VALUES ('p1', 'app')`)
		exec(`INSERT INTO api_keys (public_key, secret_hash, project_id) VALUES ('tp-pk-1', X'00', 'p1')`)
		exec(`INSERT INTO payloads (id, compression, size_raw, body) VALUES (1, 'none', 2, X'7b7d')`)
		exec(`INSERT INTO traces (project_id, id, name, timestamp, ingested_at, observation_count)
		      VALUES ('p1', 'old', 'before the widening', 1, 1, 3)`)
		// The three types the old column allowed, including the
		// generation an embedding used to collapse into.
		for i, kind := range []string{"span", "generation", "event"} {
			exec(`INSERT INTO observations
			        (project_id, trace_id, id, type, level, start_time, end_time, input_id)
			      VALUES ('p1', 'old', ?, ?, 'DEFAULT', ?, ?, 1)`,
				hexSpan(i+1), kind, i+1, i+2)
		}
		exec(`INSERT INTO search_entries (project_id, trace_id, observation_id, field)
		      VALUES ('p1', 'old', ?, 'input')`, hexSpan(2))
	}()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()

	// Every row survived the rebuild, and nothing cascaded away with the
	// dropped table.
	for table, want := range map[string]int64{
		"projects": 1, "api_keys": 1, "traces": 1, "observations": 3,
		"payloads": 1, "search_entries": 1,
	} {
		var got int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %d after the upgrade, want %d", table, got, want)
		}
	}

	// The indexes the rebuild dropped are back, and the two the spec adds
	// are there. A missing index is invisible until a listing gets slow.
	indexes := map[string]bool{}
	rows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'observations'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes[name] = true
	}
	for _, name := range []string{
		"idx_observations_trace", "idx_observations_span",
		"idx_observations_type", "idx_observations_prompt",
	} {
		if !indexes[name] {
			t.Errorf("%s is missing after the rebuild; indexes present: %v", name, indexes)
		}
	}

	// The payload reference survived: with foreign keys off during the
	// migration, only `PRAGMA foreign_key_check` stands between a mistyped
	// SELECT list and a row pointing at nothing.
	var inputs int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM observations o JOIN payloads p ON p.id = o.input_id`).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if inputs != 3 {
		t.Errorf("%d observations still resolve their input payload, want 3", inputs)
	}

	// The old rows keep the types they were collapsed to, and answer the
	// new fields as NULL.
	observations, err := s.Observations("p1", "old", SkipIO)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 3 {
		t.Fatalf("read %d observations", len(observations))
	}
	for _, observation := range observations {
		if observation.CompletionStartTime != 0 || observation.PromptName != "" ||
			observation.PromptVersion != nil {
			t.Errorf("%s answered a new field on an old row: %+v", observation.ID, observation)
		}
	}
	trace, err := s.Trace("p1", "old")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Release != "" || trace.Version != "" || trace.TTFTMs != nil {
		t.Errorf("an old trace answered a new field: %+v", trace)
	}

	// The widened column accepts what it could not before.
	if _, err := s.db.Exec(
		`INSERT INTO observations (project_id, trace_id, id, type) VALUES ('p1', 'old', ?, 'tool')`,
		hexSpan(9)); err != nil {
		t.Errorf("the upgraded column refused a tool call: %v", err)
	}
}
