package mapping_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

var update = flag.Bool("update", false, "regenerate testdata/otlp bodies and testdata/golden files")

const (
	fixtureDir = "../../testdata/otlp"
	goldenDir  = "../../testdata/golden"
)

// TestGoldenFixtures replays every fixture body through decode → map and
// diffs the result against its golden file (spec 002, Testing #1). The
// fixtures on disk are the contract: `-update` rewrites both sides from the
// builders in internal/otlptest, and the diff is what gets reviewed.
func TestGoldenFixtures(t *testing.T) {
	if *update {
		writeFixtures(t)
	}

	bodies, err := filepath.Glob(filepath.Join(fixtureDir, "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatalf("no fixtures in %s; run: go test ./internal/mapping -update", fixtureDir)
	}

	for _, body := range bodies {
		name := strings.TrimSuffix(filepath.Base(body), ".pb")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(body)
			if err != nil {
				t.Fatal(err)
			}
			resourceSpans, err := mapping.DecodeExportRequest(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := mapping.Map(resourceSpans).DebugJSON()
			if err != nil {
				t.Fatalf("encode result: %v", err)
			}

			goldenPath := filepath.Join(goldenDir, name+".json")
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("mapping of %s changed.\n--- got ---\n%s\n--- want ---\n%s",
					name, got, want)
			}
		})
	}
}

// writeFixtures regenerates the corpus. Both the bodies and the goldens come
// from one run so they can never disagree about what was mapped.
func writeFixtures(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range otlptest.Fixtures() {
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			t.Fatalf("encode %s: %v", fixture.Name, err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, fixture.Name+".pb"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		golden, err := mapping.Map(fixture.ResourceSpans).DebugJSON()
		if err != nil {
			t.Fatalf("map %s: %v", fixture.Name, err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, fixture.Name+".json"), golden, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := mapping.DecodeExportRequest([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected a decode error for a non-protobuf body")
	}
}

// An empty body is a valid, empty export: OTLP says so, and answering 400
// would make idle exporters look broken.
func TestDecodeEmptyBody(t *testing.T) {
	spans, err := mapping.DecodeExportRequest(nil)
	if err != nil || len(spans) != 0 {
		t.Fatalf("spans=%v err=%v", spans, err)
	}
}

// A fully accepted export answers with an empty message; only a partial
// success carries a body.
func TestEncodeExportResponse(t *testing.T) {
	if body := mapping.EncodeExportResponse(0, ""); len(body) != 0 {
		t.Fatalf("full success should encode to an empty message, got %d bytes", len(body))
	}
	if body := mapping.EncodeExportResponse(2, "skipped spans (invalid span id: 2)"); len(body) == 0 {
		t.Fatal("partial success should encode a body")
	}
}
