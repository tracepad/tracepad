package tracepad

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// promptServer answers every prompt fetch with the body, counting the calls;
// `status` other than 200 is answered with an error body instead.
type promptServer struct {
	*httptest.Server
	calls  atomic.Int32
	status atomic.Int32
	maxAge string
}

func servePrompts(t *testing.T, body string) *promptServer {
	t.Helper()
	ps := &promptServer{maxAge: "max-age=60"}
	ps.status.Store(200)
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", 401)
			return
		}
		if status := int(ps.status.Load()); status != 200 {
			http.Error(w, `{"error":"away"}`, status)
			return
		}
		w.Header().Set("Cache-Control", ps.maxAge)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ps.Close)
	return ps
}

const textPrompt = `{"name":"support-answer","version":3,"type":"text","prompt":"Answer {topic} for {product}.","labels":["production"],"config":{"model":"claude"}}`
const chatPrompt = `{"name":"chat","version":1,"type":"chat","prompt":[{"role":"system","content":"You are {tone}."},{"role":"user","content":"{{literal}} {question}"}]}`

func TestAPromptIsFetchedAndCached(t *testing.T) {
	fresh(t)
	ps := servePrompts(t, textPrompt)
	t.Setenv("TRACEPAD_HOST", ps.URL)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	// No Init: a script that only fetches a prompt configures through the
	// environment.
	p, err := Prompt(context.Background(), "support-answer", WithLabel("production"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "support-answer" || p.Version != 3 || p.Type != "text" || p.Labels[0] != "production" ||
		p.Config["model"] != "claude" || p.Text != "Answer {topic} for {product}." {
		t.Errorf("prompt = %+v", p)
	}
	again, _ := Prompt(context.Background(), "support-answer", WithLabel("production"))
	if again != p || ps.calls.Load() != 1 {
		t.Errorf("calls = %d, want the second fetch served from the cache", ps.calls.Load())
	}
	// A version and a label are different entries.
	_, _ = Prompt(context.Background(), "support-answer", WithVersion(3))
	if ps.calls.Load() != 2 {
		t.Errorf("calls = %d", ps.calls.Load())
	}
}

func TestTheCacheExpiresWhenTheServerSaidItWould(t *testing.T) {
	setup(t)
	ps := servePrompts(t, textPrompt)
	ps.maxAge = "max-age=0"
	def.config.host = ps.URL
	_, _ = Prompt(context.Background(), "support-answer")
	_, _ = Prompt(context.Background(), "support-answer")
	if ps.calls.Load() != 2 {
		t.Errorf("calls = %d, want a fetch each time under max-age=0", ps.calls.Load())
	}
}

func TestAServerErrorIsServedStale(t *testing.T) {
	r := setup(t)
	ps := servePrompts(t, textPrompt)
	ps.maxAge = "max-age=0"
	def.config.host = ps.URL
	first, err := Prompt(context.Background(), "support-answer")
	if err != nil {
		t.Fatal(err)
	}
	ps.status.Store(503)
	stale, err := Prompt(context.Background(), "support-answer")
	if err != nil || stale != first {
		t.Errorf("stale = %v, err = %v", stale, err)
	}
	if !strings.Contains(r.logs.String(), "stale cache") {
		t.Errorf("logs = %q", r.logs.String())
	}
	// A client error is about the request, and is returned even with
	// something cached.
	ps.status.Store(404)
	var h *HTTPError
	if _, err := Prompt(context.Background(), "support-answer"); !errors.As(err, &h) || h.Status != 404 {
		t.Errorf("err = %v, want the 404", err)
	}
}

func TestNothingCachedIsAnError(t *testing.T) {
	setup(t)
	ps := servePrompts(t, textPrompt)
	ps.status.Store(503)
	def.config.host = ps.URL
	var h *HTTPError
	if _, err := Prompt(context.Background(), "support-answer"); !errors.As(err, &h) || h.Status != 503 {
		t.Errorf("err = %v", err)
	}
	// The transport's own error, when nothing answered at all.
	def.config.host = "http://127.0.0.1:1"
	if _, err := Prompt(context.Background(), "support-answer"); err == nil || errors.As(err, &h) {
		t.Errorf("err = %v, want the transport's", err)
	}
}

func TestCompileSubstitutesInTextAndInMessages(t *testing.T) {
	text := &PromptVersion{Text: "Answer {topic} for {product}; {unknown} stays; {{braces}}"}
	got := text.Compile(map[string]any{"topic": "resets", "product": 3})
	if got.Text != "Answer resets for 3; {unknown} stays; {braces}" || got.Messages != nil {
		t.Errorf("compiled = %+v", got)
	}
	chat := readPrompt(object(response{body: decode([]byte(chatPrompt))}))
	compiled := chat.Compile(map[string]any{"tone": "terse", "question": "why?"})
	if compiled.Text != "" || len(compiled.Messages) != 2 ||
		compiled.Messages[0] != (Message{"system", "You are terse."}) ||
		compiled.Messages[1] != (Message{"user", "{literal} why?"}) {
		t.Errorf("compiled = %+v", compiled)
	}
}

// The Python and Node packages run the same table: one stored text, compiled
// with string variables, is one prompt in all three — except where a case
// says what Go gives instead, because Go never fails a compile, leaves a
// placeholder no variable names as it is, and keeps a message's role and
// content only (spec 017 #18).
func TestCompileReadsTheSharedTable(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "prompts", "compile.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("testdata/prompts/compile.json is not here: not a checkout of the repository")
	} else if err != nil {
		t.Fatal(err)
	}
	type expected struct {
		Compiled json.RawMessage `json:"compiled"`
	}
	var table struct {
		Cases []struct {
			Name      string         `json:"name"`
			Text      string         `json:"text"`
			Messages  []Message      `json:"messages"`
			Variables map[string]any `json:"variables"`
			expected
			Go *expected `json:"go"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatal(err)
	}
	for _, c := range table.Cases {
		want := c.expected
		if c.Go != nil {
			want = *c.Go
		}
		if want.Compiled == nil {
			t.Errorf("%s: raises in Python and Node, and has no `go` field saying what Go gives", c.Name)
			continue
		}
		got := (&PromptVersion{Text: c.Text, Messages: c.Messages}).Compile(c.Variables)
		if c.Messages != nil {
			var messages []Message
			if err := json.Unmarshal(want.Compiled, &messages); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			if len(got.Messages) != len(messages) {
				t.Errorf("%s: compiled = %+v, want %+v", c.Name, got.Messages, messages)
				continue
			}
			for i := range messages {
				if got.Messages[i] != messages[i] {
					t.Errorf("%s: message %d = %+v, want %+v", c.Name, i, got.Messages[i], messages[i])
				}
			}
			continue
		}
		var text string
		if err := json.Unmarshal(want.Compiled, &text); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got.Text != text {
			t.Errorf("%s: compiled = %q, want %q", c.Name, got.Text, text)
		}
	}
}

func TestNoConfigurationIsErrConfig(t *testing.T) {
	fresh(t)
	if _, err := Prompt(context.Background(), "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("err = %v", err)
	}
}
