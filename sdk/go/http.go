package tracepad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the package's own, sent as its User-Agent and as the tracer's
// instrumentation version.
const Version = "0.1.0"

// ErrConfig is returned by Init, and by the REST calls made without it, when
// neither the options nor the environment name a host and a key (spec 017
// #9): misconfiguration discovered as a 401 in a log file an hour later is
// the bug report this error prevents. It is wrapped with what is missing;
// test for it with errors.Is.
var ErrConfig = errors.New("tracepad: not configured")

// ErrNoTrace is returned by Score when there is no span in the context and no
// trace id in the options (spec 017 #7): a score that silently went nowhere
// is the failure that API is worst at surfacing, and a programming error
// visible at the call site is the one exception to "the tracing path never
// fails".
var ErrNoTrace = errors.New("tracepad: no span in the context and no trace id")

// HTTPError is a non-2xx answer from the store, with the server's own message
// kept whole: the body is where the store names the offending field or item,
// and it is the whole value of returning an error rather than logging a
// status.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("tracepad: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// config is where the store is, who we are to it, and what to call this
// deploy. The options of Init fill it first and the environment fills what
// they did not (spec 017 #10); standard OpenTelemetry variables are the OTel
// SDK's to honour, and the package neither reads nor sets them.
type config struct {
	host, key, environment, release string
}

// resolve builds a config, failing with ErrConfig when the two required
// values are nowhere.
func resolve(host, key, environment, release string) (config, error) {
	c := config{
		host:        strings.TrimRight(pick(host, "TRACEPAD_HOST"), "/"),
		key:         pick(key, "TRACEPAD_API_KEY"),
		environment: pick(environment, "TRACEPAD_ENVIRONMENT"),
		release:     pick(release, "TRACEPAD_RELEASE"),
	}
	var missing []string
	if c.host == "" {
		missing = append(missing, "host")
	}
	if c.key == "" {
		missing = append(missing, "key")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("%w: no %s; pass tracepad.WithHost and tracepad.WithKey to Init "+
			"or set TRACEPAD_HOST and TRACEPAD_API_KEY", ErrConfig, strings.Join(missing, " and no "))
	}
	return c, nil
}

func pick(given, variable string) string {
	if given != "" {
		return strings.TrimSpace(given)
	}
	return strings.TrimSpace(os.Getenv(variable))
}

// current is the configuration the REST calls use. Init is the ordinary way
// to set it, and a script that only fetches a prompt should not have to call
// it: with no Init, the environment is read on the first call and the same
// ErrConfig comes back when it says nothing.
func current() (config, error) {
	d := def
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config == nil {
		c, err := resolve("", "", "", "")
		if err != nil {
			return config{}, err
		}
		d.config = &c
	}
	return *d.config, nil
}

// The REST half: net/http with the default client and a 10 s timeout, bearer
// auth, no async twin (spec 017 #6, spec 033 #8). Its callers are scripts and
// start-up code — a prompt at boot, the harness — where a blocking call is
// the honest shape. Scores are the exception and get a queue of their own.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// response is one JSON answer: the status, the decoded body (nil when empty,
// the text when it was not JSON) and the headers.
type response struct {
	status  int
	body    any
	headers http.Header
}

// request makes one JSON call. It returns *HTTPError for a non-2xx answer and
// the transport's own error for everything that never got one.
func request(ctx context.Context, c config, method, path string, body any, params url.Values) (response, error) {
	target := c.host + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return response{}, fmt.Errorf("tracepad: %s %s: encoding the body: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return response{}, fmt.Errorf("tracepad: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tracepad-go/"+Version)
	answer, err := httpClient.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("tracepad: %s %s: %w", method, path, err)
	}
	defer answer.Body.Close()
	raw, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, fmt.Errorf("tracepad: %s %s: reading the answer: %w", method, path, err)
	}
	if answer.StatusCode < 200 || answer.StatusCode > 299 {
		return response{}, &HTTPError{Status: answer.StatusCode, Body: string(raw)}
	}
	return response{status: answer.StatusCode, body: decode(raw), headers: answer.Header}, nil
}

func decode(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

// object is the answer as the JSON object it is, or an empty one: the read
// side of the API is the server's JSON decoded into map[string]any and
// nothing is modelled over it (spec 018 #8).
func object(r response) map[string]any {
	if m, ok := r.body.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// maxAge is how long the server said an answer may be trusted. A header that
// says nothing is zero, not a default of our own: the store sends
// `Cache-Control: max-age=60` on every prompt (`docs/prompts.md`), and
// inventing a window for a server that did not ask for one would cache
// against its wishes.
func maxAge(headers http.Header) time.Duration {
	for _, directive := range strings.Split(headers.Get("Cache-Control"), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
		if strings.EqualFold(name, "max-age") {
			seconds, err := strconv.Atoi(value)
			if err != nil || seconds < 0 {
				return 0
			}
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

// isClientError tells a 4xx — about the request: a moved label, a deleted
// prompt, a wrong key — from the server being away.
func isClientError(err error) bool {
	var h *HTTPError
	return errors.As(err, &h) && h.Status >= 400 && h.Status < 500
}
