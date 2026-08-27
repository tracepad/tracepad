package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// Scores end to end (spec 003, Testing #1 and #2): the validation matrix of #5
// as a table, and real requests through the real handler into a real database.

const scoreTraceID = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"

// The validation matrix of #5: which data types accept which value field, what
// an omitted type is inferred to be, and which combinations are refused.
func TestScoreValidationMatrix(t *testing.T) {
	number := func(v float64) *float64 { return &v }
	str := func(v string) *string { return &v }

	cases := []struct {
		name     string
		request  scoreRequest
		wantType string
		wantErr  string
	}{
		{
			name:     "value alone is numeric",
			request:  scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID), Value: number(0.9)},
			wantType: store.ScoreNumeric,
		},
		{
			name:     "string_value alone is text",
			request:  scoreRequest{Name: "verdict", TraceID: str(scoreTraceID), StringValue: str("solid")},
			wantType: store.ScoreText,
		},
		{
			name:     "boolean carries 0 or 1",
			request:  scoreRequest{Name: "passed", TraceID: str(scoreTraceID), DataType: store.ScoreBoolean, Value: number(1)},
			wantType: store.ScoreBoolean,
		},
		{
			name:     "categorical carries string_value",
			request:  scoreRequest{Name: "tone", TraceID: str(scoreTraceID), DataType: store.ScoreCategorical, StringValue: str("friendly")},
			wantType: store.ScoreCategorical,
		},
		{
			name:     "a session is a target on its own",
			request:  scoreRequest{Name: "csat", SessionID: str("session-77"), Value: number(5)},
			wantType: store.ScoreNumeric,
		},
		{
			name:    "boolean refuses a value that is not 0 or 1",
			request: scoreRequest{Name: "passed", TraceID: str(scoreTraceID), DataType: store.ScoreBoolean, Value: number(0.5)},
			wantErr: "must be 0 or 1",
		},
		{
			name:    "numeric refuses a string_value",
			request: scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID), DataType: store.ScoreNumeric, Value: number(1), StringValue: str("nope")},
			wantErr: "not both",
		},
		{
			name:    "text refuses a value",
			request: scoreRequest{Name: "verdict", TraceID: str(scoreTraceID), DataType: store.ScoreText, Value: number(1)},
			wantErr: `needs a "string_value"`,
		},
		{
			name:    "categorical without a string_value",
			request: scoreRequest{Name: "tone", TraceID: str(scoreTraceID), DataType: store.ScoreCategorical, Value: number(1)},
			wantErr: `needs a "string_value"`,
		},
		{
			name:    "numeric without a value",
			request: scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID), DataType: store.ScoreNumeric, StringValue: str("x")},
			wantErr: `needs a "value"`,
		},
		{
			name:    "an unknown data type",
			request: scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID), DataType: "vibes", Value: number(1)},
			wantErr: `"data_type" must be one of`,
		},
		{
			name:    "no value at all",
			request: scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID)},
			wantErr: `needs a "value" or a "string_value"`,
		},
		{
			name:    "no name",
			request: scoreRequest{TraceID: str(scoreTraceID), Value: number(1)},
			wantErr: `"name" is required`,
		},
		{
			name:    "a name over 200 characters",
			request: scoreRequest{Name: strings.Repeat("n", 201), TraceID: str(scoreTraceID), Value: number(1)},
			wantErr: "at most 200 characters",
		},
		{
			name:    "no target",
			request: scoreRequest{Name: "helpfulness", Value: number(1)},
			wantErr: `needs a "trace_id" or a "session_id"`,
		},
		{
			name:    "an observation without its trace",
			request: scoreRequest{Name: "helpfulness", SessionID: str("session-77"), ObservationID: str("0011223344556677"), Value: number(1)},
			wantErr: `"observation_id" also needs`,
		},
		{
			name:    "an empty target id",
			request: scoreRequest{Name: "helpfulness", TraceID: str(""), SessionID: str("session-77"), Value: number(1)},
			wantErr: "must not be empty",
		},
		{
			name:    "an id that is not 32 hex characters",
			request: scoreRequest{ID: str("run-42"), Name: "helpfulness", TraceID: str(scoreTraceID), Value: number(1)},
			wantErr: "32 lower-case hex characters",
		},
		{
			name:    "a timestamp that is not RFC 3339",
			request: scoreRequest{Name: "helpfulness", TraceID: str(scoreTraceID), Value: number(1), Timestamp: str("yesterday")},
			wantErr: "RFC 3339",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			score, err := testCase.request.validate(1000)
			if testCase.wantErr != "" {
				if err == nil {
					t.Fatalf("validate() = %+v, want an error mentioning %q", score, testCase.wantErr)
				}
				if !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v", err)
			}
			if score.DataType != testCase.wantType {
				t.Errorf("data_type = %q, want %q", score.DataType, testCase.wantType)
			}
			if !hexID.MatchString(score.ID) {
				t.Errorf("generated id = %q, want 32 lower-case hex characters", score.ID)
			}
		})
	}
}

// An RFC 3339 timestamp survives the round trip through nanosecond storage
// (#16), and a client that sends none gets receive time.
func TestScoreTimestampRoundTrip(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	const sent = "2026-08-27T10:00:00Z"
	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9, "timestamp": sent,
	})
	expectStatus(t, rec, http.StatusCreated)
	ids := decodeJSON[scoreIDsResponse](t, rec).IDs

	score := decodeJSON[scoreResponse](t, h.get(t, "/api/v1/scores/"+ids[0]))
	if score.Timestamp != sent {
		t.Errorf("timestamp = %q, want %q", score.Timestamp, sent)
	}
	if score.CreatedAt == "" || score.CreatedAt == sent {
		t.Errorf("created_at = %q, want the receive time", score.CreatedAt)
	}
}

// A single score posts, stores and reads back whole.
func TestScoreSingleRoundTrip(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id":       scoreTraceID,
		"observation_id": "0011223344556677",
		"name":           "helpfulness",
		"value":          0.9,
		"comment":        "the judge liked it",
		"metadata":       map[string]any{"judge_model": "claude"},
	})
	expectStatus(t, rec, http.StatusCreated)
	ids := decodeJSON[scoreIDsResponse](t, rec).IDs
	if len(ids) != 1 || !hexID.MatchString(ids[0]) {
		t.Fatalf("ids = %v, want one generated 32-hex id", ids)
	}

	got := h.get(t, "/api/v1/scores/"+ids[0])
	expectStatus(t, got, http.StatusOK)
	score := decodeJSON[scoreResponse](t, got)
	if score.TraceID != scoreTraceID || score.ObservationID != "0011223344556677" {
		t.Errorf("targets = %+v", score)
	}
	if score.DataType != store.ScoreNumeric || score.Value == nil || *score.Value != 0.9 {
		t.Errorf("value = %+v", score)
	}
	if score.Comment != "the judge liked it" {
		t.Errorf("comment = %q", score.Comment)
	}
	if string(score.Metadata) != `{"judge_model":"claude"}` {
		t.Errorf("metadata = %s", score.Metadata)
	}
	if score.StringValue != nil {
		t.Errorf("string_value = %v, want it absent", *score.StringValue)
	}
}

// A re-POST with the same id replaces the row: a correction is a re-POST, not
// a delete and an insert (#3).
func TestScoreUpsertsByID(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const id = "aaaabbbbccccddddeeeeffff00001111"

	for _, value := range []float64{0.4, 0.8} {
		rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
			"id": id, "trace_id": scoreTraceID, "name": "helpfulness", "value": value,
		})
		expectStatus(t, rec, http.StatusCreated)
		if got := decodeJSON[scoreIDsResponse](t, rec).IDs[0]; got != id {
			t.Fatalf("id = %q, want the client's own %q", got, id)
		}
	}

	list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores"))
	if len(list.Scores) != 1 {
		t.Fatalf("scores = %d, want the second POST to have replaced the first", len(list.Scores))
	}
	if *list.Scores[0].Value != 0.8 {
		t.Errorf("value = %v, want the correction", *list.Scores[0].Value)
	}
}

// An array POST is all-or-nothing: one transaction, one 400 naming the first
// invalid item, and nothing written (#7).
func TestScoreArrayIsAllOrNothing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9},
		{"trace_id": scoreTraceID, "name": "tone", "data_type": "boolean", "value": 7},
		{"trace_id": scoreTraceID, "name": "verdict", "string_value": "good"},
	})
	expectError(t, rec, http.StatusBadRequest, "score at index 1")

	list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores"))
	if len(list.Scores) != 0 {
		t.Fatalf("scores = %d, want none: a refused batch writes nothing", len(list.Scores))
	}

	rec = h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9},
		{"trace_id": scoreTraceID, "name": "verdict", "string_value": "good"},
	})
	expectStatus(t, rec, http.StatusCreated)
	if ids := decodeJSON[scoreIDsResponse](t, rec).IDs; len(ids) != 2 {
		t.Fatalf("ids = %v, want one per score in input order", ids)
	}
}

// Nothing to write is a client bug, not a no-op (edge cases).
func TestScoreEmptyArrayIsRefused(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectError(t, h.call(t, "POST", "/api/v1/scores", []byte(`[]`)), http.StatusBadRequest, "no scores")
}

// An unattended agent that typos a field must be told, not silently robbed of
// the value (#17).
func TestScoreRejectsUnknownField(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.call(t, "POST", "/api/v1/scores",
		[]byte(`{"trace_id":"`+scoreTraceID+`","name":"helpfulness","value":0.9,"commet":"typo"}`))
	expectError(t, rec, http.StatusBadRequest, `unknown field "commet"`)

	rec = h.call(t, "POST", "/api/v1/scores", []byte(`{"name":`))
	expectError(t, rec, http.StatusBadRequest, "malformed JSON")

	rec = h.get(t, "/api/v1/scores?trace=abc")
	expectError(t, rec, http.StatusBadRequest, `unknown query parameter "trace"`)
}

// Filters narrow a listing, and the cursor walks it without gaps or repeats
// under a page size of one (#18).
func TestScoreListFiltersAndPagination(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	posted := []map[string]any{
		{"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.1, "timestamp": "2026-08-20T10:00:00Z"},
		{"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.2, "timestamp": "2026-08-21T10:00:00Z"},
		{"trace_id": scoreTraceID, "name": "verdict", "string_value": "good", "timestamp": "2026-08-22T10:00:00Z"},
		{"session_id": "session-77", "name": "csat", "value": 5, "timestamp": "2026-08-23T10:00:00Z"},
	}
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", posted), http.StatusCreated)

	byTrace := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?trace_id="+scoreTraceID))
	if len(byTrace.Scores) != 3 {
		t.Errorf("trace filter = %d scores, want 3", len(byTrace.Scores))
	}
	// Newest first.
	if byTrace.Scores[0].Name != "verdict" {
		t.Errorf("first = %q, want the newest score", byTrace.Scores[0].Name)
	}
	if got := len(decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?name=helpfulness")).Scores); got != 2 {
		t.Errorf("name filter = %d scores, want 2", got)
	}
	if got := len(decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?data_type=text")).Scores); got != 1 {
		t.Errorf("data_type filter = %d scores, want 1", got)
	}
	if got := len(decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?session_id=session-77")).Scores); got != 1 {
		t.Errorf("session filter = %d scores, want 1", got)
	}
	// Half-open: `from` includes its instant, `to` excludes it.
	window := decodeJSON[scoreListResponse](t,
		h.get(t, "/api/v1/scores?from=2026-08-21T10:00:00Z&to=2026-08-23T10:00:00Z"))
	if len(window.Scores) != 2 {
		t.Errorf("time window = %d scores, want 2", len(window.Scores))
	}

	var walked []string
	path := "/api/v1/scores?limit=1"
	for range len(posted) + 1 {
		page := decodeJSON[scoreListResponse](t, h.get(t, path))
		if len(page.Scores) == 0 {
			break
		}
		for _, score := range page.Scores {
			walked = append(walked, score.ID)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/scores?limit=1&cursor=" + *page.NextCursor
	}
	if len(walked) != len(posted) {
		t.Fatalf("cursor walk visited %d scores, want %d", len(walked), len(posted))
	}
	seen := map[string]bool{}
	for _, id := range walked {
		if seen[id] {
			t.Fatalf("cursor walk repeated score %s", id)
		}
		seen[id] = true
	}
}

// A timestamp outside what Unix nanoseconds can represent is refused rather
// than wrapped: year 3000 would otherwise be stored as 1830 (#23).
func TestScoreRefusesUnrepresentableTimestamps(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9,
		"timestamp": "3000-01-01T00:00:00Z",
	})
	expectError(t, rec, http.StatusBadRequest, "must be between")
	expectError(t, h.get(t, "/api/v1/scores?from=1000-01-01T00:00:00Z"),
		http.StatusBadRequest, "must be between")
}

// The epoch is a legal instant, not the absence of a bound: `to` at the epoch
// asks for nothing before 1970 and must answer with nothing (#23).
func TestScoreTimeFilterTreatsTheEpochAsAValue(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9,
	}), http.StatusCreated)

	before := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?to=1970-01-01T00:00:00Z"))
	if len(before.Scores) != 0 {
		t.Errorf("scores before the epoch = %d, want none", len(before.Scores))
	}
	after := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?from=1970-01-01T00:00:00Z"))
	if len(after.Scores) != 1 {
		t.Errorf("scores since the epoch = %d, want the one written", len(after.Scores))
	}
}

// A batch that names one id twice would answer with two ids for one stored
// row, and the client would count what it wrote wrong (#23).
func TestScoreBatchRefusesDuplicateIDs(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const id = "aaaabbbbccccddddeeeeffff00001111"

	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"id": id, "trace_id": scoreTraceID, "name": "helpfulness", "value": 0.1},
		{"id": id, "trace_id": scoreTraceID, "name": "helpfulness", "value": 0.2},
	})
	expectError(t, rec, http.StatusBadRequest, "already used by the score at index 0")

	list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores"))
	if len(list.Scores) != 0 {
		t.Errorf("scores = %d, want none: a refused batch writes nothing", len(list.Scores))
	}
}

// Decision 21 holds on the write routes too, and a parameter sent without a
// value is a client whose template left a variable unset (#23).
func TestScoreWriteRejectsQueryParameters(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	body := []byte(`{"trace_id":"` + scoreTraceID + `","name":"helpfulness","value":0.9}`)
	expectError(t, h.call(t, "POST", "/api/v1/scores?trace_id="+scoreTraceID, body),
		http.StatusBadRequest, "unknown query parameter")
	expectError(t, h.get(t, "/api/v1/scores?name="),
		http.StatusBadRequest, "without a value")
}

// The list endpoint refuses a page size it will not honour rather than
// silently clamping it (#18).
func TestScoreListRejectsOutOfRangeLimit(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectError(t, h.get(t, "/api/v1/scores?limit=5000"), http.StatusBadRequest, "between 1 and 500")
	expectError(t, h.get(t, "/api/v1/scores?limit=0"), http.StatusBadRequest, "between 1 and 500")
	expectError(t, h.get(t, "/api/v1/scores?cursor=not-base64!"), http.StatusBadRequest, "invalid cursor")
	expectError(t, h.get(t, "/api/v1/scores?data_type=vibes"), http.StatusBadRequest, "data_type must be one of")
}

func TestScoreUnknownIDIs404(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectError(t, h.get(t, "/api/v1/scores/aaaabbbbccccddddeeeeffff00001111"), http.StatusNotFound, "not found")
}

// The credential story is the one ingest uses (#2).
func TestScoreRequiresCredentials(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	body := []byte(`{"trace_id":"` + scoreTraceID + `","name":"helpfulness","value":0.9}`)
	strip := func(r *http.Request) { r.Header.Del("Authorization") }
	expectError(t, h.call(t, "POST", "/api/v1/scores", body, strip), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.call(t, "GET", "/api/v1/scores", nil, strip), http.StatusUnauthorized, "unauthorized")

	basic := func(r *http.Request) { r.SetBasicAuth(testPublic, testSecret) }
	expectStatus(t, h.call(t, "POST", "/api/v1/scores", body, basic), http.StatusCreated)
}

// A saturated writer answers 429 here for the same reason it does on ingest:
// backpressure a client can act on beats a request that stalls (#9).
func TestScoreBackpressureReturns429(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.writer = stubWriter{err: store.ErrWriterBusy}

	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9,
	})
	expectStatus(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must tell the client when to come back")
	}
}

// A score whose trace never arrives is stored and listed all the same: evals
// run async, and refusing would force clients to poll (#4).
func TestScoreForAnUnknownTraceIsStored(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": "ffffffffffffffffffffffffffffffff", "name": "helpfulness", "value": 0.9,
	})
	expectStatus(t, rec, http.StatusCreated)
	list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?trace_id=ffffffffffffffffffffffffffffffff"))
	if len(list.Scores) != 1 {
		t.Fatalf("scores = %d, want the dangling score", len(list.Scores))
	}
}

// Metadata is stored inline and comes back as it went in (#6).
func TestScoreMetadataIsInlineJSON(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	metadata := map[string]any{"judge_model": "claude", "run": map[string]any{"id": 42.0}}

	rec := h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "helpfulness", "value": 0.9, "metadata": metadata,
	})
	expectStatus(t, rec, http.StatusCreated)

	id := decodeJSON[scoreIDsResponse](t, rec).IDs[0]
	score := decodeJSON[scoreResponse](t, h.get(t, "/api/v1/scores/"+id))
	var out map[string]any
	if err := json.Unmarshal(score.Metadata, &out); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if fmt.Sprint(out) != fmt.Sprint(metadata) {
		t.Errorf("metadata = %v, want %v", out, metadata)
	}
}
