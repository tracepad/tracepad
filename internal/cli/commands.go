package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// sortStrings keeps the small ordered lists the tables render deterministic.
func sortStrings(values []string) { slices.Sort(values) }

// The rest of the command line: sessions, scores, prompts, stats, system.
// Each is the same three steps — read the flags, call the endpoint, render —
// because anything more would be logic the API does not have (#1).

func (r *run) sessions(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.sessionsList(ctx, rest)
	case "show":
		return r.sessionsShow(ctx, rest)
	}
	return usageErrorf("sessions takes ls or show, got %q", sub)
}

// sessionsList is `GET /api/v1/sessions` and nothing more (#1): the flags are
// the endpoint's own filters, and the cursor is walked the way `traces ls`
// walks it.
func (r *run) sessionsList(ctx context.Context, args []string) error {
	var (
		since       string
		until       string
		environment string
		user        string
		cursor      string
		limit       int
		oldest      bool
		newer       bool
		total       bool
	)
	fs := r.flags("sessions ls")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	// `--env` rather than `--environment`, for the same reason `traces ls`
	// spells it that way (spec 007 #11).
	fs.StringVar(&environment, "env", "", "")
	fs.StringVar(&user, "user", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	fs.BoolVar(&oldest, "oldest", false, "")
	fs.BoolVar(&newer, "newer", false, "")
	fs.BoolVar(&total, "total", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query := url.Values{}
	addSome(query, "environment", environment)
	addSome(query, "user_id", user)
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if err := addWalk(query, cursor, oldest, newer); err != nil {
		return err
	}
	if total {
		query.Set("count", "1")
	}
	from, err := r.instant("--since", since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	to, err := r.instant("--until", until)
	if err != nil {
		return err
	}
	addSome(query, "to", to)
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/sessions", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Sessions []struct {
			ID         string   `json:"id"`
			TraceCount int      `json:"trace_count"`
			ErrorCount int      `json:"error_count"`
			TotalCost  *float64 `json:"total_cost"`
			FirstSeen  string   `json:"first_seen"`
			LastSeen   string   `json:"last_seen"`
		} `json:"sessions"`
		NextCursor  *string `json:"next_cursor"`
		PrevCursor  *string `json:"prev_cursor"`
		Total       *int    `json:"total"`
		TotalCapped *bool   `json:"total_capped"`
	}](body)
	if err != nil {
		return err
	}
	// An empty page still falls through to the total and the way back: it is
	// where `--newer` from the newest page lands, and a bare "no sessions"
	// there is the dead end the first review found in `--oldest`
	// (PR #11, third review).
	if len(listing.Sessions) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no sessions")
	} else {
		t := newTable(r.opt.Stdout, "LAST SEEN", "SESSION", "TRACES", "ERRORS", "COST", "FIRST SEEN")
		for _, session := range listing.Sessions {
			t.row(shortTime(session.LastSeen), session.ID,
				strconv.Itoa(session.TraceCount), strconv.Itoa(session.ErrorCount),
				cost(session.TotalCost), shortTime(session.FirstSeen))
		}
		t.flush()
	}
	if listing.Total != nil {
		fmt.Fprintf(r.opt.Stdout, "\n%s matching\n", matchCount(*listing.Total, listing.TotalCapped))
	}
	walkOn(r, "older", listing.NextCursor, listing.PrevCursor)
	return nil
}

func (r *run) sessionsShow(ctx context.Context, args []string) error {
	var limit int
	fs := r.flags("sessions show")
	fs.IntVar(&limit, "limit", 0, "")
	positional, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	query := url.Values{}
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/sessions/"+url.PathEscape(positional[0]), query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	session, err := decode[struct {
		ID         string     `json:"id"`
		TraceCount int        `json:"trace_count"`
		TotalCost  *float64   `json:"total_cost"`
		ErrorCount int        `json:"error_count"`
		FirstSeen  string     `json:"first_seen"`
		LastSeen   string     `json:"last_seen"`
		Traces     []traceRow `json:"traces"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "session %s\n", session.ID)
	fmt.Fprintf(r.opt.Stdout, "  traces  %d (%d with errors)\n", session.TraceCount, session.ErrorCount)
	fmt.Fprintf(r.opt.Stdout, "  cost    %s\n", cost(session.TotalCost))
	fmt.Fprintf(r.opt.Stdout, "  window  %s .. %s\n\n",
		shortTime(session.FirstSeen), shortTime(session.LastSeen))
	renderTraceTable(r.opt.Stdout, session.Traces, r.opt.TTY)
	return nil
}

// usersList is `GET /api/v1/users` and nothing more (#1): the flags are the
// endpoint's own, and the walk is the one `traces ls` and `sessions ls` walk.
func (r *run) usersList(ctx context.Context, args []string) error {
	var (
		sortBy string
		prefix string
		cursor string
		limit  int
		oldest bool
		newer  bool
		total  bool
	)
	fs := r.flags("users ls")
	fs.StringVar(&sortBy, "sort", "", "")
	fs.StringVar(&prefix, "prefix", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	fs.BoolVar(&oldest, "oldest", false, "")
	fs.BoolVar(&newer, "newer", false, "")
	fs.BoolVar(&total, "total", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query := url.Values{}
	addSome(query, "sort", sortBy)
	addSome(query, "prefix", prefix)
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if err := addWalk(query, cursor, oldest, newer); err != nil {
		return err
	}
	if total {
		query.Set("count", "1")
	}
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/users", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Users       []userRow `json:"users"`
		NextCursor  *string   `json:"next_cursor"`
		PrevCursor  *string   `json:"prev_cursor"`
		Total       *int      `json:"total"`
		TotalCapped *bool     `json:"total_capped"`
	}](body)
	if err != nil {
		return err
	}
	// An empty page still falls through to the total and the way back, for
	// the reason `sessions ls` does: it is where `--newer` from the first
	// page lands.
	if len(listing.Users) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no users")
	} else {
		t := newTable(r.opt.Stdout,
			"USER", "TRACES", "SESSIONS", "ERRORS", "COST", "FIRST SEEN", "LAST SEEN")
		for _, user := range listing.Users {
			t.row(user.UserID, strconv.Itoa(user.Traces), strconv.Itoa(user.Sessions),
				strconv.Itoa(user.ErrorCount), cost(user.TotalCost),
				shortTime(user.FirstSeen), shortTime(user.LastSeen))
		}
		t.flush()
	}
	if listing.Total != nil {
		fmt.Fprintf(r.opt.Stdout, "\n%s matching\n", matchCount(*listing.Total, listing.TotalCapped))
	}
	// "older" would be a lie under three of the four sorts: the listing runs
	// down whatever key was asked for, and only `last_seen` makes that a
	// timeline.
	walkOn(r, "next", listing.NextCursor, listing.PrevCursor)
	return nil
}

// userRow is what both user endpoints render, which is the point of the shape
// being one shape (spec 023 #5).
type userRow struct {
	UserID     string   `json:"user_id"`
	Traces     int      `json:"traces"`
	ErrorCount int      `json:"error_count"`
	TotalCost  *float64 `json:"total_cost"`
	Sessions   int      `json:"sessions"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
}

func (r *run) usersShow(ctx context.Context, args []string) error {
	fs := r.flags("users show")
	positional, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/users/"+url.PathEscape(positional[0]), url.Values{})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	user, err := decode[struct {
		userRow
		LatencyMs struct {
			P50 *int64 `json:"p50"`
			P95 *int64 `json:"p95"`
		} `json:"latency_ms"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "user %s\n", user.UserID)
	fmt.Fprintf(r.opt.Stdout, "  traces    %d (%d with errors)\n", user.Traces, user.ErrorCount)
	fmt.Fprintf(r.opt.Stdout, "  sessions  %d\n", user.Sessions)
	fmt.Fprintf(r.opt.Stdout, "  cost      %s\n", cost(user.TotalCost))
	fmt.Fprintf(r.opt.Stdout, "  latency   p50 %s, p95 %s\n",
		duration(user.LatencyMs.P50), duration(user.LatencyMs.P95))
	fmt.Fprintf(r.opt.Stdout, "  window    %s .. %s\n",
		shortTime(user.FirstSeen), shortTime(user.LastSeen))
	return nil
}

func (r *run) scores(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.scoresList(ctx, rest)
	case "add":
		return r.scoresAdd(ctx, rest)
	case "rm":
		return r.scoresRemove(ctx, rest)
	case "trend":
		return r.scoresTrend(ctx, rest)
	}
	return usageErrorf("scores takes ls, add, rm or trend, got %q", sub)
}

// scoresTrend is `GET /api/v1/stats/scores` and nothing more (#1): how a score
// has moved, in the same buckets `stats` groups the traffic into (spec 025 #8).
//
// The verb sits under `scores` because that is the noun; `stats` keeps
// answering the traffic question it always did. The window flags are spelled
// as every other command spells them — `--since`, `--until`, `--env` (spec 007
// #11, spec 025 #15) — so that a person moving between `stats` and this does
// not have to learn a second name for the same window.
func (r *run) scoresTrend(ctx context.Context, args []string) error {
	var (
		name        string
		groupBy     string
		since       string
		until       string
		environment string
		limit       string
	)
	fs := r.flags("scores trend")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&groupBy, "group-by", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	fs.StringVar(&environment, "env", "", "")
	fs.StringVar(&limit, "limit", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query := url.Values{}
	addSome(query, "name", name)
	addSome(query, "group_by", groupBy)
	addSome(query, "environment", environment)
	addSome(query, "limit", limit)
	from, err := r.instant("--since", since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	to, err := r.instant("--until", until)
	if err != nil {
		return err
	}
	addSome(query, "to", to)

	body, err := r.api.Get(ctx, "/api/v1/stats/scores", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	result, err := decode[struct {
		GroupBy string `json:"group_by"`
		Targets string `json:"targets"`
		Omitted int    `json:"omitted"`
		Series  []struct {
			Name     string `json:"name"`
			DataType string `json:"data_type"`
			Buckets  []struct {
				Key        string           `json:"key"`
				Count      int              `json:"count"`
				Mean       *float64         `json:"mean"`
				Min        *float64         `json:"min"`
				Max        *float64         `json:"max"`
				Rate       *float64         `json:"rate"`
				Categories map[string]int64 `json:"categories"`
			} `json:"buckets"`
		} `json:"series"`
	}](body)
	if err != nil {
		return err
	}
	if len(result.Series) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no score names a trace in this range")
		return nil
	}
	for i, series := range result.Series {
		if i > 0 {
			fmt.Fprintln(r.opt.Stdout)
		}
		// The header names what was counted, because grouping by model
		// counts only the scores that name an observation (spec 025 #6)
		// and a reader comparing two runs of this command has to see it.
		fmt.Fprintf(r.opt.Stdout, "%s (%s, %s scores)\n",
			series.Name, series.DataType, result.Targets)
		key := strings.ToUpper(result.GroupBy)
		t := newTable(r.opt.Stdout, key, "SCORES", scoreTrendColumn(series.DataType))
		for _, bucket := range series.Buckets {
			t.row(bucket.Key, strconv.Itoa(bucket.Count),
				scoreTrendValue(series.DataType, bucket.Mean, bucket.Min, bucket.Max,
					bucket.Rate, bucket.Categories))
		}
		t.flush()
	}
	// A truncated answer that said nothing about it would be a wrong one
	// (spec 025 #24). The busiest names are the ones shown, and `--limit`
	// reaches the rest.
	if result.Omitted > 0 {
		noun := "names"
		if result.Omitted == 1 {
			noun = "name"
		}
		fmt.Fprintf(r.opt.Stdout,
			"\n%d rarer score %s not shown; raise --limit to see them\n",
			result.Omitted, noun)
	}
	return nil
}

// scoreTrendColumn and scoreTrendValue are the one column whose meaning
// depends on the series' type: a mean with its extremes, a rate, or the
// distribution the bucket saw.
func scoreTrendColumn(dataType string) string {
	switch dataType {
	case "boolean":
		return "RATE"
	case "categorical":
		return "CATEGORIES"
	default:
		return "MEAN (MIN..MAX)"
	}
}

func scoreTrendValue(dataType string, mean, min, max, rate *float64, categories map[string]int64) string {
	switch dataType {
	case "boolean":
		if rate == nil {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", *rate*100)
	case "categorical":
		names := make([]string, 0, len(categories))
		for value := range categories {
			names = append(names, value)
		}
		sortStrings(names)
		parts := make([]string, 0, len(names))
		for _, value := range names {
			parts = append(parts, fmt.Sprintf("%s %d", value, categories[value]))
		}
		return orDash(strings.Join(parts, " · "))
	default:
		if mean == nil {
			return "-"
		}
		if min == nil || max == nil {
			return scoreFigure(*mean)
		}
		return fmt.Sprintf("%s (%s..%s)", scoreFigure(*mean), scoreFigure(*min), scoreFigure(*max))
	}
}

// scoreFigure prints a score value the way somebody reads it back: three
// significant digits, which is what the interface renders too (spec 022 #3).
// `trimFloat` beside it is exact, which is right for a stored value and wrong
// for a mean — the mean of three thirds would otherwise arrive as sixteen
// digits of arithmetic nobody asked about.
//
// Rounded with `'g'` and printed with `'f'`, because those are two different
// questions. Nothing bounds a numeric score to 0..1 — a config's `min` and
// `max` are free and a name without a config has none — so `output_tokens` or
// `latency_ms` is an ordinary score, and `'g'` alone rendered 1234.5 as
// `1.23e+03` in a column the interface fills with `1230` (found in the second
// review of PR #44).
func scoreFigure(value float64) string {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'g', 3, 64), 64)
	if err != nil {
		rounded = value
	}
	if rounded == 0 {
		// Including a negative zero, which is a way of writing nothing.
		return "0"
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// scoresAdd is one `POST /api/v1/scores` with one object in it (spec 022 #7).
// The interface may do nothing the command line cannot, and spec 022 scores
// from a screen — so the command line scores too.
func (r *run) scoresAdd(ctx context.Context, args []string) error {
	var (
		trace       string
		observation string
		session     string
		name        string
		value       string
		stringValue string
		dataType    string
		comment     string
		id          string
	)
	fs := r.flags("scores add")
	fs.StringVar(&trace, "trace", "", "")
	fs.StringVar(&observation, "observation", "", "")
	fs.StringVar(&session, "session", "", "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&value, "value", "", "")
	fs.StringVar(&stringValue, "string", "", "")
	fs.StringVar(&dataType, "type", "", "")
	fs.StringVar(&comment, "comment", "", "")
	fs.StringVar(&id, "id", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	// Which flags were *given*, not which are non-empty: `--string ""` is a
	// text score of the empty string, and reading it as "no value" would
	// refuse a score the API accepts.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["value"] == given["string"] {
		return usageErrorf("scores add takes one of --value and --string")
	}

	request := map[string]any{"name": name}
	addSomeBody(request, "trace_id", trace)
	addSomeBody(request, "observation_id", observation)
	addSomeBody(request, "session_id", session)
	addSomeBody(request, "data_type", dataType)
	addSomeBody(request, "comment", comment)
	addSomeBody(request, "id", id)
	if given["value"] {
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return usageErrorf("--value takes a number, got %q", value)
		}
		request["value"] = number
	} else {
		request["string_value"] = stringValue
	}
	// Everything else the API checks — a target, the name's length, the
	// type's own rules, the name's config — is checked where it is decided,
	// which is inside the write (#1).
	body, err := r.api.Post(ctx, "/api/v1/scores", request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	written, err := decode[struct {
		IDs []string `json:"ids"`
	}](body)
	if err != nil {
		return err
	}
	if len(written.IDs) == 0 {
		return fmt.Errorf("the server wrote the score and named no id")
	}
	fmt.Fprintln(r.opt.Stdout, written.IDs[0])
	return nil
}

// scoresRemove retracts one score by id (spec 022 #6). No echo: a re-`add`
// with the same `--id` puts the row back.
func (r *run) scoresRemove(ctx context.Context, args []string) error {
	fs := r.flags("scores rm")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	body, err := r.api.Send(ctx, "DELETE", "/api/v1/scores/"+url.PathEscape(rest[0]), nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	fmt.Fprintf(r.opt.Stdout, "removed the score %s\n", rest[0])
	return nil
}

func (r *run) scoresList(ctx context.Context, args []string) error {
	var (
		trace       string
		observation string
		session     string
		name        string
		dataType    string
		since       string
		cursor      string
		limit       int
	)
	fs := r.flags("scores ls")
	fs.StringVar(&trace, "trace", "", "")
	fs.StringVar(&observation, "observation", "", "")
	fs.StringVar(&session, "session", "", "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&dataType, "type", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query := url.Values{}
	addSome(query, "trace_id", trace)
	addSome(query, "observation_id", observation)
	addSome(query, "session_id", session)
	addSome(query, "name", name)
	addSome(query, "data_type", dataType)
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	from, err := r.instant("--since", since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/scores", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Scores []struct {
			ID          string   `json:"id"`
			TraceID     string   `json:"trace_id"`
			Name        string   `json:"name"`
			DataType    string   `json:"data_type"`
			Value       *float64 `json:"value"`
			StringValue *string  `json:"string_value"`
			Comment     string   `json:"comment"`
			Timestamp   string   `json:"timestamp"`
		} `json:"scores"`
		NextCursor *string `json:"next_cursor"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Scores) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no scores")
		return nil
	}
	t := newTable(r.opt.Stdout, "TIME", "NAME", "VALUE", "TYPE", "TRACE", "COMMENT")
	for _, score := range listing.Scores {
		value := "-"
		switch {
		case score.Value != nil:
			value = strconv.FormatFloat(*score.Value, 'g', -1, 64)
		case score.StringValue != nil:
			value = *score.StringValue
		}
		t.row(shortTime(score.Timestamp), score.Name, value, score.DataType,
			orDash(score.TraceID), orDash(score.Comment))
	}
	t.flush()
	// `older`, because this listing is newest first and its cursor is a
	// timestamp — but only the one line: the endpoint has no `direction`, so
	// there is no far end to jump to and no page above to come back to.
	walkOn(r, "older", listing.NextCursor, nil)
	return nil
}

func (r *run) prompts(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.promptsList(ctx, rest)
	case "get":
		return r.promptsGet(ctx, rest)
	case "push":
		return r.promptsPush(ctx, rest)
	case "diff":
		return r.promptsDiff(ctx, rest)
	case "label":
		return r.promptsLabel(ctx, rest)
	case "rm":
		return r.promptsRemove(ctx, rest)
	}
	return usageErrorf("prompts takes ls, get, push, diff, label or rm, got %q", sub)
}

func (r *run) promptsList(ctx context.Context, args []string) error {
	var (
		cursor string
		limit  int
	)
	fs := r.flags("prompts ls")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query := url.Values{}
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/prompts", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Prompts []struct {
			Name          string         `json:"name"`
			Type          string         `json:"type"`
			LatestVersion int            `json:"latest_version"`
			Labels        map[string]int `json:"labels"`
			UpdatedAt     string         `json:"updated_at"`
		} `json:"prompts"`
		NextCursor *string `json:"next_cursor"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Prompts) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no prompts")
		return nil
	}
	t := newTable(r.opt.Stdout, "NAME", "TYPE", "LATEST", "LABELS", "UPDATED")
	for _, prompt := range listing.Prompts {
		var labels []string
		for label, version := range prompt.Labels {
			labels = append(labels, fmt.Sprintf("%s=%d", label, version))
		}
		sortStrings(labels)
		t.row(prompt.Name, prompt.Type, strconv.Itoa(prompt.LatestVersion),
			orDash(strings.Join(labels, " ")), shortTime(prompt.UpdatedAt))
	}
	t.flush()
	// `more`, not `older`: this listing is alphabetical by name, and it walks
	// one way only — the endpoint has no `direction`, so there is no page to
	// come back up to and nothing true to say about time.
	walkOn(r, "more", listing.NextCursor, nil)
	return nil
}

func (r *run) promptsGet(ctx context.Context, args []string) error {
	var (
		label   string
		version int
	)
	fs := r.flags("prompts get")
	fs.StringVar(&label, "label", "", "")
	fs.IntVar(&version, "version", 0, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if label != "" && version != 0 {
		return usageErrorf("a prompt is fetched by --label or by --version, not both")
	}
	query := url.Values{}
	addSome(query, "label", label)
	if version != 0 {
		query.Set("version", strconv.Itoa(version))
	}

	body, err := r.api.Get(ctx, "/api/v1/prompts/"+url.PathEscape(rest[0]), query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	prompt, err := decode[struct {
		Name          string          `json:"name"`
		Version       int             `json:"version"`
		Type          string          `json:"type"`
		Prompt        json.RawMessage `json:"prompt"`
		Config        json.RawMessage `json:"config"`
		CommitMessage string          `json:"commit_message"`
		Labels        []string        `json:"labels"`
		CreatedAt     string          `json:"created_at"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s version %d (%s)\n", prompt.Name, prompt.Version, prompt.Type)
	if len(prompt.Labels) > 0 {
		fmt.Fprintf(r.opt.Stdout, "labels: %s\n", strings.Join(prompt.Labels, ", "))
	}
	if prompt.CommitMessage != "" {
		fmt.Fprintf(r.opt.Stdout, "message: %s\n", prompt.CommitMessage)
	}
	fmt.Fprintf(r.opt.Stdout, "created: %s\n\n", shortTime(prompt.CreatedAt))
	fmt.Fprintf(r.opt.Stdout, "%s\n", indented(prompt.Prompt))
	if len(prompt.Config) > 0 {
		fmt.Fprintf(r.opt.Stdout, "\nconfig:\n%s\n", indented(prompt.Config))
	}
	return nil
}

// promptsPush publishes a new version from a file. The file carries the whole
// request body — prompt, config, type — because a prompt is JSON and a shell
// is a bad place to quote it.
func (r *run) promptsPush(ctx context.Context, args []string) error {
	var (
		file    string
		label   string
		message string
		// -1 is "not given": 0 is a real expectation, and it means "I
		// believe this name is new" (spec 021 #14).
		expect int
	)
	fs := r.flags("prompts push")
	fs.StringVar(&file, "file", "", "")
	fs.StringVar(&label, "label", "", "")
	fs.StringVar(&message, "message", "", "")
	fs.IntVar(&expect, "expect", -1, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if file == "" {
		return usageErrorf("prompts push needs --file with the version's JSON body")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", file, err)
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", file, err)
	}
	// The flags are conveniences over fields the body already has; the
	// file wins, so a script that sets both is not silently overruled.
	if label != "" {
		if _, given := request["labels"]; !given {
			request["labels"] = []string{label}
		}
	}
	if message != "" {
		if _, given := request["commit_message"]; !given {
			request["commit_message"] = message
		}
	}
	// The push a script means: "add a version to the name as I last saw it".
	// A mismatch is a `409` naming the version it is actually at, rather than
	// a silent append onto somebody else's work (spec 021 #14).
	if expect >= 0 {
		if _, given := request["expect_version"]; !given {
			request["expect_version"] = expect
		}
	}

	body, err := r.api.Post(ctx, "/api/v1/prompts/"+url.PathEscape(rest[0])+"/versions", request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	created, err := decode[struct {
		Name    string   `json:"name"`
		Version int      `json:"version"`
		Labels  []string `json:"labels"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s version %d created", created.Name, created.Version)
	if len(created.Labels) > 0 {
		fmt.Fprintf(r.opt.Stdout, " (%s)", strings.Join(created.Labels, ", "))
	}
	fmt.Fprintln(r.opt.Stdout)
	return nil
}

func (r *run) promptsDiff(ctx context.Context, args []string) error {
	var from, to int
	fs := r.flags("prompts diff")
	fs.IntVar(&from, "from", 0, "")
	fs.IntVar(&to, "to", 0, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if from < 1 || to < 1 {
		return usageErrorf("prompts diff needs --from and --to, both positive version numbers")
	}
	query := url.Values{
		"from": []string{strconv.Itoa(from)},
		"to":   []string{strconv.Itoa(to)},
	}

	body, err := r.api.Get(ctx, "/api/v1/prompts/"+url.PathEscape(rest[0])+"/diff", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	result, err := decode[struct {
		Name string `json:"name"`
		Diff string `json:"diff"`
	}](body)
	if err != nil {
		return err
	}
	if result.Diff == "" {
		fmt.Fprintf(r.opt.Stdout, "%s versions %d and %d are identical\n", result.Name, from, to)
		return nil
	}
	fmt.Fprint(r.opt.Stdout, result.Diff)
	return nil
}

// promptsLabel moves a label onto a version or retires it (spec 021 #8).
// `push --label` covered creation-time labels only; promoting an already
// published version — and rolling back to it — had no command, though it is
// the deploy path the whole design rests on (spec 003 #12).
func (r *run) promptsLabel(ctx context.Context, args []string) error {
	var (
		version int
		remove  bool
	)
	fs := r.flags("prompts label")
	fs.IntVar(&version, "version", 0, "")
	fs.BoolVar(&remove, "rm", false, "")
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}
	if remove == (version > 0) {
		return usageErrorf("prompts label takes --version N or --rm, not both and not neither")
	}
	name, label := rest[0], rest[1]
	path := "/api/v1/prompts/" + url.PathEscape(name) + "/labels/" + url.PathEscape(label)

	var body json.RawMessage
	if remove {
		body, err = r.api.Send(ctx, "DELETE", path, nil, nil)
	} else {
		body, err = r.api.Send(ctx, "PUT", path, nil, map[string]any{"version": version})
	}
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	answer, err := decode[labelObject](body)
	if err != nil {
		return err
	}
	// Both answers name the version: a move names where the label landed, a
	// removal where it had been — which is the number a rollback needs.
	if remove {
		fmt.Fprintf(r.opt.Stdout, "%s: %s removed from version %d\n", name, answer.Label, answer.Version)
		return nil
	}
	fmt.Fprintf(r.opt.Stdout, "%s: %s now points at version %d\n", name, answer.Label, answer.Version)
	return nil
}

// labelObject is what both label endpoints answer with.
type labelObject struct {
	Label   string `json:"label"`
	Version int    `json:"version"`
}

// promptsRemove deletes a name whole, wearing spec 005's ceremony: the
// server's dry run, then the name typed back (spec 021 #7, #8).
func (r *run) promptsRemove(ctx context.Context, args []string) error {
	var yes bool
	fs := r.flags("prompts rm")
	fs.BoolVar(&yes, "yes", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	name := rest[0]
	body, err := r.destructive(ctx, "DELETE", "/api/v1/prompts/"+url.PathEscape(name),
		nil, nil, yes, "delete prompt "+name+" with every version and label")
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	deleted, err := decode[struct {
		Name        string `json:"name"`
		WouldDelete struct {
			Versions int `json:"versions"`
			Labels   int `json:"labels"`
		} `json:"would_delete"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "deleted %s: %s and %s gone\n", deleted.Name,
		plural(deleted.WouldDelete.Versions, "version"), plural(deleted.WouldDelete.Labels, "label"))
	return nil
}

func (r *run) stats(ctx context.Context, args []string) error {
	var (
		groupBy     string
		since       string
		until       string
		environment string
		user        string
	)
	fs := r.flags("stats")
	fs.StringVar(&groupBy, "group-by", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	fs.StringVar(&environment, "env", "", "")
	// `--user`, spelled as `traces ls` and `sessions ls` spell it (spec 007
	// #11): the same question about one end user's traffic (spec 023 #7).
	fs.StringVar(&user, "user", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query := url.Values{}
	addSome(query, "group_by", groupBy)
	addSome(query, "environment", environment)
	addSome(query, "user_id", user)
	from, err := r.instant("--since", since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	to, err := r.instant("--until", until)
	if err != nil {
		return err
	}
	addSome(query, "to", to)

	body, err := r.api.Get(ctx, "/api/v1/stats", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	result, err := decode[struct {
		GroupBy string `json:"group_by"`
		Unit    string `json:"unit"`
		Buckets []struct {
			Key        string   `json:"key"`
			Count      int      `json:"count"`
			ErrorCount int      `json:"error_count"`
			TotalCost  *float64 `json:"total_cost"`
			// Sessions rides only a `--user` timeline (spec 023 #6), so
			// the column appears only when the answer carries it.
			Sessions *int `json:"sessions"`
			// Tokens is absent when nothing in the bucket reported usage,
			// and each key when nothing reported that class (spec 031 #4).
			Tokens *struct {
				Input  *int64 `json:"input"`
				Output *int64 `json:"output"`
			} `json:"tokens"`
			LatencyMs struct {
				P50 *int64 `json:"p50"`
				P95 *int64 `json:"p95"`
			} `json:"latency_ms"`
		} `json:"buckets"`
	}](body)
	if err != nil {
		return err
	}
	if len(result.Buckets) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no data in this range")
		return nil
	}
	// The header names the unit, because a count of traces and a count of
	// observations are not comparable (spec 004 Decision 23).
	headers := []string{strings.ToUpper(result.GroupBy), strings.ToUpper(result.Unit) + "S"}
	sessions := result.Buckets[0].Sessions != nil
	if sessions {
		headers = append(headers, "SESSIONS")
	}
	// TOKENS is input plus output — what a bill is made of — and a dash
	// when the bucket reported neither, the way COST is a dash when nothing
	// in it was priced (spec 031 #13). Cache read is not in it: a provider
	// that reports cached tokens inside the input would be counted twice.
	t := newTable(r.opt.Stdout, append(headers, "ERRORS", "COST", "TOKENS", "P50", "P95")...)
	for _, bucket := range result.Buckets {
		cells := []string{bucket.Key, strconv.Itoa(bucket.Count)}
		if sessions {
			cells = append(cells, strconv.Itoa(deref(bucket.Sessions)))
		}
		tokens := "-"
		if bucket.Tokens != nil && (bucket.Tokens.Input != nil || bucket.Tokens.Output != nil) {
			var billed int64
			if bucket.Tokens.Input != nil {
				billed += *bucket.Tokens.Input
			}
			if bucket.Tokens.Output != nil {
				billed += *bucket.Tokens.Output
			}
			tokens = strconv.FormatInt(billed, 10)
		}
		t.row(append(cells, strconv.Itoa(bucket.ErrorCount), cost(bucket.TotalCost), tokens,
			duration(bucket.LatencyMs.P50), duration(bucket.LatencyMs.P95))...)
	}
	t.flush()
	return nil
}

// facets is `GET /api/v1/facets` and nothing more (#1): what the three
// many-valued filters can be set to, over a range (spec 027 #5).
//
// It is a command of its own rather than a flag on `stats` because it answers a
// different question — not "how much traffic" but "what is there to ask about"
// — and because it is what a person reaches for before writing `--env`. The
// window flags are spelled as every other command spells them (spec 007 #11).
func (r *run) facets(ctx context.Context, args []string) error {
	var since, until string
	fs := r.flags("facets")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	query := url.Values{}
	from, err := r.instant("--since", since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	to, err := r.instant("--until", until)
	if err != nil {
		return err
	}
	addSome(query, "to", to)

	body, err := r.api.Get(ctx, "/api/v1/facets", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	type facetValue struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	}
	result, err := decode[struct {
		From        string       `json:"from"`
		To          string       `json:"to"`
		Environment []facetValue `json:"environment"`
		Release     []facetValue `json:"release"`
		Name        []facetValue `json:"name"`
		Omitted     struct {
			Environment int `json:"environment"`
			Release     int `json:"release"`
			Name        int `json:"name"`
		} `json:"omitted"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s .. %s\n", shortTime(result.From), shortTime(result.To))
	// One block per column, in the order the answer lists them and the
	// filter bar shows them. A column with nothing in it says so rather
	// than printing a header over an empty table.
	for _, column := range []struct {
		flag    string
		values  []facetValue
		omitted int
	}{
		{"--env", result.Environment, result.Omitted.Environment},
		{"--release", result.Release, result.Omitted.Release},
		{"--name", result.Name, result.Omitted.Name},
	} {
		fmt.Fprintln(r.opt.Stdout)
		if len(column.values) == 0 {
			fmt.Fprintf(r.opt.Stdout, "%s: nothing in this range\n", column.flag)
			continue
		}
		t := newTable(r.opt.Stdout, strings.ToUpper(strings.TrimPrefix(column.flag, "--")), "TRACES")
		for _, value := range column.values {
			t.row(value.Value, strconv.Itoa(value.Count))
		}
		t.flush()
		// A truncated list that said nothing about it would be a wrong
		// one (spec 027 #2). The busiest values are the ones shown.
		if column.omitted > 0 {
			fmt.Fprintf(r.opt.Stdout, "and %s not shown; narrow the range to see them\n",
				plural(column.omitted, "rarer value"))
		}
	}
	return nil
}

func deref(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// health is the liveness probe (spec 020 #4): `GET /health`, which is the one
// route on the server that authenticates nothing, so this is the one command
// that needs no key. That is what makes it usable where a probe lives — a
// container's HEALTHCHECK, a systemd unit, a load balancer — none of which
// should hold a project secret to learn whether a process is up.
//
// It is also the only probe the image can have: a distroless runtime has no
// curl, no wget and no shell to write one in, so the binary is the probe
// (spec 020 #2).
func (r *run) health(ctx context.Context, args []string) error {
	// Where to probe, when nobody said. Every other command asks a server
	// somewhere else; this one is usually asking about the process beside
	// it, and in the image that process's port is `TRACEPAD_LISTEN` —
	// which the probe would otherwise not read, so `-e TRACEPAD_LISTEN=:8080`
	// would produce a healthy server marked unhealthy for ever (#15).
	//
	// Before the flag set is built, because that captures this as `--url`'s
	// default; `--url` and `TRACEPAD_URL` still win, in that order.
	if r.opt.Env("TRACEPAD_URL") == "" {
		if target := listenTarget(r.opt.Env("TRACEPAD_LISTEN")); target != "" {
			r.url = target
		}
	}
	fs := r.flags("health")
	if _, err := r.parseUnauthenticated(fs, args, 0); err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/health", nil)
	if err != nil {
		return err
	}
	answer, err := decode[struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}](body)
	if err != nil {
		return err
	}
	// A 200 from something that is not this server — a proxy's splash page,
	// a different service on the port — is not health. The version is what
	// makes the answer this server's, so its absence is a failure and not
	// an empty column.
	if answer.Version == "" {
		return fmt.Errorf("%s answered /health without a version", r.url)
	}
	if r.wantJSON() {
		// The one place the CLI does not pass the server's bytes through
		// (#1): what this command reports is its own verdict, and the
		// verdict is the exit code. The JSON restates it in a field so
		// that a script reading stdout does not have to know which
		// spellings of `status` count as healthy.
		out, err := json.Marshal(struct {
			Version string `json:"version"`
			OK      bool   `json:"ok"`
		}{Version: answer.Version, OK: true})
		if err != nil {
			return err
		}
		return r.emit(out)
	}
	fmt.Fprintln(r.opt.Stdout, answer.Version)
	return nil
}

// listenTarget turns a server's listen address into a URL a probe on the same
// host can reach, or "" if it is not one it can make sense of.
//
// A wildcard bind is not a connectable address, so it becomes loopback —
// 127.0.0.1 rather than `localhost`, because the probe should not depend on
// what a container's resolver thinks that name means, or on which of IPv4 and
// IPv6 it answers with first when the server bound only one of them.
func listenTarget(listen string) string {
	if listen == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		// Not an address this can read. The default stands, and the
		// server will have its own complaint about it.
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (r *run) system(ctx context.Context, args []string) error {
	fs := r.flags("system")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/system", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	info, err := decode[struct {
		Version       string `json:"version"`
		GoVersion     string `json:"go_version"`
		StartedAt     string `json:"started_at"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		Database      struct {
			SizeBytes int64            `json:"size_bytes"`
			Rows      map[string]int64 `json:"rows"`
		} `json:"database"`
		WriterQueue struct {
			Waiting  int `json:"waiting"`
			Capacity int `json:"capacity"`
		} `json:"writer_queue"`
		MCP struct {
			Enabled bool   `json:"enabled"`
			Path    string `json:"path"`
		} `json:"mcp"`
		Raw struct {
			Enabled            bool    `json:"enabled"`
			Batches            int64   `json:"batches"`
			Bytes              int64   `json:"bytes"`
			OldestReceivedAt   *string `json:"oldest_received_at"`
			NewestReceivedAt   *string `json:"newest_received_at"`
			TracesBeforeWindow int64   `json:"traces_before_window"`
		} `json:"raw"`
		Counters struct {
			Since    string `json:"since"`
			Dialects map[string]struct {
				Batches      int64 `json:"batches"`
				SpansStored  int64 `json:"spans_stored"`
				SpansSkipped int64 `json:"spans_skipped"`
			} `json:"dialects"`
			RejectedBatches int64    `json:"rejected_batches"`
			SDKVersions     []string `json:"langfuse_ingestion_versions"`
		} `json:"counters"`
	}](body)
	if err != nil {
		return err
	}

	fmt.Fprintf(r.opt.Stdout, "tracepad %s (%s), client %s\n", info.Version, info.GoVersion, r.opt.Version)
	fmt.Fprintf(r.opt.Stdout, "  up since   %s (%s)\n", shortTime(info.StartedAt), uptime(info.UptimeSeconds))
	fmt.Fprintf(r.opt.Stdout, "  database   %s\n", byteSize(int(info.Database.SizeBytes)))
	fmt.Fprintf(r.opt.Stdout, "  writes     %d of %d queued\n",
		info.WriterQueue.Waiting, info.WriterQueue.Capacity)
	if info.MCP.Path != "" {
		fmt.Fprintf(r.opt.Stdout, "  mcp        %s at %s\n", enabled(info.MCP.Enabled), info.MCP.Path)
	}

	// This project's rows, not the deployment's (spec 004 Decision 33);
	// `projects` is the exception and is only a count of tenants.
	fmt.Fprintln(r.opt.Stdout, "\nrows in this project")
	rows := newTable(r.opt.Stdout)
	for _, table := range []string{"traces", "observations", "raw_batches",
		"scores", "prompts", "prompt_labels", "api_keys", "projects"} {
		if count, known := info.Database.Rows[table]; known {
			rows.row("  "+table, strconv.FormatInt(count, 10))
		}
	}
	rows.flush()

	// The archive, which is what `export --otlp` can carry out (spec 019
	// #4). On disk rather than since start, unlike the counters below, so
	// it reads beside the row counts rather than under them.
	fmt.Fprintln(r.opt.Stdout, "\nraw archive in this project")
	archive := newTable(r.opt.Stdout)
	archive.row("  storage", enabled(info.Raw.Enabled))
	archive.row("  batches", strconv.FormatInt(info.Raw.Batches, 10))
	archive.row("  on disk", byteSize(int(info.Raw.Bytes)))
	if info.Raw.OldestReceivedAt != nil && info.Raw.NewestReceivedAt != nil {
		archive.row("  covering", shortTime(*info.Raw.OldestReceivedAt)+
			" .. "+shortTime(*info.Raw.NewestReceivedAt))
	}
	// Named as the edge of the promise rather than as a failure: these
	// traces are parsed rows only, and an export says so rather than
	// letting the receiver's gap say it (spec 019 #1).
	archive.row("  not covered", fmt.Sprintf("%d traces started before it begins",
		info.Raw.TracesBeforeWindow))
	archive.flush()

	fmt.Fprintf(r.opt.Stdout, "\ningest in this project since %s\n", shortTime(info.Counters.Since))
	if len(info.Counters.Dialects) == 0 {
		fmt.Fprintln(r.opt.Stdout, "  nothing has been exported to this project yet")
	} else {
		counters := newTable(r.opt.Stdout, "  DIALECT", "BATCHES", "SPANS", "SKIPPED")
		names := make([]string, 0, len(info.Counters.Dialects))
		for name := range info.Counters.Dialects {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			entry := info.Counters.Dialects[name]
			counters.row("  "+name, strconv.FormatInt(entry.Batches, 10),
				strconv.FormatInt(entry.SpansStored, 10),
				strconv.FormatInt(entry.SpansSkipped, 10))
		}
		counters.flush()
	}
	if info.Counters.RejectedBatches > 0 {
		fmt.Fprintf(r.opt.Stdout, "  %d bodies could not be decoded at all\n", info.Counters.RejectedBatches)
	}
	if len(info.Counters.SDKVersions) > 0 {
		fmt.Fprintf(r.opt.Stdout, "  langfuse SDK versions seen: %s\n",
			strings.Join(info.Counters.SDKVersions, ", "))
	}
	return nil
}

func uptime(seconds int64) string {
	switch {
	case seconds >= 86400:
		return fmt.Sprintf("%dd%dh", seconds/86400, (seconds%86400)/3600)
	case seconds >= 3600:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	case seconds >= 60:
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}

func enabled(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// indented renders stored JSON the way a person reads it.
func indented(raw json.RawMessage) string {
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}
