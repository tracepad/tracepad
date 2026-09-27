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

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

const key = "tp-sk-e2e-0000000000000000000000000000"

// adminToken mints keys: that is a person's act or the admin token's, never
// a key's (spec 045 #4).
const adminToken = "tp-admin-e2e-00000000000000000000000000"

// store is a running binary, and the read API as a person would call it.
type store struct {
	host string
	t    *testing.T
}

// The one store and the one Init of the package: Init is process-wide by
// design, so every test here shares them. The application's own provider
// is set global before Init, which adopts it (spec 017 #2): its spans and
// the package's are one pipeline, and a framework's span is a root.
var (
	shared      *store
	application *sdktrace.TracerProvider
)

func TestMain(m *testing.M) {
	binary := os.Getenv("TRACEPAD_BINARY")
	if binary == "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "tracepad-e2e")
	if err != nil {
		panic(err)
	}
	stop, s, err := boot(binary, dir)
	if err != nil {
		panic(err)
	}
	shared = s
	application = sdktrace.NewTracerProvider()
	otel.SetTracerProvider(application)
	shutdown, err := tracepad.Init(context.Background(), tracepad.WithHost(s.host), tracepad.WithKey(key))
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = shutdown(context.Background())
	stop()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// serve is the shared store, or a skip without a binary.
func serve(t *testing.T) *store {
	t.Helper()
	if shared == nil {
		t.Skip("TRACEPAD_BINARY is not set")
	}
	return &store{host: shared.host, t: t}
}

// boot starts the binary on a free port and waits for it to answer; stop
// kills it and reaps it.
func boot(binary, dir string) (stop func(), s *store, err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cmd := exec.Command(binary, "serve")
	cmd.Env = append(os.Environ(),
		"TRACEPAD_DATA_DIR="+dir,
		fmt.Sprintf("TRACEPAD_LISTEN=127.0.0.1:%d", port),
		"TRACEPAD_PROJECTS=e2e:tp-pk-e2e:"+key,
		"TRACEPAD_ADMIN_TOKEN="+adminToken)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	// Wait in the background: a server that exits at once is reported at
	// once, not after the health poll has run out.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop = func() {
		_ = cmd.Process.Kill()
		<-exited
	}
	s = &store{host: fmt.Sprintf("http://127.0.0.1:%d", port)}
	for i := 0; i < 100; i++ {
		select {
		case <-exited:
			return nil, nil, fmt.Errorf("the server exited: %s", output.String())
		default:
		}
		if _, err := s.try("GET", "/health", nil); err == nil {
			return stop, s, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return nil, nil, fmt.Errorf("the server never became healthy: %s", output.String())
}

func (s *store) try(method, path string, body any) (map[string]any, error) {
	return s.as(key, method, path, body)
}

func (s *store) as(token, method, path string, body any) (map[string]any, error) {
	var payload io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.host+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

// mint is the secret of a new key of the project carrying only scopes.
func (s *store) mint(scopes ...string) string {
	s.t.Helper()
	projects, _ := s.call("GET", "/api/v1/projects", nil)["projects"].([]any)
	if len(projects) != 1 {
		s.t.Fatalf("projects = %v, want the one", projects)
	}
	id, _ := projects[0].(map[string]any)["id"].(string)
	minted, err := s.as(adminToken, "POST", "/api/v1/projects/"+id+"/keys", map[string]any{"scopes": scopes})
	if err != nil {
		s.t.Fatal(err)
	}
	secret, _ := minted["secret_key"].(string)
	return secret
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
