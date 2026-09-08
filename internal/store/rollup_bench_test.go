package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// What the rollup buys, measured rather than asserted (spec 013, Testing):
// the cost of one pass, and the cost of a month's chart read each way.
//
// The corpus is a synthetic month at a modest rate — a month is the range
// that motivates the spec, and the live scan's cost is linear in the rows it
// holds while the rollup's is linear in the hours.

const (
	benchHours         = 24 * 30
	benchTracesPerHour = 50
	benchSpansPerTrace = 1 // each carrying a model and a cost
	// benchUsers is how many distinct end users the corpus spreads over, and
	// benchTracesPerSession how many traces one session of theirs holds. The
	// per-user rollup's cost is what these two decide (spec 023 #1): rows are
	// active-user-hours, and a pass with a user id on every trace is what the
	// PR times against one without.
	benchUsers            = 200
	benchTracesPerSession = 5
	benchStartHourEpoch   = int64(1780000000) / SecondsPerHour * SecondsPerHour
)

// seedMonth writes the corpus once and hands back the store.
func seedMonth(b *testing.B) (*Store, *Project) {
	b.Helper()
	s, err := Open(b.TempDir() + "/tracepad.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	project, err := s.CreateProject("bench", KeyPair{PublicKey: "tp-pk-b", Secret: "tp-sk-b"})
	if err != nil {
		b.Fatal(err)
	}
	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer writer.Close()

	cost := 0.002
	n := 0
	for hour := range benchHours {
		start := (benchStartHourEpoch + int64(hour)*SecondsPerHour) * 1e9
		batch := &IngestBatch{ProjectID: project.ID}
		for i := range benchTracesPerHour {
			n++
			id := fmt.Sprintf("%032x", n)
			at := start + int64(i)*int64(time.Second)
			batch.Traces = append(batch.Traces, &model.Trace{
				ID: id, Name: "run", Environment: "production", Release: "2026.8.30",
				UserID: fmt.Sprintf("user-%04d", n%benchUsers),
				SessionID: fmt.Sprintf("sess-%04d-%d",
					n%benchUsers, n/(benchUsers*benchTracesPerSession)),
			})
			for j := range benchSpansPerTrace {
				batch.Observations = append(batch.Observations, &model.Observation{
					TraceID: id, ID: fmt.Sprintf("%016x", n*10+j),
					Type: model.TypeGeneration, Name: "call", Level: model.LevelDefault,
					Model: "claude-sonnet-5", StartTime: at, EndTime: at + int64(200+i)*1e6,
					CostDetails: map[string]any{"total": cost},
				})
			}
		}
		if err := writer.Submit(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
	return s, project
}

// BenchmarkRollupPass times the aggregator over a month of raw rows — which
// is also what the first pass on an existing database costs, since that pass
// is the backfill (spec 013 #5).
// The two commit windows are measured because a pass is many small jobs, and
// a job's cost is its SQL plus its wait for the group-commit flush. Which of
// the two dominates decides whether a long backfill is worth tuning, and
// guessing at it would be the kind of claim this project does not make.
func BenchmarkRollupPass(b *testing.B) {
	for _, window := range []struct {
		name   string
		commit time.Duration
	}{
		{"default-commit-window", 0},
		{"tight-commit-window", time.Millisecond},
	} {
		b.Run(window.name, func(b *testing.B) {
			s, project := seedMonth(b)
			writer, err := s.NewWriter(WriterOptions{CommitWindow: window.commit})
			if err != nil {
				b.Fatal(err)
			}
			defer writer.Close()
			at := time.Unix(benchStartHourEpoch+int64(benchHours+1)*SecondsPerHour, 0)
			aggregator := s.NewAggregator(writer, RollupOptions{
				Interval: time.Minute,
				Now:      func() time.Time { return at },
			})

			b.ResetTimer()
			for range b.N {
				if err := aggregator.Pass(context.Background()); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				// The next iteration must do the same work, not
				// find it already done.
				if _, err := s.db.Exec(
					`DELETE FROM stats_rollup WHERE project_id = ?`, project.ID); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkStatsMonth is the read the spec exists for: a month grouped by
// day, answered by the live scan and by the rollup.
func BenchmarkStatsMonth(b *testing.B) {
	s, project := seedMonth(b)
	from := benchStartHourEpoch * 1e9
	to := (benchStartHourEpoch + int64(benchHours)*SecondsPerHour) * 1e9

	b.Run("live-scan", func(b *testing.B) {
		for range b.N {
			var rows int
			err := s.StatsSamples(project.ID, StatsFilter{
				From: &from, To: &to, GroupBy: GroupByDay,
			}, func(StatsSample) { rows++ })
			if err != nil {
				b.Fatal(err)
			}
			if rows != benchHours*benchTracesPerHour {
				b.Fatalf("scanned %d rows, want the whole month", rows)
			}
		}
	})

	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer writer.Close()
	at := time.Unix(benchStartHourEpoch+int64(benchHours+1)*SecondsPerHour, 0)
	aggregator := s.NewAggregator(writer, RollupOptions{
		Interval: time.Minute, Now: func() time.Time { return at },
	})
	if err := aggregator.Pass(context.Background()); err != nil {
		b.Fatal(err)
	}

	b.Run("rollup", func(b *testing.B) {
		for range b.N {
			var rows int
			err := s.StatsRollupRows(project.ID,
				benchStartHourEpoch, benchStartHourEpoch+int64(benchHours)*SecondsPerHour, nil,
				func(StatsRow) { rows++ })
			if err != nil {
				b.Fatal(err)
			}
			if rows == 0 {
				b.Fatal("the rollup answered nothing; the pass did not run")
			}
		}
	})
}
