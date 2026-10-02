package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/rawid"
	"github.com/tracepad/tracepad/internal/store"
)

// Fuzz targets for what the server reads off the network. The mapping
// package's targets cover the OTLP decoders themselves; these cover the
// handlers around them and the small parsers every listing leans on. See
// internal/mapping/fuzz_test.go for how to run them.
//
// The invariant of every handler target is the one a client can see: whatever
// the bytes, the answer is a refusal that says why (4xx) or an acceptance —
// never a 5xx, which the handlers reserve for a database that failed, and
// never a panic, which is a dropped connection.

const maxFuzzInput = 64 << 10

// fuzzHarness is one server for the whole fuzzing process: building a store
// per input would be the whole budget. It is seeded with the synthetic OTLP
// corpus, so a listing has rows to filter and a trace to open.
func fuzzHarness(f *testing.F) *harness {
	f.Helper()
	h := newHarness(f, nil, store.WriterOptions{})
	// newHarness captures the log to replay on failure; across a fuzzing
	// run that is a buffer that never stops growing.
	slog.SetDefault(slog.New(slog.DiscardHandler))
	for _, fixture := range otlptest.Fixtures() {
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			f.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			f.Fatalf("seeding %s: %d %s", fixture.Name, rec.Code, rec.Body)
		}
	}
	return h
}

// answered is the shared assertion: no 5xx, and an answer that is JSON when it
// says it is.
func answered(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code >= 500 {
		t.Fatalf("%s: status %d: %s", what, rec.Code, rec.Body)
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") && !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("%s: a JSON answer that is not JSON: %q", what, rec.Body)
	}
}

// FuzzIngest posts bytes to the ingest routes in every encoding the endpoint
// names and a few it does not, compressed or claiming to be.
func FuzzIngest(f *testing.F) {
	h := fuzzHarness(f)
	for _, fixture := range otlptest.Fixtures()[:6] {
		body, _ := mapping.EncodeExportRequest(fixture.ResourceSpans)
		f.Add(body, uint8(0))
		f.Add(body, uint8(1))
		jsonBody, _ := mapping.EncodeExportRequestJSON(fixture.ResourceSpans)
		f.Add(jsonBody, uint8(2))
		f.Add(jsonBody, uint8(3))
	}
	f.Add([]byte(nil), uint8(0))
	f.Add([]byte(`{"resourceSpans":[]}`), uint8(2))
	f.Add([]byte("not gzip"), uint8(4))
	f.Add([]byte{0x0a, 0x03, 0xff, 0xff, 0xff}, uint8(0))

	paths := []string{"/v1/traces", "/api/public/otel/v1/traces"}
	types := []string{"application/x-protobuf", "application/protobuf", "application/json", "text/plain", "", "application/json; charset=utf-8", "application/json;;"}
	f.Fuzz(func(t *testing.T, body []byte, mode uint8) {
		if len(body) > maxFuzzInput {
			t.Skip()
		}
		sent := body
		gzipped := mode&4 != 0
		if gzipped && mode&8 == 0 {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			w.Write(body)
			w.Close()
			sent = buf.Bytes()
		}
		req := httptest.NewRequest("POST", paths[int(mode)&1], bytes.NewReader(sent))
		req.Header.Set("Content-Type", types[int(mode>>4)%len(types)])
		req.Header.Set("Authorization", "Bearer "+testSecret)
		if gzipped {
			// With bit 8 set the bytes are not gzip at all.
			req.Header.Set("Content-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		answered(t, "ingest", rec)

		// An acceptance answers in the encoding it was sent in, and one
		// the exporter can read.
		if rec.Code == http.StatusOK {
			ct := req.Header.Get("Content-Type")
			if strings.HasPrefix(ct, "application/json") {
				if !json.Valid(rec.Body.Bytes()) {
					t.Fatalf("a JSON export was answered with %q", rec.Body)
				}
			} else if rec.Body.Len() > 0 {
				rejected, _ := mapping.DecodeExportResponse(rec.Body.Bytes())
				_ = rejected
			}
		}
	})
}

// FuzzWrite sends a body to each JSON write route of the API: the strict
// decoder, the validators behind it, and the store under them.
func FuzzWrite(f *testing.F) {
	h := fuzzHarness(f)
	// What a path parameter names has to exist for the body behind it to be
	// reached.
	for _, setup := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/datasets/d", `{}`},
		{"POST", "/api/v1/datasets/d/items", `{"input":{"q":1}}`},
		{"PUT", "/api/v1/score-configs/quality", `{"data_type":"numeric","min":0,"max":1,"direction":"higher"}`},
		{"PUT", "/api/v1/queues/q", `{"score_configs":["quality"]}`},
		{"POST", "/api/v1/prompts/p/versions", `{"type":"text","prompt":"hello {{name}}"}`},
	} {
		req := httptest.NewRequest(setup.method, setup.path, strings.NewReader(setup.body))
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		if rec.Code >= 300 {
			f.Fatalf("setup %s %s: %d %s", setup.method, setup.path, rec.Code, rec.Body)
		}
	}

	routes := []struct{ method, path string }{
		{"POST", "/api/v1/scores"},
		{"POST", "/api/v1/prompts/p/versions"},
		{"PUT", "/api/v1/prompts/p/labels/production"},
		{"PUT", "/api/v1/datasets/d"},
		{"POST", "/api/v1/datasets/d/items"},
		{"POST", "/api/v1/datasets/d/runs"},
		{"PUT", "/api/v1/score-configs/quality"},
		{"PUT", "/api/v1/queues/q"},
		{"POST", "/api/v1/queues/q/items"},
		{"POST", "/api/v1/queues/q/items/from-traces"},
		{"POST", "/api/public/media"},
	}
	trace := mapping.Map(otlptest.Fixtures()[0].ResourceSpans).Traces[0].ID
	bodies := []string{
		`{}`, `[]`, `null`, `[null]`, `"x"`, `1`, `{"unknown":1}`, `{"name":"a","value":1,"trace_id":"` + trace + `"}`,
		`[{"name":"a","value":1e400,"trace_id":"` + trace + `"}]`,
		`{"name":"a","string_value":"x","data_type":"categorical","trace_id":"` + trace + `"}`,
		`{"prompt":"x","labels":["production"],"config":{"a":1}}`, `{"prompt":[{"role":"user","content":"x"}]}`,
		`{"version":1}`, `{"description":"d","metadata":{"a":[1,2]}}`,
		`{"input":{"a":1},"expected_output":"x","metadata":null}`, `[{"id":"` + strings.Repeat("a", 32) + `","input":1}]`,
		`{"name":"r","metadata":{}}`, `{"data_type":"categorical","categories":["a","b"]}`,
		`{"data_type":"numeric","min":1,"max":0}`, `{"score_configs":[],"description":"x"}`,
		`{"trace_id":"` + trace + `"}`, `[{"trace_id":"` + trace + `","observation_id":"0011223344556677"}]`,
		`{"limit":5}`, `{"filter":{"name":"a"}}`, `{"media_type":"image/png","content_length":10,"sha256":"` + strings.Repeat("0", 64) + `"}`,
		`{"contentType":"image/png","contentLength":10,"sha256":"` + strings.Repeat("0", 43) + `=","traceId":"` + trace + `","field":"input"}`,
	}
	for i := range routes {
		for _, body := range bodies {
			f.Add(uint8(i), []byte(body))
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		if len(body) > maxFuzzInput {
			t.Skip()
		}
		route := routes[int(which)%len(routes)]
		req := httptest.NewRequest(route.method, route.path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		answered(t, route.method+" "+route.path, rec)
	})
}

// FuzzRead sends arbitrary query strings to every listing and read route: the
// filters, the time bounds, the cursors, the search. A refusal is a 400 that
// names the parameter; what a filter cannot parse is never a 500.
func FuzzRead(f *testing.F) {
	h := fuzzHarness(f)
	traceResult := mapping.Map(otlptest.Fixtures()[0].ResourceSpans)
	trace, observation := traceResult.Traces[0].ID, traceResult.Observations[0].ID
	routes := []string{
		"/api/v1/traces", "/api/v1/traces/last", "/api/v1/traces/" + trace,
		"/api/v1/observations/" + observation + "/io", "/api/v1/raw", "/api/v1/raw/1",
		"/api/v1/sessions", "/api/v1/sessions/session-77", "/api/v1/stats", "/api/v1/stats/scores",
		"/api/v1/facets", "/api/v1/users", "/api/v1/users/user-4821", "/api/v1/scores",
		"/api/v1/prompts", "/api/v1/prompts/p", "/api/v1/prompts/p/versions", "/api/v1/prompts/p/diff",
		"/api/v1/datasets", "/api/v1/datasets/d", "/api/v1/datasets/d/items", "/api/v1/datasets/d/runs",
		"/api/v1/runs", "/api/v1/score-configs", "/api/v1/queues", "/api/v1/queues/q",
		"/api/v1/queues/q/items", "/api/v1/system",
	}
	queries := []string{
		"", "limit=5", "limit=0", "limit=-1", "limit=abc", "limit=1000000", "cursor=!!", "cursor=" + encodeCursor("1", trace),
		"direction=prev", "direction=prev&cursor=" + encodeCursor("0", "x"), "count=true",
		"from=2026-08-26T10:00:00Z&to=2026-08-27T10:00:00Z", "from=0000-01-01T00:00:00Z", "to=9999-12-31T23:59:59Z", "from=yesterday",
		"q=refund", "q=%22refund+order%22", "q=err*", "q=(a+OR+b)", "q=" + strings.Repeat("a", 600), "q=%00",
		"tag=a&tag=b", "tag=", "environment=production,staging", "environment=a,,b", "release=1.2.3", "name=x", "type=generation",
		"prompt=p@3", "prompt=@", "prompt=p@-1", "user_id=u", "session_id=s", "status=error", "level=ERROR",
		"min_cost=0.5", "max_cost=1e400", "min_latency_ms=NaN", "min_tokens=-1", "sort=cost", "sort=oldest", "order=asc",
		"group_by=model", "group_by=release", "group_by=", "bucket=hour", "bucket=week", "window=7d", "range=24h",
		"fields=id,name", "fields=,", "fields=nope", "version=1", "label=production", "a=1&b=2", "%", "%zz=1", "q=%ff",
		"dataset=d", "run_id=" + strings.Repeat("0", 32), "item_id=x", "compare=1", "prefix=a", "sort=tokens&cursor=" + encodeCursor("5", "u"),
		"sort=cost&cursor=" + encodeCursor("", "u"), "from_version=1&to_version=2", "name=quality", "data_type=numeric",
	}
	for i := range routes {
		for _, query := range queries {
			f.Add(uint8(i), query)
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, rawQuery string) {
		if len(rawQuery) > 8192 {
			t.Skip()
		}
		path := routes[int(which)%len(routes)]
		req := httptest.NewRequest("GET", path, nil)
		req.URL.RawQuery = rawQuery
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		answered(t, "GET "+path+"?"+rawQuery, rec)
	})
}

// FuzzParsers is every small parser of an HTTP value that has no handler of its
// own to be reached through: each answers or refuses, and what it answers
// survives being written out and read back.
func FuzzParsers(f *testing.F) {
	for _, seed := range []string{
		"", "Bearer abc", "bearer  abc ", "Basic cHVibGljOnNlY3JldA==", "Basic Og==", "Basic !!", "Digest x",
		"application/json", "application/json; charset=utf-8", "application/x-protobuf;", "text/plain;;", `a/b; c="d`,
		"2026-10-02T10:00:00Z", "2026-10-02T10:00:00.123456789+03:00", "1678-01-01T00:00:00Z", "2262-04-11T23:47:16.854775807Z",
		"1.2.3.4", "1.2.3.4:80", "[::1]:80", "[::1]", "::1", "unknown", "_hidden", "fe80::1%eth0", "::ffff:1.2.3.4", "64:ff9b::1.2.3.4",
		"p", "p@3", "@acme/support", "name@latest", "svc@-1", "name@+7", "p@99999999999999999999", "@", "a@b@7",
		"id,name", "id,,name", " id ", "id,id",
		encodeCursor("1", "x"), encodeCursor("x", "y"), encodeCursor("1"), encodeCursor("1", "2", "3"), "!!!", "AA", encodeCursor("-9223372036854775808", "z"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > maxFuzzInput {
			t.Skip()
		}

		if secret, ok := credential(text); ok && secret == "" {
			t.Fatalf("credential(%q) answered an empty secret", text)
		}
		if mediaType, ok := acceptableContentType(text); ok {
			switch mediaType {
			case contentTypeProtobuf, contentTypeProtobufAlt, contentTypeJSON:
			default:
				t.Fatalf("acceptableContentType(%q) accepted %q", text, mediaType)
			}
		}
		if addr, ok := parseForwardedAddress(text); ok && !addr.IsValid() {
			t.Fatalf("parseForwardedAddress(%q) answered an invalid address", text)
		}
		if filter, err := parsePrompt(text); err == nil {
			if filter.Name == "" {
				t.Fatalf("parsePrompt(%q) answered an empty name", text)
			}
			if filter.Version != nil && text != filter.Name+"@"+strconv.FormatInt(*filter.Version, 10) &&
				!strings.HasPrefix(text, filter.Name+"@") {
				t.Fatalf("parsePrompt(%q) = %q@%d", text, filter.Name, *filter.Version)
			}
		}
		if selected, err := parseSelection(text, []string{"id", "name", "input"}); err == nil {
			for _, field := range selected {
				if field != "id" && field != "name" && field != "input" {
					t.Fatalf("parseSelection(%q) selected the unknown %q", text, field)
				}
			}
		}
		_ = validName("name", text)

		if ns, err := parseTime("from", text); err == nil {
			again, err := parseTime("from", formatTime(ns))
			if err != nil || again != ns {
				t.Fatalf("parseTime(%q) = %d, which does not survive formatTime: %d, %v", text, ns, again, err)
			}
		}

		// Cursors: what decodes re-encodes to something that decodes to the
		// same keyset.
		if c, err := decodeTraceCursor(text); err == nil {
			again, err := decodeTraceCursor(encodeCursor(strconv.FormatInt(c.Timestamp, 10), c.ID))
			if err != nil || *again != *c {
				t.Fatalf("trace cursor %q does not round-trip: %v %v", text, again, err)
			}
		}
		if c, err := decodeSessionCursor(text); err == nil {
			again, err := decodeSessionCursor(encodeCursor(strconv.FormatInt(c.LastSeen, 10), c.ID))
			if err != nil || *again != *c {
				t.Fatalf("session cursor %q does not round-trip: %v %v", text, again, err)
			}
		}
		if c, err := decodeRawCursor(text); err == nil {
			again, err := decodeRawCursor(rawid.Cursor(c.ReceivedAt, c.Number))
			if err != nil || *again != *c {
				t.Fatalf("raw cursor %q does not round-trip: %v %v", text, again, err)
			}
		}
		if c, err := decodeRunCursor(text); err == nil {
			again, err := decodeRunCursor(encodeCursor(strconv.FormatInt(c.CreatedAt, 10), c.ID))
			if err != nil || *again != *c {
				t.Fatalf("run cursor %q does not round-trip: %v %v", text, again, err)
			}
		}
		for _, sortBy := range store.UserSorts {
			if c, err := decodeUserCursor(text, sortBy); err == nil {
				again, err := decodeUserCursor(encodeCursor(c.Key, c.UserID), sortBy)
				if err != nil || *again != *c {
					t.Fatalf("user cursor %q (%s) does not round-trip: %v %v", text, sortBy, again, err)
				}
			}
		}
	})
}

// FuzzBatchDecoders is the body decoder under every array write: one value or
// an array, strict, counted before it is decoded. It answers the values or a
// refusal with a message; an array over the limit is the typed refusal and not
// a decode of everything.
func FuzzBatchDecoders(f *testing.F) {
	for _, seed := range []string{
		``, ` `, `{}`, `[]`, `[{}]`, `[null]`, `null`, `{"trace_id":"x"}`, `{"trace_id":"x"}]`, `[{"trace_id":"x"}`,
		`[{"trace_id":"x"},]`, `{"trace_id":"x"} {"trace_id":"y"}`, `{"unknown":1}`, `{"trace_id":1}`,
		`[` + strings.Repeat(`{},`, 5000) + `{}]`, `[` + strings.Repeat(`{},`, 5000), `[` + strings.Repeat(`[`, 500),
		"\xef\xbb\xbf{}", `{"trace_id":"\ud800"}`, `{"trace_id":"a","trace_id":"b"}`,
	} {
		f.Add([]byte(seed), uint8(3))
	}
	f.Fuzz(func(t *testing.T, body []byte, limit uint8) {
		if len(body) > maxFuzzInput {
			t.Skip()
		}
		n := int(limit)%50 + 1
		values, err := decodeBatch[itemTargetRequest](body, "target", n)
		if err != nil {
			if err.Error() == "" {
				t.Fatalf("a refusal with no message for %q", body)
			}
			return
		}
		if len(values) > n && !bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
			t.Fatalf("decodeBatch took %d values over the limit of %d", len(values), n)
		}
		for i, value := range values {
			if value == nil {
				t.Fatalf("decodeBatch answered a nil value at %d", i)
			}
		}
		// Whatever it decoded is also one JSON value, which the standard
		// library agrees with.
		if !json.Valid(body) {
			t.Fatalf("decodeBatch took %q, which is not one JSON value", body)
		}
		var one any
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&one); err != nil {
			t.Fatalf("decodeBatch took %q, which json cannot read: %v", body, err)
		}
		if _, err := dec.Token(); err != io.EOF {
			t.Fatalf("decodeBatch took %q, which has something after the value", body)
		}
	})
}

// FuzzMediaUpload is the presigned PUT of the Langfuse media channel, the one
// route that takes bytes from a client that sends no credential: a token this
// server signed, naming what the PUT may store, and a body that has to be what
// the token says — its hash, its length, its type. Every disagreement is a
// refusal; none is a 5xx, and a token that is not ours is a 403 having read
// nothing.
func FuzzMediaUpload(f *testing.F) {
	h := fuzzHarness(f)
	trace := mapping.Map(otlptest.Fixtures()[0].ResourceSpans).Traces[0].ID
	sign := func(t *testing.T, grant uploadGrant) string {
		t.Helper()
		token, err := h.server.signUpload(grant)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	f.Add([]byte("a picture"), uint8(0), "image/png", "")
	f.Add([]byte("a picture"), uint8(1), "image/png", "")
	f.Add([]byte("a picture"), uint8(2), "image/png", "")
	f.Add([]byte("a picture"), uint8(3), "text/plain", "")
	f.Add([]byte(nil), uint8(0), "", "")
	f.Add([]byte("a picture"), uint8(0), "image/png", "x.y")
	f.Add([]byte("a picture"), uint8(0), "image/png", "e30.e30")
	f.Fuzz(func(t *testing.T, body []byte, mode uint8, mime, token string) {
		if len(body) > maxFuzzInput || len(token) > 4096 {
			t.Skip()
		}
		sum := sha256.Sum256(body)
		sha := hex.EncodeToString(sum[:])
		grant := uploadGrant{
			Project: h.project.ID, Trace: trace, SHA256: sha, MimeType: mime,
			Length: int64(len(body)), Expires: time.Now().Add(time.Hour).Unix(), Key: testPublic,
		}
		switch mode % 6 {
		case 1:
			grant.Length++ // the body is shorter than declared
		case 2:
			grant.Length-- // and longer
		case 3:
			grant.SHA256 = strings.Repeat("0", 64) // another hash than the bytes'
		case 4:
			grant.Expires = time.Now().Add(-time.Hour).Unix()
		case 5:
			grant.Trace = "nope"
		}
		if token == "" {
			token = sign(t, grant)
		}
		req := httptest.NewRequest("PUT", "/api/public/media/"+store.MediaIDFor(grant.SHA256)+"/upload", bytes.NewReader(body))
		req.URL.RawQuery = "token=" + url.QueryEscape(token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		answered(t, "PUT media upload", rec)

		// Whatever this server signed, it opens; whatever it did not, it
		// refuses without panicking.
		if opened, err := h.server.verifyUpload(token); err == nil {
			opened.refusal("anything", time.Now())
		}
	})
}

// FuzzClientAddress is who a request is from (spec 046 #4): the peer, or the
// address a chain of trusted proxies vouches for in `X-Forwarded-For`. The
// header is the client's, so the answer must never be one the client can write
// when the peer is not a proxy of ours, and must always be in the one spelling
// the rate limiter keys on.
func FuzzClientAddress(f *testing.F) {
	h := fuzzHarness(f)
	h.server.trusted = newTrustedProxies("10.0.0.0/8,fd00::/8,192.168.1.1/32")
	f.Add("203.0.113.9:1234", "198.51.100.1", "")
	f.Add("10.0.0.1:80", "198.51.100.1, 10.0.0.2", "")
	f.Add("10.0.0.1:80", "198.51.100.1", "203.0.113.7")
	f.Add("[fd00::1]:80", "2001:db8::1", "")
	f.Add("10.0.0.1:80", "unknown", "")
	f.Add("10.0.0.1:80", ", ,"+strings.Repeat("10.0.0.3,", 40), "")
	f.Add("[::ffff:10.0.0.1]:80", "::ffff:198.51.100.1", "fe80::1%eth0")
	f.Add("", "", "")
	f.Add("garbage", "[::1]:99", "1.2.3.4:5")
	f.Fuzz(func(t *testing.T, remote, first, second string) {
		if len(remote)+len(first)+len(second) > maxFuzzInput {
			t.Skip()
		}
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr = remote
		// httptest builds a request that Header.Add accepts any value of.
		req.Header["X-Forwarded-For"] = nil
		if first != "" {
			req.Header["X-Forwarded-For"] = append(req.Header["X-Forwarded-For"], first)
		}
		if second != "" {
			req.Header["X-Forwarded-For"] = append(req.Header["X-Forwarded-For"], second)
		}
		got := h.server.clientAddress(withClientMemo(req))

		if got.IsValid() && (got.Zone() != "" || got.Is4In6()) {
			t.Fatalf("clientAddress(%q, %q, %q) = %v, which is not in the one spelling a source is keyed on", remote, first, second, got)
		}
		peer := peerAddress(remote)
		if !h.server.trusted.trusts(peer) && got != peer {
			t.Fatalf("a peer that is no proxy of ours (%q) was replaced by %v from a header it wrote", remote, got)
		}
		if prefix := sourceOf(got); got.IsValid() && !prefix.IsValid() {
			t.Fatalf("sourceOf(%v) is no prefix", got)
		}
	})
}
