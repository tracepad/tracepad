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

func benchBatch(projectID string, n int) *IngestBatch {
	traceID := fmt.Sprintf("%032x", n)
	prompt := "summarise the ticket and decide whether a refund is owed: " +
		strings.Repeat("the customer wrote in about an order that never arrived. ", 20)
	completion := "the refund was approved because the carrier confirmed the loss. " +
		strings.Repeat("here is the reasoning in full. ", 30)
	batch := &IngestBatch{
		ProjectID: projectID,
		Traces:    []*model.Trace{{ID: traceID, Name: "support-chat", UserID: "u1", SessionID: "s1"}},
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
			was := searchIndexEnabled
			searchIndexEnabled = indexed
			b.Cleanup(func() { searchIndexEnabled = was })

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
				if err := benchBatch(project.ID, i).apply(tx); err != nil {
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
		if err := benchBatch(project.ID, i).apply(tx); err != nil {
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
