package cli

import (
	"context"
	"encoding/json"
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

func (r *run) scores(ctx context.Context, args []string) error {
	sub, rest := split(args)
	if sub != "ls" {
		return usageErrorf("scores takes ls, got %q", sub)
	}
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
	if _, err := r.parse(fs, rest, 0); err != nil {
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
	}
	return usageErrorf("prompts takes ls, get, push or diff, got %q", sub)
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
	)
	fs := r.flags("prompts push")
	fs.StringVar(&file, "file", "", "")
	fs.StringVar(&label, "label", "", "")
	fs.StringVar(&message, "message", "", "")
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

func (r *run) stats(ctx context.Context, args []string) error {
	var (
		groupBy     string
		since       string
		until       string
		environment string
	)
	fs := r.flags("stats")
	fs.StringVar(&groupBy, "group-by", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	fs.StringVar(&environment, "env", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query := url.Values{}
	addSome(query, "group_by", groupBy)
	addSome(query, "environment", environment)
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
			LatencyMs  struct {
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
	t := newTable(r.opt.Stdout,
		strings.ToUpper(result.GroupBy), strings.ToUpper(result.Unit)+"S", "ERRORS", "COST", "P50", "P95")
	for _, bucket := range result.Buckets {
		t.row(bucket.Key, strconv.Itoa(bucket.Count), strconv.Itoa(bucket.ErrorCount),
			cost(bucket.TotalCost), duration(bucket.LatencyMs.P50), duration(bucket.LatencyMs.P95))
	}
	t.flush()
	return nil
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
