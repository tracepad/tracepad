package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Scores (spec 003): POST one or many, list them filtered, fetch one by id.

// maxScoreNameLength bounds the name a score is filed under (API contract).
const maxScoreNameLength = 200

// scoreRequest is the wire shape of one score. A field is a pointer only
// where "absent" and "sent as empty" lead somewhere different: an absent id is
// generated while an empty one is refused, an absent target is not the same as
// an empty one, and which of `value`/`string_value` arrived decides the data
// type (#5). For the rest, empty and absent mean the same thing.
type scoreRequest struct {
	ID            *string         `json:"id"`
	TraceID       *string         `json:"trace_id"`
	ObservationID *string         `json:"observation_id"`
	SessionID     *string         `json:"session_id"`
	Name          string          `json:"name"`
	DataType      string          `json:"data_type"`
	Value         *float64        `json:"value"`
	StringValue   *string         `json:"string_value"`
	Comment       string          `json:"comment"`
	Metadata      json.RawMessage `json:"metadata"`
	Timestamp     *string         `json:"timestamp"`
}

// scoreIDsResponse answers a create in input order, single object included
// (API contract).
type scoreIDsResponse struct {
	IDs []string `json:"ids"`
}

type scoreListResponse struct {
	Scores     []scoreResponse `json:"scores"`
	NextCursor *string         `json:"next_cursor"`
}

type scoreResponse struct {
	ID            string          `json:"id"`
	TraceID       string          `json:"trace_id,omitempty"`
	ObservationID string          `json:"observation_id,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	Name          string          `json:"name"`
	DataType      string          `json:"data_type"`
	Value         *float64        `json:"value,omitempty"`
	StringValue   *string         `json:"string_value,omitempty"`
	Comment       string          `json:"comment,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Timestamp     string          `json:"timestamp"`
	CreatedAt     string          `json:"created_at"`
}

// handleCreateScores accepts one score object or an array of them. An array is
// all-or-nothing: one transaction, and one 400 naming the first invalid item
// (#7).
func (s *Server) handleCreateScores(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	// The write routes take no query parameters at all, and one that was
	// sent means the caller expected it to do something (#21, #23).
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}

	requests, err := decodeScores(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(requests) == 0 {
		// Nothing to write is a client bug, not a no-op (edge cases).
		writeError(w, http.StatusBadRequest, "no scores in the request")
		return
	}

	now := time.Now().UnixNano()
	write := &store.ScoreWrite{ProjectID: project.ID, Scores: make([]*store.Score, 0, len(requests))}
	ids := make([]string, 0, len(requests))
	seen := make(map[string]int, len(requests))
	for i, request := range requests {
		score, err := request.validate(now)
		if err != nil {
			writeError(w, http.StatusBadRequest, itemError(len(requests), i, err))
			return
		}
		// Writes upsert by id (#3), so a batch naming one id twice
		// would answer with two ids for one stored row — a client
		// counting what it wrote would be counting wrong (#23).
		if first, duplicate := seen[score.ID]; duplicate {
			writeError(w, http.StatusBadRequest, itemError(len(requests), i,
				fmt.Errorf("id %s is already used by the score at index %d", score.ID, first)))
			return
		}
		seen[score.ID] = i
		write.Scores = append(write.Scores, score)
		ids = append(ids, score.ID)
	}

	if !s.submit(w, r, write) {
		return
	}
	// 201 only now: the transaction is committed and fsynced (#9).
	writeJSON(w, http.StatusCreated, scoreIDsResponse{IDs: ids})
}

// decodeScores reads the body as either one score or an array of them, keeping
// the strictness of a single object in both shapes (#17).
func decodeScores(body []byte) ([]*scoreRequest, error) {
	// TrimLeft on the bytes, not on a string copy of them: the body can be
	// megabytes, and all that is needed is its first meaningful character.
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
		var requests []*scoreRequest
		if err := decodeStrict(body, &requests); err != nil {
			return nil, err
		}
		for i, request := range requests {
			if request == nil {
				return nil, fmt.Errorf("score at index %d is null", i)
			}
		}
		return requests, nil
	}
	var request scoreRequest
	if err := decodeStrict(body, &request); err != nil {
		return nil, err
	}
	return []*scoreRequest{&request}, nil
}

// itemError names which item of an array POST was refused; a single object
// speaks for itself.
func itemError(total, index int, err error) string {
	if total == 1 {
		return err.Error()
	}
	return fmt.Sprintf("score at index %d: %s", index, err)
}

// validate turns a request into a storable score, resolving the data type and
// checking every cross-field rule of #5.
func (in *scoreRequest) validate(now int64) (*store.Score, error) {
	score := &store.Score{
		TraceID:       text(in.TraceID),
		ObservationID: text(in.ObservationID),
		SessionID:     text(in.SessionID),
		Name:          in.Name,
		Value:         in.Value,
		StringValue:   in.StringValue,
		Comment:       in.Comment,
		Timestamp:     now,
		CreatedAt:     now,
	}

	// An empty string is never a meaningful id; reading it as "absent"
	// would hide a client's bug rather than report it.
	for _, target := range []struct {
		field string
		value *string
	}{
		{"trace_id", in.TraceID},
		{"observation_id", in.ObservationID},
		{"session_id", in.SessionID},
	} {
		if target.value != nil && *target.value == "" {
			return nil, fmt.Errorf("%q must not be empty", target.field)
		}
	}
	if score.Name == "" {
		return nil, fmt.Errorf(`"name" is required`)
	}
	if len(score.Name) > maxScoreNameLength {
		return nil, fmt.Errorf(`"name" must be at most %d characters`, maxScoreNameLength)
	}

	// A score targets at least one of trace or session; an observation is
	// addressed within its trace. Whether the target exists is deliberately
	// not checked — an eval may grade a trace whose spans are still in
	// flight (#4).
	if score.TraceID == "" && score.SessionID == "" {
		return nil, fmt.Errorf(`a score needs a "trace_id" or a "session_id"`)
	}
	if score.ObservationID != "" && score.TraceID == "" {
		return nil, fmt.Errorf(`"observation_id" also needs the "trace_id" it belongs to`)
	}

	if in.ID != nil {
		if !hexID.MatchString(*in.ID) {
			return nil, fmt.Errorf(`"id" must be 32 lower-case hex characters, got %q`, *in.ID)
		}
		score.ID = *in.ID
	} else {
		generated, err := store.NewID()
		if err != nil {
			return nil, fmt.Errorf("cannot generate a score id: %w", err)
		}
		score.ID = generated
	}

	dataType, err := resolveDataType(in)
	if err != nil {
		return nil, err
	}
	score.DataType = dataType

	if jsonValue(in.Metadata) {
		score.Metadata = compactJSON(in.Metadata)
	}
	if in.Timestamp != nil {
		// Event time, not receive time: when the graded interaction
		// happened (#16). Absent, it defaults to now.
		timestamp, err := parseTime(`"timestamp"`, *in.Timestamp)
		if err != nil {
			return nil, err
		}
		score.Timestamp = timestamp
	}
	return score, nil
}

// resolveDataType applies #5: numeric and boolean carry `value`, categorical
// and text carry `string_value`, the other field must be absent, and an
// omitted type is inferred from whichever field was sent.
func resolveDataType(in *scoreRequest) (string, error) {
	if in.Value != nil && in.StringValue != nil {
		return "", fmt.Errorf(`a score carries either "value" or "string_value", not both`)
	}
	dataType := in.DataType
	switch dataType {
	case "":
		// The common cases need no ceremony: a float from an eval is
		// numeric, a verdict from a judge is text. Boolean and
		// categorical are semantic claims, so they must be stated.
		switch {
		case in.Value != nil:
			return store.ScoreNumeric, nil
		case in.StringValue != nil:
			return store.ScoreText, nil
		}
		return "", fmt.Errorf(`a score needs a "value" or a "string_value"`)
	case store.ScoreNumeric:
		if in.Value == nil {
			return "", fmt.Errorf(`a numeric score needs a "value"`)
		}
	case store.ScoreBoolean:
		if in.Value == nil {
			return "", fmt.Errorf(`a boolean score needs a "value" of 0 or 1`)
		}
		if *in.Value != 0 && *in.Value != 1 {
			return "", fmt.Errorf(`a boolean score's "value" must be 0 or 1, got %v`, *in.Value)
		}
	case store.ScoreCategorical, store.ScoreText:
		if in.StringValue == nil {
			return "", fmt.Errorf(`a %s score needs a "string_value"`, dataType)
		}
	default:
		return "", fmt.Errorf(`"data_type" must be one of numeric, boolean, categorical, text, got %q`, dataType)
	}
	if (dataType == store.ScoreNumeric || dataType == store.ScoreBoolean) && in.StringValue != nil {
		return "", fmt.Errorf(`a %s score must not carry a "string_value"`, dataType)
	}
	if (dataType == store.ScoreCategorical || dataType == store.ScoreText) && in.Value != nil {
		return "", fmt.Errorf(`a %s score must not carry a "value"`, dataType)
	}
	return dataType, nil
}

// handleListScores serves the filtered, cursor-paginated listing, newest first.
func (s *Server) handleListScores(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "trace_id", "observation_id", "session_id",
		"name", "data_type", "from", "to", "limit", "cursor")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	filter := store.ScoreFilter{
		TraceID:       values.Get("trace_id"),
		ObservationID: values.Get("observation_id"),
		SessionID:     values.Get("session_id"),
		Name:          values.Get("name"),
		DataType:      values.Get("data_type"),
		// One row beyond the page tells us whether there is a next one.
		Limit: limit + 1,
	}
	if filter.DataType != "" && !validDataType(filter.DataType) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("data_type must be one of numeric, boolean, categorical, text, got %q", filter.DataType))
		return
	}
	// The range is half-open — `from` inclusive, `to` exclusive — so that
	// walking a timeline a day at a time never reports a score twice.
	for _, bound := range []struct {
		name   string
		target **int64
	}{
		{"from", &filter.From},
		{"to", &filter.To},
	} {
		raw := values.Get(bound.name)
		if raw == "" {
			continue
		}
		instant, err := parseTime(bound.name, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		*bound.target = &instant
	}
	if raw := values.Get("cursor"); raw != "" {
		parts, err := decodeCursor(raw, 2)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		timestamp, convErr := strconv.ParseInt(parts[0], 10, 64)
		if convErr != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		filter.After = &store.ScoreCursor{Timestamp: timestamp, ID: parts[1]}
	}

	scores, err := s.store.Scores(project.ID, filter)
	if err != nil {
		slog.Error("list scores failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list scores")
		return
	}

	var next *string
	if len(scores) > limit {
		scores = scores[:limit]
		last := scores[len(scores)-1]
		cursor := encodeCursor(strconv.FormatInt(last.Timestamp, 10), last.ID)
		next = &cursor
	}
	out := make([]scoreResponse, 0, len(scores))
	for _, score := range scores {
		out = append(out, renderScore(score))
	}
	writeJSON(w, http.StatusOK, scoreListResponse{Scores: out, NextCursor: next})
}

// handleGetScore serves one score by id.
func (s *Server) handleGetScore(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	score, err := s.store.Score(project.ID, id)
	if err != nil {
		slog.Error("read score failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the score")
		return
	}
	if score == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("score %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, renderScore(score))
}

func renderScore(score *store.Score) scoreResponse {
	return scoreResponse{
		ID:            score.ID,
		TraceID:       score.TraceID,
		ObservationID: score.ObservationID,
		SessionID:     score.SessionID,
		Name:          score.Name,
		DataType:      score.DataType,
		Value:         score.Value,
		StringValue:   score.StringValue,
		Comment:       score.Comment,
		Metadata:      json.RawMessage(score.Metadata),
		Timestamp:     formatTime(score.Timestamp),
		CreatedAt:     formatTime(score.CreatedAt),
	}
}

func validDataType(value string) bool {
	switch value {
	case store.ScoreNumeric, store.ScoreBoolean, store.ScoreCategorical, store.ScoreText:
		return true
	}
	return false
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
