package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// What the write path costs, measured rather than asserted (spec 011, Testing
// #5): the same batches ingested with the search index on and with it off, so
// that "the write cost is bounded" (spec 011 #7) is a pair of numbers in the PR
// and not a hope.
//
// The corpus is synthetic and built here, the way `otlptest` builds the OTLP
// one: a batch is one trace and twenty observations carrying prompts and
// completions of the size real ones are. Nothing captured, nothing real.
//
//	go test ./internal/store -run '^$' -bench BenchmarkIngestBatch -benchtime 200x

const (
	benchObservations = 20
	benchPayloadBytes = 1200
)

func benchBatch(projectID string, n int, indexed bool) *IngestBatch {
	traceID := fmt.Sprintf("%032x", n)
	prompt := "summarise the ticket and decide whether a refund is owed: " +
		strings.Repeat("the customer wrote in about an order that never arrived. ", 20)
	completion := "the refund was approved because the carrier confirmed the loss. " +
		strings.Repeat("here is the reasoning in full. ", 30)
	batch := &IngestBatch{
		ProjectID:       projectID,
		Traces:          []*model.Trace{{ID: traceID, Name: "support-chat", UserID: "u1", SessionID: "s1"}},
		skipSearchIndex: !indexed,
	}
	for i := range benchObservations {
		batch.Observations = append(batch.Observations, &model.Observation{
			TraceID: traceID, ID: fmt.Sprintf("%016x", n*100+i),
			Type: model.TypeGeneration, Level: model.LevelDefault, Name: "answer",
			Model: "gpt-4o", StartTime: int64(n)*day + int64(i), EndTime: int64(n)*day + int64(i) + 1,
			Input:    map[string]any{"messages": prompt[:min(len(prompt), benchPayloadBytes)]},
			Output:   map[string]any{"content": completion[:min(len(completion), benchPayloadBytes)]},
			Metadata: map[string]any{"deployment": "eu-central", "attempt": 1},
		})
	}
	return batch
}

func BenchmarkIngestBatch(b *testing.B) {
	for _, indexed := range []bool{true, false} {
		name := "with-search-index"
		if !indexed {
			name = "without-search-index"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { s.Close() })
			project, err := s.CreateProject("bench",
				KeyPair{PublicKey: "tp-pk-bench", Secret: "tp-sk-bench"})
			if err != nil {
				b.Fatal(err)
			}

			// One transaction per batch, which is what the group-commit
			// writer does inside its window; going through the writer
			// would measure the window instead of the work.
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				tx, err := s.db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if err := benchBatch(project.ID, i, indexed).apply(tx); err != nil {
					tx.Rollback()
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(benchObservations), "spans/batch")
		})
	}
}

// What the two indexes spec 012 #8 adds cost the write path, measured the way
// spec 011 #7 set the precedent for: the same batches ingested with them and
// without them, so "two indexes on the biggest table" is a pair of numbers in
// the PR rather than a shrug.
//
// The batches carry a prompt on half their observations, which is what makes
// the partial index do work: an index on `prompt_name IS NOT NULL` is free for
// the observations that carry none, and measuring only those would measure
// nothing.
//
//	go test ./internal/store -run '^$' -bench BenchmarkIngestWireIndexes -benchtime 200x
func BenchmarkIngestWireIndexes(b *testing.B) {
	for _, indexed := range []bool{true, false} {
		name := "with-wire-indexes"
		if !indexed {
			name = "without-wire-indexes"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { s.Close() })
			if !indexed {
				for _, index := range []string{"idx_observations_type", "idx_observations_prompt"} {
					if _, err := s.db.Exec(`DROP INDEX ` + index); err != nil {
						b.Fatal(err)
					}
				}
			}
			project, err := s.CreateProject("bench",
				KeyPair{PublicKey: "tp-pk-bench", Secret: "tp-sk-bench"})
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				tx, err := s.db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if err := wireBenchBatch(project.ID, i).apply(tx); err != nil {
					tx.Rollback()
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(benchObservations), "spans/batch")
		})
	}
}

// wireBenchBatch is benchBatch with the columns spec 012 adds: the type
// vocabulary spread over the batch and a prompt on every second observation.
func wireBenchBatch(projectID string, n int) *IngestBatch {
	batch := benchBatch(projectID, n, true)
	kinds := []string{model.TypeGeneration, model.TypeTool, model.TypeAgent, model.TypeEmbedding}
	for i, observation := range batch.Observations {
		observation.Type = kinds[i%len(kinds)]
		observation.CompletionStartTime = observation.StartTime + int64(i)
		if i%2 == 0 {
			version := int64(i%7 + 1)
			observation.PromptName, observation.PromptVersion = "support-answer", &version
		}
	}
	batch.Traces[0].Release, batch.Traces[0].Version = "2026.8.30", "checkout-v9"
	return batch
}

// What the run link costs the write path (spec 014, Testing — cost): the same
// batches ingested with `tracepad.run_id` / `tracepad.item_id` on the trace
// and without them. With them, every trace pays one primary-key lookup on
// `dataset_runs` inside its transaction (spec 014 #3) plus the two columns
// and their index; without them the lookup is skipped entirely, and that
// number must sit inside the noise of BenchmarkIngestBatch.
//
//	go test ./internal/store -run '^$' -bench BenchmarkIngestRunLink -benchtime 200x
func BenchmarkIngestRunLink(b *testing.B) {
	for _, linked := range []bool{true, false} {
		name := "with-run-link"
		if !linked {
			name = "without-run-link"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { s.Close() })
			project, err := s.CreateProject("bench",
				KeyPair{PublicKey: "tp-pk-bench", Secret: "tp-sk-bench"})
			if err != nil {
				b.Fatal(err)
			}
			runID := strings.Repeat("a", 32)
			if linked {
				// A real run, so the lookup finds a row rather than
				// missing every time.
				for _, job := range []WriteJob{
					&DatasetItemsWrite{ProjectID: project.ID, Dataset: "bench", Now: 1,
						Items: []*DatasetItemInput{{ID: strings.Repeat("b", 32), Input: []byte(`{}`)}}},
					&RunCreate{ProjectID: project.ID, Dataset: "bench", ID: runID, Now: 1},
				} {
					tx, err := s.db.Begin()
					if err != nil {
						b.Fatal(err)
					}
					if err := job.apply(tx); err != nil {
						b.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						b.Fatal(err)
					}
				}
			}

			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				batch := benchBatch(project.ID, i, true)
				if linked {
					batch.Traces[0].RunID = runID
					batch.Traces[0].ItemID = fmt.Sprintf("%032x", i%200)
				}
				tx, err := s.db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if err := batch.apply(tx); err != nil {
					tx.Rollback()
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(benchObservations), "spans/batch")
		})
	}
}

// BenchmarkSearchBackfill is the rate spec 011 #8 asks to be written into the
// retention docs: how long the first start after the upgrade takes per trace.
//
//	go test ./internal/store -run '^$' -bench BenchmarkSearchBackfill -benchtime 1x
func BenchmarkSearchBackfill(b *testing.B) {
	const traces = 500

	s, err := Open(filepath.Join(b.TempDir(), "backfill.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	project, err := s.CreateProject("bench", KeyPair{PublicKey: "tp-pk-bench", Secret: "tp-sk-bench"})
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= traces; i++ {
		tx, err := s.db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		if err := benchBatch(project.ID, i, true).apply(tx); err != nil {
			b.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}

	for b.Loop() {
		b.StopTimer()
		for _, statement := range []string{
			`DELETE FROM search_fts`,
			`DELETE FROM search_entries`,
			`UPDATE search_backfill SET done_at = NULL WHERE id = 1`,
		} {
			if _, err := s.db.Exec(statement); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		if err := s.backfillSearchIndex(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(traces), "traces/op")
	b.ReportMetric(float64(traces*benchObservations), "spans/op")
}
