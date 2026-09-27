package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// What an export costs from the request to its `200`, through the handler and
// the group-commit writer at its production window, measured rather than
// asserted (spec 043, Testing): the slices of #11 must not make an ordinary
// batch slower. Three sizes — the 512 spans an OpenTelemetry batch processor
// sends by default, the Collector's default batch of 8,192, and the ceiling of
// 20,000 — each a new set of traces every iteration, so every row is an
// insert, as live traffic is.
//
//	go test ./internal/server -run '^$' -bench BenchmarkIngestExport -benchtime 20x -count 5
func BenchmarkIngestExport(b *testing.B) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, shape := range []struct {
		name                  string
		traces, spansPerTrace int
	}{
		{"512-spans", 32, 16},
		{"8192-spans", 512, 16},
		{"20000-spans", 1250, 16},
	} {
		b.Run(shape.name, func(b *testing.B) {
			st, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { st.Close() })
			if _, err := st.CreateProject("bench", store.KeyPair{PublicKey: testPublic, Secret: testSecret}); err != nil {
				b.Fatal(err)
			}
			writer, err := st.NewWriter(store.WriterOptions{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { writer.Close() })
			srv := New(&config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes},
				"bench", st, writer, nil)

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				body, err := mapping.EncodeExportRequest(otlptest.Bulk(i, shape.traces, shape.spansPerTrace))
				if err != nil {
					b.Fatal(err)
				}
				req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Authorization", "Bearer "+testSecret)
				rec := httptest.NewRecorder()
				b.StartTimer()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
			}
			b.ReportMetric(float64(shape.traces*shape.spansPerTrace), "spans/export")
		})
	}
}
