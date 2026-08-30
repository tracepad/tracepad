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
	release     string
	version     string
	kind        string
	prompt      string
	onlyErrors  bool
}

// register adds the filters a listing takes: every one of them, the upper
// bound of a time range included.
func (f *traceFilterFlags) register(fs *flag.FlagSet) {
	f.registerFollowing(fs)
	fs.StringVar(&f.until, "until", "", "")
}

// registerFollowing adds the filters a *follow* takes, which is all of them
// but `--until` — and it is where a new shared filter belongs, so that `tail`
// keeps getting them.
//
// An upper bound on a tail is either nothing — the newest page is never past
// it — or a silent end to the following, and neither is what somebody typing
// it meant; the bound on a range of traces is `traces ls --until`. It used to
// be registered here with the rest because the three commands share their
// filters so they cannot drift apart, and the one thing they must differ by
// had no usage line to disagree with. Now `tail --until` is an unknown flag,
// which is the answer a reader can act on (INBOX, PR #9).
func (f *traceFilterFlags) registerFollowing(fs *flag.FlagSet) {
	fs.StringVar(&f.environment, "env", "", "")
	fs.StringVar(&f.user, "user", "", "")
	fs.StringVar(&f.session, "session", "", "")
	fs.StringVar(&f.name, "name", "", "")
	fs.StringVar(&f.tag, "tag", "", "")
	fs.StringVar(&f.since, "since", "", "")
	fs.StringVar(&f.minCost, "min-cost", "", "")
	fs.StringVar(&f.release, "release", "", "")
	fs.StringVar(&f.version, "version", "", "")
	// `--type`, not `--kind`: the parameter is `type` and the CLI's flags
	// are named after the API's (#1). The field is `kind` because `type` is
	// a keyword.
	fs.StringVar(&f.kind, "type", "", "")
	fs.StringVar(&f.prompt, "prompt", "", "")
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
	addSome(query, "release", f.release)
	addSome(query, "version", f.version)
	addSome(query, "type", f.kind)
	addSome(query, "prompt", f.prompt)
	if f.onlyErrors {
		query.Set("status", "error")
	}
	from, err := r.instant("--since", f.since)
	if err != nil {
		return nil, err
	}
	addSome(query, "from", from)
	to, err := r.instant("--until", f.until)
	if err != nil {
		return nil, err
	}
	addSome(query, "to", to)
	return query, nil
}

func (r *run) tracesList(ctx context.Context, args []string) error {
	var (
		filters traceFilterFlags
		search  string
		fields  string
		cursor  string
		limit   int
		oldest  bool
		newer   bool
		total   bool
	)
	fs := r.flags("traces ls")
	filters.register(fs)
	// Registered here and on `traces last` rather than with the shared
	// filters: `tail` takes those too, and following the newest page is not a
	// question about text (spec 011, CLI contract).
	fs.StringVar(&search, "search", "", "")
	fs.StringVar(&fields, "fields", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	// The API's `direction=prev` under two names, because the two things a
	// person does with it read differently: `--oldest` jumps to the far end
	// (no cursor), `--newer` walks back up from one. Keyset reaches either
	// for the price of any other page.
	fs.BoolVar(&oldest, "oldest", false, "")
	fs.BoolVar(&newer, "newer", false, "")
	fs.BoolVar(&total, "total", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query, err := filters.query(r)
	if err != nil {
		return err
	}
	addSome(query, "q", search)
	addSome(query, "fields", fields)
	addSome(query, "cursor", cursor)
	if err := addWalk(query, cursor, oldest, newer); err != nil {
		return err
	}
	if total {
		query.Set("count", "1")
	}
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
		Traces      []traceRow `json:"traces"`
		NextCursor  *string    `json:"next_cursor"`
		PrevCursor  *string    `json:"prev_cursor"`
		Total       *int       `json:"total"`
		TotalCapped *bool      `json:"total_capped"`
	}](body)
	if err != nil {
		return err
	}
	renderTraceTable(r.opt.Stdout, listing.Traces, r.opt.TTY)
	if listing.Total != nil {
		fmt.Fprintf(r.opt.Stdout, "\n%s matching\n", matchCount(*listing.Total, listing.TotalCapped))
	}
	walkOn(r, listing.NextCursor, listing.PrevCursor)
	return nil
}

// walkOn prints the commands that continue the walk in either direction.
// Both, because `--oldest` lands somewhere with no next page, and a listing
// that says nothing there is a dead end (PR #11 review).
func walkOn(r *run, next, prev *string) {
	if next != nil {
		fmt.Fprintf(r.opt.Stdout, "\nolder: --cursor %s\n", *next)
	}
	if prev != nil {
		fmt.Fprintf(r.opt.Stdout, "newer: --newer --cursor %s\n", *prev)
	}
}

/*
addWalk turns the two direction flags into the API's one parameter, and
refuses the combination that would quietly mean something else.

`--oldest` is "the far end", which is `direction=prev` with *no* cursor;
`--newer --cursor X` is "the page above X", which is `direction=prev` *with*
one. Given both, the cursor wins and the jump silently does not happen — so
it is an error rather than a surprise (the house rule of spec 003 #23).
*/
func addWalk(query url.Values, cursor string, oldest, newer bool) error {
	if oldest && cursor != "" {
		return usageErrorf("--oldest starts at the far end and takes no --cursor; " +
			"use --newer --cursor to walk back towards newer rows")
	}
	// The mirror of it: without a cursor, `direction=prev` *is* the far end,
	// so a bare `--newer` would jump to the oldest page — the opposite of
	// what it says (PR #11, third review).
	if newer && cursor == "" {
		return usageErrorf("--newer walks back from a --cursor; " +
			"use --oldest to jump to the far end of the listing")
	}
	if oldest || newer {
		query.Set("direction", "prev")
	}
	return nil
}

// matchCount renders a capped count: the number, or the number and a plus
// where the server stopped counting rather than kept going (spec 009 #4).
func matchCount(total int, capped *bool) string {
	if capped != nil && *capped {
		return fmt.Sprintf("%d+", total)
	}
	return strconv.Itoa(total)
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
		search  string
		full    bool
	)
	fs := r.flags("traces last")
	filters.register(fs)
	fs.StringVar(&search, "search", "", "")
	fs.BoolVar(&full, "full", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query, err := filters.query(r)
	if err != nil {
		return err
	}
	addSome(query, "q", search)
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
	filters.registerFollowing(fs)
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

// tailOverlap is how far behind the newest trace a follow keeps looking.
//
// A trace's `timestamp` is when its earliest span *started*, not when it was
// committed, and exporters batch: the OTel SDK's default processor flushes
// every five seconds, so a run that began at 12:00:00 can be stored after one
// that began at 12:00:03. Following only what is newer than the newest row
// already seen would silently skip it — missing lines in something a person
// reads as a live log (Decision 31). Looking back over a window and skipping
// what was already printed costs one page and produces no duplicates.
const tailOverlap = 60 * time.Second

// maxTailMemory bounds the ids remembered inside that window. Past it the
// oldest are forgotten and the follow narrows back towards its watermark,
// which is the honest failure mode: a bounded command that might repeat a line
// under extreme load beats one that grows without limit.
const maxTailMemory = 10000

// follow polls the listing and prints what it has not printed before, oldest
// first, until the context ends.
func (r *run) follow(ctx context.Context, query url.Values, interval time.Duration) error {
	window := &tailWindow{seen: map[string]time.Time{}}
	for {
		fresh, err := r.poll(ctx, query, window)
		if err != nil {
			if ctx.Err() != nil {
				// Interrupted while a poll was in flight. Being
				// stopped is what was asked for, so it is not a
				// failure to report — Ctrl-C must not exit 1.
				return nil
			}
			return err
		}
		// Oldest first: a follow reads like a log, not like a listing.
		for i := len(fresh) - 1; i >= 0; i-- {
			r.printTailed(fresh[i])
		}
		window.advance(fresh)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// tailWindow is what a follow remembers: how far back it still looks, and what
// it has already printed inside that window.
type tailWindow struct {
	mark *tailMark
	seen map[string]time.Time
}

// wants reports whether a row should be printed: inside the window, and not
// already shown. Before the first page there is no window, so that page is the
// tail of the stream, the way tail(1) shows the end of a file before following
// it.
func (w *tailWindow) wants(row traceRow) bool {
	if _, printed := w.seen[row.ID]; printed {
		return false
	}
	return w.mark.after(row)
}

// advance records what was printed and moves the window to
// (newest seen) - tailOverlap, forgetting what has fallen out of it.
func (w *tailWindow) advance(printed []tailed) {
	newest := time.Time{}
	if w.mark != nil {
		newest = w.mark.at.Add(tailOverlap)
	}
	for _, entry := range printed {
		at := instantOf(entry.row)
		w.seen[entry.row.ID] = at
		if at.After(newest) {
			newest = at
		}
	}
	w.mark = &tailMark{at: newest.Add(-tailOverlap)}
	for id, at := range w.seen {
		if at.Before(w.mark.at) {
			delete(w.seen, id)
		}
	}
	for len(w.seen) > maxTailMemory {
		oldest, oldestAt := "", time.Time{}
		for id, at := range w.seen {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = id, at
			}
		}
		delete(w.seen, oldest)
	}
}

// tailMark is the oldest instant a follow still looks back to. A nil mark is
// the first poll, which prints the page it gets.
type tailMark struct {
	at time.Time
}

// after reports whether a row falls inside the window.
//
// The instants are compared as times, never as their RFC 3339 text: the
// server renders variable fractional precision, so "…:00Z" and "…:00.002Z"
// order the wrong way round as strings — the later trace would look older and
// a follow would stop at it, printing nothing ever again.
func (m *tailMark) after(row traceRow) bool {
	if m == nil {
		return true
	}
	at := instantOf(row)
	if at.IsZero() {
		// No usable instant: it belongs at the oldest end, which the
		// walk has already passed.
		return false
	}
	return at.After(m.at)
}

// instantOf reads a row's timestamp. A trace whose spans never started has
// none, and the zero time is where such a trace sorts in the listing too.
func instantOf(row traceRow) time.Time {
	at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return at
}

// tailed is one row of a follow, kept both parsed (for the watermark and the
// table) and raw (so JSON mode prints the API's own bytes rather than this
// struct's re-encoding of them).
type tailed struct {
	row traceRow
	raw json.RawMessage
}

// poll fetches the newest page and keeps what the window has not printed. The
// walk stops at the first row older than the window rather than at the first
// row already seen: inside the overlap, an unseen row can sit behind a seen
// one, which is the whole point of looking back.
func (r *run) poll(ctx context.Context, query url.Values, window *tailWindow) ([]tailed, error) {
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
		if !window.mark.after(row) {
			break
		}
		if window.wants(row) {
			fresh = append(fresh, tailed{row: row, raw: raw})
		}
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
