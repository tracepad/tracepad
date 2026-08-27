package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The client is exercised end to end by the CLI and MCP suites, against a real
// server. What is left here is what those cannot reach: a URL that is not one,
// and a failure that did not come from a tracepad.

func TestNewRefusesSomethingThatIsNotAURL(t *testing.T) {
	for _, target := range []string{"localhost:4318", "not a url", "/api/v1"} {
		if _, err := New(target, "tp-sk-test"); err == nil {
			t.Errorf("New(%q) was accepted; a missing scheme is a mistake worth naming", target)
		}
	}
	c, err := New("http://localhost:4318/", "tp-sk-test")
	if err != nil {
		t.Fatal(err)
	}
	// The trailing slash is dropped so paths do not double up on it.
	if c.BaseURL != "http://localhost:4318" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	c, err = New("", "tp-sk-test")
	if err != nil || c.BaseURL != DefaultURL {
		t.Errorf("New(\"\") = %v, %v, want the default server", c, err)
	}
}

// TestErrorFromSomethingElse: pointed at a service that is not a tracepad, the
// message has to say what actually came back rather than an empty string —
// that is the difference between "the port is wrong" and "no idea".
func TestErrorFromSomethingElse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer server.Close()

	c, err := New(server.URL, "tp-sk-test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(t.Context(), "/api/v1/traces", nil)
	if err == nil {
		t.Fatal("a 502 was reported as success")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a typed API error", err)
	}
	if apiErr.Status != http.StatusBadGateway || !strings.Contains(apiErr.Message, "Bad Gateway") {
		t.Errorf("err = %+v, want it to carry what the other end said", apiErr)
	}
}

// TestVersionIsReported: the header the CLI watches for skew reaches its
// callback on every response, including a failing one.
func TestVersionIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, "9.9.9")
		if r.URL.RawQuery != "environment=production" {
			t.Errorf("query = %q, want the values as given", r.URL.RawQuery)
		}
		w.Write([]byte(`{"traces":[]}`))
	}))
	defer server.Close()

	c, err := New(server.URL, "tp-sk-test")
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	c.OnVersion = func(version string) { seen = version }
	body, err := c.Get(context.Background(), "/api/v1/traces",
		url.Values{"environment": []string{"production"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"traces":[]}` {
		t.Errorf("body = %s, want it verbatim", body)
	}
	if seen != "9.9.9" {
		t.Errorf("version = %q, want the server's", seen)
	}
}
