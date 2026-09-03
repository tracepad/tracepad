package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// The eval commands (spec 014, CLI contract): `datasets`, `runs` and
// `score-configs`. Between them they are the whole loop a harness needs —
// declare the configs, push the cases, open the run, stamp the traces, post
// the scores, close the run, compare — with the server doing every piece of
// arithmetic (spec 004 #1).

func (r *run) datasets(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.datasetsList(ctx, rest)
	case "show":
		return r.datasetsShow(ctx, rest)
	case "push":
		return r.datasetsPush(ctx, rest)
	case "rm-item":
		return r.datasetsRemoveItem(ctx, rest)
	case "rm":
		return r.datasetsRemove(ctx, rest)
	}
	return usageErrorf("datasets takes ls, show, push, rm-item or rm, got %q", sub)
}

func (r *run) datasetsList(ctx context.Context, args []string) error {
	var (
		cursor string
		limit  int
	)
	fs := r.flags("datasets ls")
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

	body, err := r.api.Get(ctx, "/api/v1/datasets", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Datasets []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Version     int    `json:"version"`
			ItemCount   int64  `json:"item_count"`
			RunCount    int64  `json:"run_count"`
			UpdatedAt   string `json:"updated_at"`
		} `json:"datasets"`
		NextCursor *string `json:"next_cursor"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Datasets) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no datasets")
		return nil
	}
	t := newTable(r.opt.Stdout, "NAME", "ITEMS", "RUNS", "VERSION", "UPDATED", "DESCRIPTION")
	for _, dataset := range listing.Datasets {
		t.row(dataset.Name,
			strconv.FormatInt(dataset.ItemCount, 10),
			strconv.FormatInt(dataset.RunCount, 10),
			strconv.Itoa(dataset.Version),
			shortTime(dataset.UpdatedAt),
			orDash(dataset.Description))
	}
	t.flush()
	// Alphabetical and one-way, like the prompts listing: there is no page
	// above the first, so nothing true to say about going back.
	walkOn(r, "more", listing.NextCursor, nil)
	return nil
}

// datasetItem is one case as the item endpoints render it.
type datasetItem struct {
	ID             string          `json:"id"`
	Seq            int64           `json:"seq"`
	Version        int             `json:"version"`
	Input          json.RawMessage `json:"input"`
	ExpectedOutput json.RawMessage `json:"expected_output"`
	Metadata       json.RawMessage `json:"metadata"`
}

type itemPage struct {
	Dataset    string        `json:"dataset"`
	Version    int           `json:"version"`
	Items      []datasetItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func (r *run) datasetsShow(ctx context.Context, args []string) error {
	var (
		version int
		limit   int
		cursor  string
	)
	fs := r.flags("datasets show")
	fs.IntVar(&version, "version", 0, "")
	fs.IntVar(&limit, "limit", 0, "")
	fs.StringVar(&cursor, "cursor", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	name := rest[0]
	query := url.Values{}
	if err := addLimit(query, limit); err != nil {
		return err
	}
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if fs.Lookup("version").Value.String() != "0" || version != 0 {
		query.Set("version", strconv.Itoa(version))
	}

	// In JSON mode the command is an export, so it walks to the end: a
	// dataset larger than one page that came back as one page would be a
	// file that looks complete and is not (the same trap the docs' harness
	// recipe steps around).
	if r.wantJSON() {
		return r.exportDataset(ctx, name, query)
	}

	dataset, err := r.api.Get(ctx, "/api/v1/datasets/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	envelope, err := decode[struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     int    `json:"version"`
		ItemCount   int64  `json:"item_count"`
		RunCount    int64  `json:"run_count"`
	}](dataset)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/datasets/"+url.PathEscape(name)+"/items", query)
	if err != nil {
		return err
	}
	page, err := decode[itemPage](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s at version %d: %d items, %d runs\n",
		envelope.Name, page.Version, envelope.ItemCount, envelope.RunCount)
	if envelope.Description != "" {
		fmt.Fprintf(r.opt.Stdout, "%s\n", envelope.Description)
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(r.opt.Stdout, "\nno items")
		return nil
	}
	fmt.Fprintln(r.opt.Stdout)
	t := newTable(r.opt.Stdout, "ID", "SEQ", "VERSION", "INPUT", "EXPECTED")
	for _, item := range page.Items {
		t.row(item.ID, strconv.FormatInt(item.Seq, 10), strconv.Itoa(item.Version),
			compactJSON(item.Input), compactJSON(item.ExpectedOutput))
	}
	t.flush()
	walkOn(r, "more", page.NextCursor, nil)
	return nil
}

// exportDataset walks every page of the items and emits one document. It is
// the export the spec points at when it says a dataset needs no separate one.
func (r *run) exportDataset(ctx context.Context, name string, query url.Values) error {
	path := "/api/v1/datasets/" + url.PathEscape(name) + "/items"
	page := url.Values{}
	for key, values := range query {
		page[key] = values
	}
	// The largest page the endpoint allows, so a big dataset costs the
	// fewest round trips rather than the most.
	if page.Get("limit") == "" {
		page.Set("limit", "500")
	}
	var (
		items   []datasetItem
		version int
	)
	for {
		body, err := r.api.Get(ctx, path, page)
		if err != nil {
			return err
		}
		decoded, err := decode[itemPage](body)
		if err != nil {
			return err
		}
		version = decoded.Version
		items = append(items, decoded.Items...)
		if decoded.NextCursor == nil {
			break
		}
		page.Set("cursor", *decoded.NextCursor)
		// Every page after the first must resolve the same version, or
		// an edit mid-walk would stitch two datasets into one file.
		page.Set("version", strconv.Itoa(version))
	}
	if items == nil {
		items = []datasetItem{}
	}
	encoded, err := json.Marshal(map[string]any{
		"dataset": name, "version": version, "items": items,
	})
	if err != nil {
		return err
	}
	return r.emit(encoded)
}

func (r *run) datasetsPush(ctx context.Context, args []string) error {
	var (
		file        string
		description string
	)
	fs := r.flags("datasets push")
	fs.StringVar(&file, "file", "", "")
	fs.StringVar(&description, "description", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if file == "" {
		return usageErrorf("datasets push needs --file with the cases, as JSONL or a JSON array")
	}
	items, err := readCases(file)
	if err != nil {
		return err
	}
	name := rest[0]
	if description != "" {
		if _, err := r.api.Send(ctx, "PUT", "/api/v1/datasets/"+url.PathEscape(name), nil,
			map[string]any{"description": description}); err != nil {
			return err
		}
	}

	// One POST for the batch, because the version clock ticks once for it:
	// a file sent item by item would leave a version per case and no way to
	// name the state the file describes (#5).
	body, err := r.api.Post(ctx, "/api/v1/datasets/"+url.PathEscape(name)+"/items", items)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	written, err := decode[struct {
		IDs     []string `json:"ids"`
		Version int      `json:"version"`
		Changed int      `json:"changed"`
	}](body)
	if err != nil {
		return err
	}
	if written.Changed == 0 {
		fmt.Fprintf(r.opt.Stdout, "unchanged at version %d\n", written.Version)
		return nil
	}
	fmt.Fprintf(r.opt.Stdout, "version %d: %s changed, %d in the batch\n",
		written.Version, plural(written.Changed, "item"), len(written.IDs))
	return nil
}

// readCases reads a `.jsonl` file — one case per line — or a `.json` array.
// Both are the same batch on the wire; the two shapes exist because a dataset
// is edited by hand in one and generated by a script in the other.
func readCases(path string) ([]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if strings.HasSuffix(path, ".jsonl") {
		var items []json.RawMessage
		for number, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item json.RawMessage
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				return nil, fmt.Errorf("%s line %d is not JSON: %w", path, number+1, err)
			}
			items = append(items, item)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("%s has no cases in it", path)
		}
		return items, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s is not a JSON array of cases: %w", path, err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s has no cases in it", path)
	}
	return items, nil
}

func (r *run) datasetsRemoveItem(ctx context.Context, args []string) error {
	fs := r.flags("datasets rm-item")
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}
	body, err := r.api.Send(ctx, "DELETE",
		"/api/v1/datasets/"+url.PathEscape(rest[0])+"/items/"+url.PathEscape(rest[1]), nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	archived, err := decode[struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}](body)
	if err != nil {
		return err
	}
	// "archived", not "deleted": the row is still readable at every earlier
	// version, which is the whole point of the append-only history (#5).
	fmt.Fprintf(r.opt.Stdout, "archived %s at version %d\n", archived.ID, archived.Version)
	return nil
}

func (r *run) datasetsRemove(ctx context.Context, args []string) error {
	var yes bool
	fs := r.flags("datasets rm")
	fs.BoolVar(&yes, "yes", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	name := rest[0]
	body, err := r.destructive(ctx, "DELETE", "/api/v1/datasets/"+url.PathEscape(name),
		nil, nil, yes, "delete dataset "+name+" with its items and runs")
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	deleted, err := decode[struct {
		Dataset      string `json:"dataset"`
		Items        int64  `json:"items"`
		Runs         int64  `json:"runs"`
		PinnedTraces int64  `json:"pinned_traces"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "deleted %s: %s and %s gone, %s released to the retention window\n",
		deleted.Dataset, plural(int(deleted.Items), "item"), plural(int(deleted.Runs), "run"),
		plural(int(deleted.PinnedTraces), "trace"))
	return nil
}

func (r *run) runs(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.runsList(ctx, rest)
	case "create":
		return r.runsCreate(ctx, rest)
	case "show":
		return r.runsShow(ctx, rest)
	case "finish":
		return r.runsFinish(ctx, rest)
	case "compare":
		return r.runsCompare(ctx, rest)
	case "rm":
		return r.runsRemove(ctx, rest)
	}
	return usageErrorf("runs takes ls, create, show, finish, compare or rm, got %q", sub)
}

// runObject is a run as every run endpoint renders it.
type runObject struct {
	ID             string          `json:"id"`
	Dataset        string          `json:"dataset"`
	DatasetVersion int             `json:"dataset_version"`
	Name           string          `json:"name"`
	Metadata       json.RawMessage `json:"metadata"`
	Status         string          `json:"status"`
	Error          string          `json:"error"`
	CreatedAt      string          `json:"created_at"`
	FinishedAt     string          `json:"finished_at"`
}

func (r *run) runsList(ctx context.Context, args []string) error {
	var (
		cursor string
		limit  int
	)
	fs := r.flags("runs ls")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	query := url.Values{}
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if err := addLimit(query, limit); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/datasets/"+url.PathEscape(rest[0])+"/runs", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Runs       []runObject `json:"runs"`
		NextCursor *string     `json:"next_cursor"`
		PrevCursor *string     `json:"prev_cursor"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Runs) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no runs")
		return nil
	}
	t := newTable(r.opt.Stdout, "ID", "NAME", "VERSION", "STATUS", "CREATED")
	for _, item := range listing.Runs {
		t.row(item.ID, orDash(item.Name), strconv.Itoa(item.DatasetVersion),
			item.Status, shortTime(item.CreatedAt))
	}
	t.flush()
	walkOn(r, "older", listing.NextCursor, listing.PrevCursor)
	return nil
}

func (r *run) runsCreate(ctx context.Context, args []string) error {
	var (
		name         string
		metadataFile string
		version      int
		id           string
	)
	fs := r.flags("runs create")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&metadataFile, "metadata-file", "", "")
	fs.IntVar(&version, "dataset-version", -1, "")
	fs.StringVar(&id, "id", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	request := map[string]any{}
	addSomeBody(request, "name", name)
	addSomeBody(request, "id", id)
	if version >= 0 {
		request["dataset_version"] = version
	}
	if metadataFile != "" {
		raw, err := os.ReadFile(metadataFile)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", metadataFile, err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return fmt.Errorf("%s is not a JSON object: %w", metadataFile, err)
		}
		request["metadata"] = metadata
	}

	body, err := r.api.Post(ctx, "/api/v1/datasets/"+url.PathEscape(rest[0])+"/runs", request)
	if err != nil {
		return err
	}
	// JSON mode answers with the whole run so that a script can read the
	// version it pinned as well as the id: `RUN=$(tracepad runs create s
	// --json | jq -r .id)` is the line the docs promise.
	if r.wantJSON() {
		return r.emit(body)
	}
	created, err := decode[runObject](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s\n", created.ID)
	fmt.Fprintf(r.opt.Stdout, "%s at version %d, %s\n",
		created.Dataset, created.DatasetVersion, created.Status)
	return nil
}

func (r *run) runsShow(ctx context.Context, args []string) error {
	var (
		items   bool
		unknown bool
		limit   int
		cursor  string
	)
	fs := r.flags("runs show")
	fs.BoolVar(&items, "items", false, "")
	fs.BoolVar(&unknown, "unknown", false, "")
	fs.IntVar(&limit, "limit", 0, "")
	fs.StringVar(&cursor, "cursor", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if items {
		return r.runItems(ctx, rest[0], unknown, limit, cursor, fs)
	}
	if unknown || limit != 0 || cursor != "" {
		return usageErrorf("--unknown, --limit and --cursor belong to `runs show --items`")
	}

	body, err := r.api.Get(ctx, "/api/v1/runs/"+url.PathEscape(rest[0]), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	return r.renderRun(body)
}

// runItems renders the item view: the cases with the attempts made at them.
func (r *run) runItems(ctx context.Context, id string, unknown bool, limit int,
	cursor string, fs *flag.FlagSet) error {
	query := url.Values{}
	if err := addLimit(query, limit); err != nil {
		return err
	}
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if unknown {
		query.Set("unknown", "true")
	}
	body, err := r.api.Get(ctx, "/api/v1/runs/"+url.PathEscape(id)+"/items", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	page, err := decode[struct {
		Items []struct {
			ID       *string `json:"id"`
			Seq      *int64  `json:"seq"`
			Unknown  bool    `json:"unknown"`
			Attempts []struct {
				TraceID    string          `json:"trace_id"`
				ErrorCount int             `json:"error_count"`
				TotalCost  *float64        `json:"total_cost"`
				LatencyMs  *int64          `json:"latency_ms"`
				Output     json.RawMessage `json:"output"`
				Scores     []struct {
					Name        string   `json:"name"`
					Value       *float64 `json:"value"`
					StringValue string   `json:"string_value"`
				} `json:"scores"`
			} `json:"attempts"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
		PrevCursor *string `json:"prev_cursor"`
	}](body)
	if err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no items")
		return nil
	}
	t := newTable(r.opt.Stdout, "ITEM", "TRACE", "LATENCY", "COST", "SCORES", "OUTPUT")
	for _, item := range page.Items {
		name := "unknown"
		if item.ID != nil {
			name = *item.ID
		}
		if item.Unknown {
			name += " (unknown)"
		}
		if len(item.Attempts) == 0 {
			// A case with no attempt is the interesting row of a
			// half-finished run, so it gets a line rather than
			// silence.
			t.row(name, "—", "—", "—", "—", "no attempt")
			continue
		}
		for _, attempt := range item.Attempts {
			var scores []string
			for _, score := range attempt.Scores {
				if score.Value != nil {
					scores = append(scores, fmt.Sprintf("%s=%s", score.Name, trimFloat(*score.Value)))
					continue
				}
				scores = append(scores, fmt.Sprintf("%s=%s", score.Name, score.StringValue))
			}
			t.row(name, attempt.TraceID,
				duration(attempt.LatencyMs), cost(attempt.TotalCost),
				orDash(strings.Join(scores, " ")), compactJSON(attempt.Output))
			name = ""
		}
	}
	t.flush()
	walkOn(r, "more", page.NextCursor, page.PrevCursor)
	return nil
}

// renderRun prints a run and its summary: the block a person reads to answer
// "how did that go".
func (r *run) renderRun(body json.RawMessage) error {
	shown, err := decode[struct {
		runObject
		Summary struct {
			Items struct {
				Total   int64 `json:"total"`
				Covered int64 `json:"covered"`
				Missing int64 `json:"missing"`
				Unknown int64 `json:"unknown"`
			} `json:"items"`
			Traces struct {
				Count       int64    `json:"count"`
				AttemptsMax int64    `json:"attempts_max"`
				ErrorCount  int64    `json:"error_count"`
				TotalCost   *float64 `json:"total_cost"`
				LatencyMs   struct {
					P50 *int64 `json:"p50"`
					P95 *int64 `json:"p95"`
				} `json:"latency_ms"`
			} `json:"traces"`
			Scores map[string]struct {
				DataType     string           `json:"data_type"`
				Direction    string           `json:"direction"`
				Count        int64            `json:"count"`
				Mean         *float64         `json:"mean"`
				Min          *float64         `json:"min"`
				Max          *float64         `json:"max"`
				Distribution map[string]int64 `json:"distribution"`
			} `json:"scores"`
			Models  []string `json:"models"`
			Prompts []struct {
				Name    string `json:"name"`
				Version *int64 `json:"version"`
			} `json:"prompts"`
		} `json:"summary"`
	}](body)
	if err != nil {
		return err
	}
	out := r.opt.Stdout
	fmt.Fprintf(out, "%s %s\n", shown.ID, orDash(shown.Name))
	fmt.Fprintf(out, "%s at version %d, %s\n", shown.Dataset, shown.DatasetVersion, shown.Status)
	if shown.Error != "" {
		fmt.Fprintf(out, "error: %s\n", shown.Error)
	}
	summary := shown.Summary
	fmt.Fprintf(out, "\nitems:  %d of %d covered, %d missing, %d unknown traces\n",
		summary.Items.Covered, summary.Items.Total, summary.Items.Missing, summary.Items.Unknown)
	fmt.Fprintf(out, "traces: %d, %d failed, up to %d attempts per item\n",
		summary.Traces.Count, summary.Traces.ErrorCount, summary.Traces.AttemptsMax)
	fmt.Fprintf(out, "cost:   %s   latency: p50 %s, p95 %s\n",
		cost(summary.Traces.TotalCost),
		duration(summary.Traces.LatencyMs.P50), duration(summary.Traces.LatencyMs.P95))
	if len(summary.Models) > 0 {
		fmt.Fprintf(out, "models: %s\n", strings.Join(summary.Models, ", "))
	}
	if len(summary.Prompts) > 0 {
		var prompts []string
		for _, prompt := range summary.Prompts {
			if prompt.Version != nil {
				prompts = append(prompts, fmt.Sprintf("%s@%d", prompt.Name, *prompt.Version))
				continue
			}
			prompts = append(prompts, prompt.Name)
		}
		fmt.Fprintf(out, "prompts: %s\n", strings.Join(prompts, ", "))
	}
	if len(summary.Scores) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	t := newTable(out, "SCORE", "TYPE", "COUNT", "MEAN", "RANGE")
	for _, name := range sortedNames(summary.Scores) {
		stat := summary.Scores[name]
		mean, span := "—", "—"
		if stat.Mean != nil {
			mean = trimFloat(*stat.Mean)
			span = trimFloat(*stat.Min) + ".." + trimFloat(*stat.Max)
		}
		if stat.Distribution != nil {
			var parts []string
			for _, value := range sortedNames(stat.Distribution) {
				parts = append(parts, fmt.Sprintf("%s=%d", value, stat.Distribution[value]))
			}
			span = strings.Join(parts, " ")
		}
		t.row(name, typeAndDirection(stat.DataType, stat.Direction),
			strconv.FormatInt(stat.Count, 10), mean, span)
	}
	t.flush()
	return nil
}

func typeAndDirection(dataType, direction string) string {
	if direction == "" {
		return dataType
	}
	return dataType + " " + direction
}

func (r *run) runsFinish(ctx context.Context, args []string) error {
	var failed string
	fs := r.flags("runs finish")
	fs.StringVar(&failed, "failed", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	request := map[string]any{}
	if failed != "" {
		request["status"] = "failed"
		request["error"] = failed
	}
	body, err := r.api.Post(ctx, "/api/v1/runs/"+url.PathEscape(rest[0])+"/finish", request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	closed, err := decode[runObject](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s is %s\n", closed.ID, closed.Status)
	if closed.Error != "" {
		fmt.Fprintf(r.opt.Stdout, "error: %s\n", closed.Error)
	}
	return nil
}

// comparison is what `GET /api/v1/runs/{a}/compare/{b}` answers, as much of it
// as the table renders.
type comparison struct {
	A struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		DatasetVersion int    `json:"dataset_version"`
		Status         string `json:"status"`
	} `json:"a"`
	B struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		DatasetVersion int    `json:"dataset_version"`
		Status         string `json:"status"`
	} `json:"b"`
	Dataset     string `json:"dataset"`
	SameVersion bool   `json:"same_version"`
	Metadata    map[string]struct {
		A json.RawMessage `json:"a"`
		B json.RawMessage `json:"b"`
	} `json:"metadata"`
	Traces struct {
		Count      struct{ A, B int64 } `json:"count"`
		ErrorCount struct{ A, B int64 } `json:"error_count"`
		TotalCost  struct {
			A, B, Delta *float64
		} `json:"total_cost"`
		LatencyMs struct {
			P50 struct{ A, B *int64 } `json:"p50"`
			P95 struct{ A, B *int64 } `json:"p95"`
		} `json:"latency_ms"`
	} `json:"traces"`
	Scores []struct {
		Name      string `json:"name"`
		DataType  string `json:"data_type"`
		Direction string `json:"direction"`
		A         *struct {
			Mean  *float64 `json:"mean"`
			Count int64    `json:"count"`
		} `json:"a"`
		B *struct {
			Mean  *float64 `json:"mean"`
			Count int64    `json:"count"`
		} `json:"b"`
		Delta     *float64 `json:"delta"`
		Improved  int64    `json:"improved"`
		Regressed int64    `json:"regressed"`
		Changed   int64    `json:"changed"`
		Same      int64    `json:"same"`
	} `json:"scores"`
	Items []struct {
		ID     string `json:"id"`
		Seq    int64  `json:"seq"`
		In     string `json:"in"`
		Scores map[string]struct {
			A       json.RawMessage `json:"a"`
			B       json.RawMessage `json:"b"`
			Delta   *float64        `json:"delta"`
			Verdict string          `json:"verdict"`
		} `json:"scores"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func (r *run) runsCompare(ctx context.Context, args []string) error {
	var all bool
	fs := r.flags("runs compare")
	fs.BoolVar(&all, "all", false, "")
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}
	path := "/api/v1/runs/" + url.PathEscape(rest[0]) + "/compare/" + url.PathEscape(rest[1])
	if r.wantJSON() {
		body, err := r.api.Get(ctx, path, nil)
		if err != nil {
			return err
		}
		return r.emit(body)
	}

	// The table walks every page: the items worth reading are the ones that
	// moved, and stopping at the first page would hide the regression on
	// page two — which is the one thing this command exists to find.
	query := url.Values{"limit": {"500"}}
	var (
		compared comparison
		rows     = 0
	)
	for {
		body, err := r.api.Get(ctx, path, query)
		if err != nil {
			return err
		}
		page, err := decode[comparison](body)
		if err != nil {
			return err
		}
		if rows == 0 {
			compared = page
		} else {
			compared.Items = append(compared.Items, page.Items...)
		}
		rows += len(page.Items)
		if page.NextCursor == nil {
			break
		}
		query.Set("cursor", *page.NextCursor)
	}
	return r.renderComparison(compared, all)
}

func (r *run) renderComparison(compared comparison, all bool) error {
	out := r.opt.Stdout
	fmt.Fprintf(out, "%s\n", compared.Dataset)
	fmt.Fprintf(out, "a  %s  %s  version %d  %s\n",
		compared.A.ID, orDash(compared.A.Name), compared.A.DatasetVersion, compared.A.Status)
	fmt.Fprintf(out, "b  %s  %s  version %d  %s\n",
		compared.B.ID, orDash(compared.B.Name), compared.B.DatasetVersion, compared.B.Status)
	if !compared.SameVersion {
		fmt.Fprintln(out, "\nthe dataset moved between the two runs; items outside the "+
			"intersection are marked")
	}
	if len(compared.Metadata) > 0 {
		fmt.Fprintln(out)
		t := newTable(out, "METADATA", "A", "B")
		for _, key := range sortedNames(compared.Metadata) {
			t.row(key, compactJSON(compared.Metadata[key].A), compactJSON(compared.Metadata[key].B))
		}
		t.flush()
	}

	fmt.Fprintln(out)
	traces := compared.Traces
	t := newTable(out, "", "A", "B")
	t.row("traces", strconv.FormatInt(traces.Count.A, 10), strconv.FormatInt(traces.Count.B, 10))
	t.row("failed", strconv.FormatInt(traces.ErrorCount.A, 10), strconv.FormatInt(traces.ErrorCount.B, 10))
	t.row("cost", cost(traces.TotalCost.A), cost(traces.TotalCost.B))
	t.row("p50", duration(traces.LatencyMs.P50.A), duration(traces.LatencyMs.P50.B))
	t.row("p95", duration(traces.LatencyMs.P95.A), duration(traces.LatencyMs.P95.B))
	t.flush()

	if len(compared.Scores) > 0 {
		fmt.Fprintln(out)
		scores := newTable(out, "SCORE", "A", "B", "DELTA", "MOVED")
		for _, score := range compared.Scores {
			moved := fmt.Sprintf("%d improved, %d regressed, %d same",
				score.Improved, score.Regressed, score.Same)
			if score.Direction == "" || score.Direction == "none" {
				moved = fmt.Sprintf("%d changed, %d same", score.Changed, score.Same)
			}
			scores.row(score.Name, meanOf(score.A), meanOf(score.B), deltaOf(score.Delta), moved)
		}
		scores.flush()
	}

	// Only the items that moved, unless asked for all of them: a list where
	// every row says `same` buries the three that do not.
	items := newTable(out, "ITEM", "IN", "SCORE", "A", "B", "VERDICT")
	shown := 0
	for _, item := range compared.Items {
		for _, name := range sortedNames(item.Scores) {
			verdict := item.Scores[name]
			if !all && verdict.Verdict == "same" {
				continue
			}
			shown++
			items.row(item.ID, item.In, name,
				compactJSON(verdict.A), compactJSON(verdict.B), verdict.Verdict)
		}
		if len(item.Scores) == 0 && all {
			shown++
			items.row(item.ID, item.In, "-", "-", "-", "-")
		}
	}
	if shown == 0 {
		fmt.Fprintln(out, "\nno item changed; --all lists them anyway")
		return nil
	}
	fmt.Fprintln(out)
	items.flush()
	return nil
}

func meanOf(side *struct {
	Mean  *float64 `json:"mean"`
	Count int64    `json:"count"`
}) string {
	if side == nil || side.Mean == nil {
		return "-"
	}
	return trimFloat(*side.Mean)
}

func deltaOf(delta *float64) string {
	if delta == nil {
		return "-"
	}
	if *delta > 0 {
		return "+" + trimFloat(*delta)
	}
	return trimFloat(*delta)
}

// trimFloat prints a number the way a person would write it: no trailing
// zeroes, and no exponent for the small numbers a score is made of.
func trimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func (r *run) runsRemove(ctx context.Context, args []string) error {
	fs := r.flags("runs rm")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	// No confirmation: one row of bookkeeping goes, and the traces it held
	// are released rather than deleted (#20). A harness that opens a run per
	// CI job has to be able to prune from a script.
	body, err := r.api.Send(ctx, "DELETE", "/api/v1/runs/"+url.PathEscape(rest[0]), nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	deleted, err := decode[struct {
		ID             string `json:"id"`
		ReleasedTraces int64  `json:"released_traces"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "deleted %s, released %s to the retention window\n",
		deleted.ID, plural(int(deleted.ReleasedTraces), "trace"))
	return nil
}

func (r *run) scoreConfigs(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.scoreConfigsList(ctx, rest)
	case "show":
		return r.scoreConfigsShow(ctx, rest)
	case "push":
		return r.scoreConfigsPush(ctx, rest)
	case "rm":
		return r.scoreConfigsRemove(ctx, rest)
	}
	return usageErrorf("score-configs takes ls, show, push or rm, got %q", sub)
}

type scoreConfig struct {
	Name        string   `json:"name"`
	DataType    string   `json:"data_type"`
	Direction   string   `json:"direction"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	Categories  []string `json:"categories"`
	Description string   `json:"description"`
}

func (r *run) scoreConfigsList(ctx context.Context, args []string) error {
	fs := r.flags("score-configs ls")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/score-configs", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Configs []scoreConfig `json:"configs"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Configs) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no score configs")
		return nil
	}
	t := newTable(r.opt.Stdout, "NAME", "TYPE", "DIRECTION", "RANGE", "DESCRIPTION")
	for _, config := range listing.Configs {
		t.row(config.Name, config.DataType, orDash(config.Direction),
			orDash(configRange(config)), orDash(config.Description))
	}
	t.flush()
	return nil
}

// configRange renders what a config admits: the bounds of a number, or the
// words a category may be.
func configRange(config scoreConfig) string {
	if len(config.Categories) > 0 {
		return strings.Join(config.Categories, ", ")
	}
	switch {
	case config.Min != nil && config.Max != nil:
		return trimFloat(*config.Min) + ".." + trimFloat(*config.Max)
	case config.Min != nil:
		return "at least " + trimFloat(*config.Min)
	case config.Max != nil:
		return "at most " + trimFloat(*config.Max)
	}
	return ""
}

func (r *run) scoreConfigsShow(ctx context.Context, args []string) error {
	fs := r.flags("score-configs show")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/score-configs/"+url.PathEscape(rest[0]), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	config, err := decode[scoreConfig](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s: %s\n", config.Name, typeAndDirection(config.DataType, config.Direction))
	if span := configRange(config); span != "" {
		fmt.Fprintf(r.opt.Stdout, "admits: %s\n", span)
	}
	if config.Description != "" {
		fmt.Fprintf(r.opt.Stdout, "%s\n", config.Description)
	}
	return nil
}

func (r *run) scoreConfigsPush(ctx context.Context, args []string) error {
	var file string
	fs := r.flags("score-configs push")
	fs.StringVar(&file, "file", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if file == "" {
		return usageErrorf("score-configs push needs --file with the config's JSON body")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", file, err)
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", file, err)
	}
	body, err := r.api.Send(ctx, "PUT", "/api/v1/score-configs/"+url.PathEscape(rest[0]), nil, request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	config, err := decode[scoreConfig](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s: %s\n", config.Name, typeAndDirection(config.DataType, config.Direction))
	return nil
}

func (r *run) scoreConfigsRemove(ctx context.Context, args []string) error {
	fs := r.flags("score-configs rm")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	// Plain: removing a config changes what is accepted next and touches no
	// score that was already written (#17, #20).
	body, err := r.api.Send(ctx, "DELETE", "/api/v1/score-configs/"+url.PathEscape(rest[0]), nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	fmt.Fprintf(r.opt.Stdout, "removed the config for %s; the scores it admitted stay\n", rest[0])
	return nil
}

// addSomeBody sets a request field only when the flag carried a value: a
// request that spells out every empty option would ask the server to clear
// fields the caller never mentioned.
func addSomeBody(request map[string]any, key, value string) {
	if value != "" {
		request[key] = value
	}
}

// compactJSON renders an opaque body for a table cell: one line, cut where it
// stops being readable. The whole of it is one `--json` away.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "—"
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	const width = 48
	if len(text) <= width {
		return text
	}
	return text[:width-1] + "…"
}

func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}
