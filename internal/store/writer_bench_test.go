package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

// What a commit window full of small jobs costs through the writer, at its
// production window and on its own connection, and what one refused job in it
// costs (spec 043 #38): 64 jobs submitted at once, each iteration a new set,
// of three shapes. Ingest: 64 one-span exports. Scores: 64 writes of one score
// each. Mixed: 32 and 32, so that a job of one kind follows a job of the other.
// A clean window is applied plainly whatever the shape. In each, "refused"
// makes one of the jobs — which one depends on the order the goroutines reach
// the writer — fail with a rejection, the way a queue that does not exist or an
// echo that does not match fails: the window is applied again with a savepoint
// for every job.
//
//	go test ./internal/store -run '^$' -bench BenchmarkWriterWindow -benchtime 100x -count 8
func BenchmarkWriterWindow(b *testing.B) {
	previous := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
	b.Cleanup(func() { logger = previous })
	rejection := &weighedJob{rows: 1, do: func(*sql.Tx) error {
		return &Rejection{Kind: RejectNotFound, Message: `queue "gone" not found`}
	}}
	score := func(projectID string, n, i int) WriteJob {
		one := 1.0
		return &ScoreWrite{ProjectID: projectID, Scores: []*Score{{ID: fmt.Sprintf("s-%d-%d", n, i),
			SessionID: "sess", Name: "q", DataType: "numeric", Value: &one, Timestamp: 1}}}
	}
	ingest := func(projectID string, n, i int) WriteJob {
		return batchFor(projectID, fmt.Sprintf("%016x%016x", n, i), fmt.Sprintf("%016x", i))
	}
	for _, shape := range []struct {
		name string
		job  func(projectID string, n, i int) WriteJob
	}{
		{"ingest", ingest},
		{"scores", score},
		{"mixed", func(projectID string, n, i int) WriteJob {
			if i%2 == 0 {
				return ingest(projectID, n, i)
			}
			return score(projectID, n, i)
		}},
	} {
		for _, refused := range []bool{false, true} {
			b.Run(fmt.Sprintf("64-%s/refused=%v", shape.name, refused), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { s.Close() })
				p, err := s.CreateProject("bench", KeyPair{PublicKey: "tp-pk-bench", Secret: "tp-sk-bench"})
				if err != nil {
					b.Fatal(err)
				}
				w, err := s.NewWriter(WriterOptions{})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { w.Close() })

				for n := 0; b.Loop(); n++ {
					b.StopTimer()
					jobs := make([]WriteJob, 64)
					for i := range jobs {
						jobs[i] = shape.job(p.ID, n, i)
					}
					if refused {
						jobs[63] = rejection
					}
					b.StartTimer()
					var wg sync.WaitGroup
					for _, job := range jobs {
						wg.Go(func() {
							err := w.Submit(context.Background(), job)
							if (err != nil) != (job == rejection) {
								b.Error(err)
							}
						})
					}
					wg.Wait()
				}
			})
		}
	}
}
