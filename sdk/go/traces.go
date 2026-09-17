package tracepad

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Deleting traces: one by id, or every trace a listing filter matches (spec
// 036). A script's act — an eval harness that exported under the wrong key,
// a test suite that wants its traces gone before the next run — so both
// calls are synchronous and return errors, like every REST call here. The
// file is its own so that the tracing path never imports it.

// TraceFilter is the trace listing's filters, one field per parameter, by
// their API names (docs/api.md). To is required so that the set is closed;
// From and To are sent as RFC 3339 UTC; Tag is repeatable, and a trace must
// carry every tag given. An empty field is not sent.
type TraceFilter struct {
	From, To                             time.Time
	Environment, UserID, SessionID, Name string
	Tag                                  []string
	Status, MinCost, Q, Release, Version string
	Type, Prompt, RunID, ItemID          string
}

func (f TraceFilter) values() (url.Values, error) {
	if f.To.IsZero() {
		return nil, errors.New("tracepad: DeleteTraces: To is required: a deletion by filter names " +
			"the moment before which traces go, so that what was previewed is what is deleted")
	}
	params := url.Values{"to": {rfc3339(f.To)}}
	if !f.From.IsZero() {
		params.Set("from", rfc3339(f.From))
	}
	for name, value := range map[string]string{
		"environment": f.Environment, "user_id": f.UserID, "session_id": f.SessionID, "name": f.Name,
		"status": f.Status, "min_cost": f.MinCost, "q": f.Q, "release": f.Release, "version": f.Version,
		"type": f.Type, "prompt": f.Prompt, "run_id": f.RunID, "item_id": f.ItemID,
	} {
		if value != "" {
			params.Set(name, value)
		}
	}
	for _, tag := range f.Tag {
		params.Add("tag", tag)
	}
	return params, nil
}

// roundClient waits for one confirmed round: the server sizes a round for
// the interface's thirty-second clock, and this leaves room over it.
var roundClient = &http.Client{Timeout: 60 * time.Second}

// DeleteOption configures DeleteTraces.
type DeleteOption func(url.Values)

// WithRoundLimit is the most traces one confirmed round takes (1 to 1000,
// 1000 by default). It bounds a round, not the whole: the rounds go on
// until the API says there is no more.
func WithRoundLimit(limit int) DeleteOption {
	return func(p url.Values) { p.Set("limit", strconv.Itoa(limit)) }
}

// DeleteTrace deletes one trace and everything attached to it. With confirm
// false it is the API's dry run, and the answer is its preview
// (would_delete, affected_runs, confirm, note); with confirm true the id is
// sent as the echo the API asks for, and the answer says what went. An
// unknown id is a 404 *HTTPError either way.
func DeleteTrace(ctx context.Context, id string, confirm bool) (map[string]any, error) {
	params := url.Values{}
	if confirm {
		params.Set("confirm", id)
	}
	return del(ctx, "/api/v1/traces/"+url.PathEscape(id), params, httpClient)
}

// DeleteTraces deletes every trace the filter matches that started before
// filter.To. With an empty confirm it is one dry run, and the answer is the
// API's preview (matched, would_delete, affected_runs, oldest, confirm,
// note). With the project's name it deletes in rounds of at most the limit,
// repeating while the API says more, and answers the total —
// {"deleted": {"traces", "observations", "scores", "payloads",
// "annotation_items"}, "rounds"} — with float64 numbers, as in every answer
// the package decodes. A wrong echo is the 400 of the first round, before
// anything went; a round that fails later is returned as it is — the rounds
// before it are done and consistent, and a repeat continues.
func DeleteTraces(ctx context.Context, filter TraceFilter, confirm string, opts ...DeleteOption) (map[string]any, error) {
	params, err := filter.values()
	if err != nil {
		return nil, err
	}
	if confirm == "" {
		return del(ctx, "/api/v1/traces", params, httpClient)
	}
	params.Set("confirm", confirm)
	params.Set("limit", "1000")
	for _, opt := range opts {
		opt(params)
	}
	deleted := map[string]any{}
	rounds := 0
	for {
		answer, err := del(ctx, "/api/v1/traces", params, roundClient)
		if err != nil {
			return nil, err
		}
		rounds++
		counts, _ := answer["deleted"].(map[string]any)
		for kind, count := range counts {
			n, _ := count.(float64)
			sum, _ := deleted[kind].(float64)
			deleted[kind] = sum + n
		}
		if more, _ := answer["more"].(bool); !more {
			return map[string]any{"deleted": deleted, "rounds": float64(rounds)}, nil
		}
	}
}

func del(ctx context.Context, path string, params url.Values, client *http.Client) (map[string]any, error) {
	c, err := current()
	if err != nil {
		return nil, err
	}
	answer, err := requestWith(ctx, client, c, "DELETE", path, nil, params)
	if err != nil {
		return nil, err
	}
	return object(answer), nil
}
