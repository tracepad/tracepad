// Package e2e runs the package against a real binary (spec 033, Testing).
//
// Everything below the package — the OTLP encoding, the transport, the auth,
// the mapper's table, the columns — only breaks at a seam the unit layer
// cannot see. So this boots the store on a temporary database, exports
// through the package's own exporter, posts a score and fetches a prompt,
// and reads all of it back through the API a person would read it with.
//
// TRACEPAD_BINARY names the binary; scripts/sdk-go-test.sh builds it and
// sets it. Without it the package skips, so `go test ./...` stays a unit run.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

const key = "tp-sk-e2e"

// store is a running binary, and the read API as a person would call it.
type store struct {
	host string
	t    *testing.T
}

func serve(t *testing.T) *store {
	t.Helper()
	binary := os.Getenv("TRACEPAD_BINARY")
	if binary == "" {
		t.Skip("TRACEPAD_BINARY is not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cmd := exec.Command(binary, "serve")
	cmd.Env = append(os.Environ(),
		"TRACEPAD_DATA_DIR="+t.TempDir(),
		fmt.Sprintf("TRACEPAD_LISTEN=127.0.0.1:%d", port),
		"TRACEPAD_PROJECTS=e2e:tp-pk-e2e:"+key)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait in the background: a server that exits at once is reported at
	// once, not after the health poll has run out.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	s := &store{host: fmt.Sprintf("http://127.0.0.1:%d", port), t: t}
	for i := 0; i < 100; i++ {
		select {
		case <-exited:
			t.Fatalf("the server exited: %s", output.String())
		default:
		}
		if _, err := s.try("GET", "/health", nil); err == nil {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the server never became healthy: %s", output.String())
	return nil
}

func (s *store) try(method, path string, body any) (map[string]any, error) {
	var payload io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.host+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	answer, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer answer.Body.Close()
	raw, _ := io.ReadAll(answer.Body)
	if answer.StatusCode < 200 || answer.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, answer.StatusCode, raw)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return decoded, nil
}

func (s *store) call(method, path string, body any) map[string]any {
	s.t.Helper()
	answer, err := s.try(method, path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	return answer
}

// trace is the trace with its payloads, once the export has landed.
func (s *store) trace(id string) map[string]any {
	s.t.Helper()
	var err error
	for i := 0; i < 50; i++ {
		var answer map[string]any
		if answer, err = s.try("GET", "/api/v1/traces/"+id+"?expand=io", nil); err == nil {
			return answer
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("trace %s never arrived: %v", id, err)
	return nil
}

// walk flattens the observation tree depth-first.
func walk(observations any) []map[string]any {
	var out []map[string]any
	list, _ := observations.([]any)
	for _, item := range list {
		o, _ := item.(map[string]any)
		out = append(out, o)
		out = append(out, walk(o["children"])...)
	}
	return out
}

func names(observations []map[string]any) []string {
	var out []string
	for _, o := range observations {
		out = append(out, fmt.Sprint(o["name"]))
	}
	return out
}

// flush delivers the scores and the spans, with the patience an export
// through a real socket needs.
func flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := tracepad.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
