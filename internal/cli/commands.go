package cli

import (
	"context"
	"encoding/json"
	"fmt"
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
	if sub != "show" {
		return usageErrorf("sessions takes show, got %q", sub)
	}
	var limit int
	fs := r.flags("sessions show")
	fs.IntVar(&limit, "limit", 0, "")
	positional, err := r.parse(fs, rest, 1)
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
	renderTraceTable(r.opt.Stdout, session.Traces)
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
		limit       int
	)
	fs := r.flags("scores ls")
	fs.StringVar(&trace, "trace", "", "")
	fs.StringVar(&observation, "observation", "", "")
	fs.StringVar(&session, "session", "", "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&dataType, "type", "", "")
	fs.StringVar(&since, "since", "", "")
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
	from, err := r.since(since)
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
	var limit int
	fs := r.flags("prompts ls")
	fs.IntVar(&limit, "limit", 0, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query := url.Values{}
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
	from, err := r.since(since)
	if err != nil {
		return err
	}
	addSome(query, "from", from)
	to, err := r.since(until)
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

	fmt.Fprintln(r.opt.Stdout, "\nrows")
	rows := newTable(r.opt.Stdout)
	for _, table := range []string{"traces", "observations", "payloads", "raw_batches",
		"scores", "prompts", "prompt_labels", "projects", "api_keys"} {
		if count, known := info.Database.Rows[table]; known {
			rows.row("  "+table, strconv.FormatInt(count, 10))
		}
	}
	rows.flush()

	fmt.Fprintf(r.opt.Stdout, "\ningest since %s\n", shortTime(info.Counters.Since))
	if len(info.Counters.Dialects) == 0 {
		fmt.Fprintln(r.opt.Stdout, "  nothing has been exported to this server yet")
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
