package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// What the third table costs, measured rather than asserted (spec 025,
// Testing): one pass over the benchmark month with scores on it, against the
// same pass over the same month without any.
//
// The pass is the number that matters, because the first pass after an upgrade
// re-rolls the whole rolled history (Decision 5) and because a pass runs every
// five minutes for ever afterwards.

// seedMonthScores grades every trace of the benchmark month twice — a numeric
// name and a categorical one, which is a modest eval loop and enough for both
// row shapes to exist.
func seedMonthScores(b *testing.B, s *Store, project *Project) {
	b.Helper()
	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer writer.Close()

	traces, err := s.db.Query(
		`SELECT id, timestamp FROM traces WHERE project_id = ?`, project.ID)
	if err != nil {
		b.Fatal(err)
	}
	type graded struct {
		id string
		at int64
	}
	var all []graded
	for traces.Next() {
		var one graded
		if err := traces.Scan(&one.id, &one.at); err != nil {
			b.Fatal(err)
		}
		all = append(all, one)
	}
	traces.Close()
	if err := traces.Err(); err != nil {
		b.Fatal(err)
	}

	// In batches, the way an eval loop posts them.
	const batch = 500
	for start := 0; start < len(all); start += batch {
		end := min(start+batch, len(all))
		write := &ScoreWrite{ProjectID: project.ID}
		for i := start; i < end; i++ {
			value := float64(i%100) / 100
			verdict := "pass"
			if i%3 == 0 {
				verdict = "fail"
			}
			write.Scores = append(write.Scores,
				&Score{ID: fmt.Sprintf("%032x", 2*i+1), TraceID: all[i].id,
					Name: "hallucination", DataType: ScoreNumeric, Value: &value,
					Timestamp: all[i].at, CreatedAt: all[i].at},
				&Score{ID: fmt.Sprintf("%032x", 2*i+2), TraceID: all[i].id,
					Name: "verdict", DataType: ScoreCategorical, StringValue: &verdict,
					Timestamp: all[i].at, CreatedAt: all[i].at})
		}
		if err := writer.Submit(context.Background(), write); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRollupPassWithScores is `BenchmarkRollupPass` over a corpus that has
// been graded. The two benchmarks together are what the PR reports: the
// difference is what `scores_hourly` costs a pass.
//
// Both commit windows, for the reason the other one measures both: a pass is
// many small jobs, and under the default window most of a job's wall clock is
// its wait for the group-commit flush. A third `INSERT … SELECT` inside a
// transaction that is already open shows up in the tight window or nowhere.
func BenchmarkRollupPassWithScores(b *testing.B) {
	for _, window := range []struct {
		name   string
		commit time.Duration
	}{
		{"default-commit-window", 0},
		{"tight-commit-window", time.Millisecond},
	} {
		b.Run(window.name, func(b *testing.B) {
			s, project := seedMonth(b)
			seedMonthScores(b, s, project)

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
				// The next iteration must do the same work, not find it
				// already done.
				if _, err := s.db.Exec(
					`DELETE FROM stats_rollup WHERE project_id = ?`, project.ID); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkScoreTrendsMonth is the read this spec exists for: a month of one
// score name, answered by the live scan of the raw rows and by the rollup.
func BenchmarkScoreTrendsMonth(b *testing.B) {
	s, project := seedMonth(b)
	seedMonthScores(b, s, project)

	from := benchStartHourEpoch * 1e9
	to := (benchStartHourEpoch + int64(benchHours)*SecondsPerHour) * 1e9

	b.Run("live-scan", func(b *testing.B) {
		for range b.N {
			var rows int
			if err := s.ScoreSamples(project.ID, from, to, nil, "hallucination",
				func(ScoreStatsRow) { rows++ }); err != nil {
				b.Fatal(err)
			}
			if rows != benchHours*benchTracesPerHour {
				b.Fatalf("scanned %d rows, want one score per trace of the month", rows)
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
			if err := s.ScoresRollupRows(project.ID,
				benchStartHourEpoch, benchStartHourEpoch+int64(benchHours)*SecondsPerHour,
				nil, "hallucination", func(ScoreStatsRow) { rows++ }); err != nil {
				b.Fatal(err)
			}
			if rows == 0 {
				b.Fatal("the rollup answered nothing; the pass did not run")
			}
		}
	})
}
