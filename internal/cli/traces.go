package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/config"
)

// `traces ls`, `traces show`, `traces last` and `tail` — the commands the
// design sketch names as the reason the CLI exists (design §3.3).

// fullBudget is what `--full` asks for: the largest budget the API allows, so
// that a payload comes back whole unless it is genuinely enormous.
const fullBudget = config.MaxResponseBudgetBytes

func (r *run) traces(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.tracesList(ctx, rest)
	case "show":
		return r.tracesShow(ctx, rest)
	case "last":
		return r.tracesLast(ctx, rest)
	}
	return usageErrorf("traces takes ls, show or last, got %q", sub)
}

// traceFilterFlags are the filters `traces ls`, `traces last` and `tail`
// share, registered once so the three cannot drift apart.
type traceFilterFlags struct {
	environment string
	user        string
	session     string
	name        string
	tag         string
	since       string
	until       string
	minCost     string
	onlyErrors  bool
}

func (f *traceFilterFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.environment, "env", "", "")
	fs.StringVar(&f.user, "user", "", "")
	fs.StringVar(&f.session, "session", "", "")
	fs.StringVar(&f.name, "name", "", "")
	fs.StringVar(&f.tag, "tag", "", "")
	fs.StringVar(&f.since, "since", "", "")
	fs.StringVar(&f.until, "until", "", "")
	fs.StringVar(&f.minCost, "min-cost", "", "")
	fs.BoolVar(&f.onlyErrors, "error", false, "")
}

// query turns the flags into the endpoint's own query parameters. The mapping
// is the whole of the CLI's "logic": every filter is a parameter the API
// already has (#1).
func (f *traceFilterFlags) query(r *run) (url.Values, error) {
	query := url.Values{}
	addSome(query, "environment", f.environment)
	addSome(query, "user_id", f.user)
	addSome(query, "session_id", f.session)
	addSome(query, "name", f.name)
	addSome(query, "tag", f.tag)
	addSome(query, "min_cost", f.minCost)
	if f.onlyErrors {
		query.Set("status", "error")
	}
	from, err := r.since(f.since)
	if err != nil {
		return nil, err
	}
	addSome(query, "from", from)
	to, err := r.since(f.until)
	if err != nil {
		return nil, err
	}
	addSome(query, "to", to)
	return query, nil
}

func (r *run) tracesList(ctx context.Context, args []string) error {
	var (
		filters traceFilterFlags
		fields  string
		cursor  string
		limit   int
	)
	fs := r.flags("traces ls")
	filters.register(fs)
	fs.StringVar(&fields, "fields", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query, err := filters.query(r)
	if err != nil {
		return err
	}
	addSome(query, "fields", fields)
	addSome(query, "cursor", cursor)
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/traces", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Traces     []traceRow `json:"traces"`
		NextCursor *string    `json:"next_cursor"`
	}](body)
	if err != nil {
		return err
	}
	renderTraceTable(r.opt.Stdout, listing.Traces)
	if listing.NextCursor != nil {
		fmt.Fprintf(r.opt.Stdout, "\nmore: --cursor %s\n", *listing.NextCursor)
	}
	return nil
}

func (r *run) tracesShow(ctx context.Context, args []string) error {
	var full bool
	fs := r.flags("traces show")
	fs.BoolVar(&full, "full", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/traces/"+url.PathEscape(rest[0]), expansion(full))
	if err != nil {
		return err
	}
	return r.showTrace(body)
}

func (r *run) tracesLast(ctx context.Context, args []string) error {
	var (
		filters traceFilterFlags
		full    bool
	)
	fs := r.flags("traces last")
	filters.register(fs)
	fs.BoolVar(&full, "full", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query, err := filters.query(r)
	if err != nil {
		return err
	}
	for key, values := range expansion(full) {
		query[key] = values
	}

	body, err := r.api.Get(ctx, "/api/v1/traces/last", query)
	if err != nil {
		return err
	}
	return r.showTrace(body)
}

func (r *run) showTrace(body json.RawMessage) error {
	if r.wantJSON() {
		return r.emit(body)
	}
	trace, err := decode[traceDetail](body)
	if err != nil {
		return err
	}
	renderTraceDetail(r.opt.Stdout, trace)
	return nil
}

// expansion is what `--full` means: the payloads, with the largest budget the
// API allows.
func expansion(full bool) url.Values {
	if !full {
		return url.Values{}
	}
	return url.Values{
		"expand": []string{"io"},
		"budget": []string{strconv.Itoa(fullBudget)},
	}
}

// defaultTailInterval is how often `tail` asks. Polling the public API needs
// no new server surface and inherits auth, filters and budgets; two seconds is
// below the threshold at which a person notices the delay (#13).
const defaultTailInterval = 2 * time.Second

func (r *run) tail(ctx context.Context, args []string) error {
	var (
		filters  traceFilterFlags
		interval time.Duration
		limit    int
	)
	fs := r.flags("tail")
	filters.register(fs)
	fs.DurationVar(&interval, "interval", defaultTailInterval, "")
	fs.IntVar(&limit, "limit", 0, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	if interval <= 0 {
		return usageErrorf("--interval must be positive, got %s", interval)
	}
	query, err := filters.query(r)
	if err != nil {
		return err
	}
	if err := addLimit(query, limit); err != nil {
		return err
	}
	return r.follow(ctx, query, interval)
}

// follow polls the listing and prints what is newer than the last row it saw.
// The watermark is the cursor pair, not a wall clock: a trace committed a
// moment ago with an older timestamp is still new to this walk, and a
// time-window poll would either miss it or print it twice (#13).
func (r *run) follow(ctx context.Context, query url.Values, interval time.Duration) error {
	var watermark *tailMark
	for {
		fresh, err := r.poll(ctx, query, watermark)
		if err != nil {
			if ctx.Err() != nil {
				// Interrupted while a poll was in flight. Being
				// stopped is what was asked for, so it is not a
				// failure to report — Ctrl-C must not exit 1.
				return nil
			}
			return err
		}
		if len(fresh) > 0 {
			// Oldest first: a follow reads like a log, not like a
			// listing.
			for i := len(fresh) - 1; i >= 0; i-- {
				r.printTailed(fresh[i])
			}
			watermark = newTailMark(fresh[0].row)
		} else if watermark == nil {
			// Nothing at all yet: start following from now, so the
			// first trace to arrive is printed.
			watermark = &tailMark{}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// tailMark is the last row a follow printed. A nil mark is the first poll,
// which prints the page it gets.
type tailMark struct {
	at time.Time
	id string
}

// newTailMark reads the cursor pair off a row. An instant that does not parse
// leaves the mark's time zero, which sorts before everything — the same place
// a trace whose spans never started sorts in the listing.
func newTailMark(row traceRow) *tailMark {
	mark := &tailMark{id: row.ID}
	if at, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil {
		mark.at = at
	}
	return mark
}

// after reports whether a row is newer than the mark.
//
// The instants are compared as times, never as their RFC 3339 text: the
// server renders variable fractional precision, so "…:00Z" and "…:00.002Z"
// order the wrong way round as strings — the later trace would look older and
// a follow would stop at it, printing nothing ever again.
func (m *tailMark) after(row traceRow) bool {
	if m == nil {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil {
		// No usable instant: it belongs at the oldest end, which the
		// walk has already passed.
		return false
	}
	if !at.Equal(m.at) {
		return at.After(m.at)
	}
	return row.ID > m.id
}

// tailed is one row of a follow, kept both parsed (for the watermark and the
// table) and raw (so JSON mode prints the API's own bytes rather than this
// struct's re-encoding of them).
type tailed struct {
	row traceRow
	raw json.RawMessage
}

// poll fetches the newest page and keeps what the mark has not seen. On the
// very first poll it returns the page as it is, the way `tail` shows the end
// of a file before following it.
func (r *run) poll(ctx context.Context, query url.Values, mark *tailMark) ([]tailed, error) {
	body, err := r.api.Get(ctx, "/api/v1/traces", query)
	if err != nil {
		return nil, err
	}
	listing, err := decode[struct {
		Traces []json.RawMessage `json:"traces"`
	}](body)
	if err != nil {
		return nil, err
	}
	var fresh []tailed
	for _, raw := range listing.Traces {
		row, err := decode[traceRow](raw)
		if err != nil {
			return nil, err
		}
		if !mark.after(row) {
			break
		}
		fresh = append(fresh, tailed{row: row, raw: raw})
	}
	return fresh, nil
}

func (r *run) printTailed(entry tailed) {
	if r.wantJSON() {
		fmt.Fprintf(r.opt.Stdout, "%s\n", entry.raw)
		return
	}
	row := entry.row
	status := "ok"
	if row.ErrorCount > 0 {
		status = fmt.Sprintf("%d errors", row.ErrorCount)
	}
	fmt.Fprintf(r.opt.Stdout, "%s  %s  %s  %s  %s  %s\n",
		shortTime(row.Timestamp), row.ID, orDash(row.Name),
		orDash(row.Environment), status, duration(row.LatencyMs))
}
