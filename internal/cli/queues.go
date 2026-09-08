package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The annotation commands (spec 024 #9): `queues`. Between them they are the
// desk's every step — declare the queue, fill it by hand or by filter, take
// the next item, complete, skip or reopen it — so a script can annotate with a
// judge model through the same door a person uses (spec 004 #1).
//
// The scores themselves are `scores add`, which already exists: this half is
// the *list*, and completing an item is the server checking that the scores
// the queue asked for are there.

func (r *run) queues(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.queuesList(ctx, rest)
	case "put":
		return r.queuesPut(ctx, rest)
	case "rm":
		return r.queuesRemove(ctx, rest)
	case "add":
		return r.queuesAdd(ctx, rest)
	case "items":
		return r.queuesItems(ctx, rest)
	case "next":
		return r.queuesNext(ctx, rest)
	case "complete":
		return r.queuesFinish(ctx, "complete", rest)
	case "skip":
		return r.queuesFinish(ctx, "skip", rest)
	case "reopen":
		return r.queuesFinish(ctx, "reopen", rest)
	}
	return usageErrorf(
		"queues takes ls, put, rm, add, items, next, complete, skip or reopen, got %q", sub)
}

// queueObject is a queue as every queue endpoint renders it.
type queueObject struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ScoreConfigs []string `json:"score_configs"`
	Counts       struct {
		Pending   int64 `json:"pending"`
		Completed int64 `json:"completed"`
		Skipped   int64 `json:"skipped"`
	} `json:"counts"`
	UpdatedAt string `json:"updated_at"`
}

// queueItem is one item as every item endpoint renders it.
type queueItem struct {
	ID            string `json:"id"`
	TraceID       string `json:"trace_id"`
	ObservationID string `json:"observation_id"`
	Status        string `json:"status"`
	Seq           int64  `json:"seq"`
	AddedAt       string `json:"added_at"`
	ClaimedBy     string `json:"claimed_by"`
	ClaimedUntil  string `json:"claimed_until"`
	CompletedBy   string `json:"completed_by"`
	CompletedAt   string `json:"completed_at"`
	SkipReason    string `json:"skip_reason"`
}

func (r *run) queuesList(ctx context.Context, args []string) error {
	fs := r.flags("queues ls")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/queues", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Queues []queueObject `json:"queues"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Queues) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no queues")
		return nil
	}
	t := newTable(r.opt.Stdout, "NAME", "PENDING", "DONE", "SKIPPED", "CONFIGS", "DESCRIPTION")
	for _, queue := range listing.Queues {
		t.row(queue.Name,
			strconv.FormatInt(queue.Counts.Pending, 10),
			strconv.FormatInt(queue.Counts.Completed, 10),
			strconv.FormatInt(queue.Counts.Skipped, 10),
			orDash(strings.Join(queue.ScoreConfigs, ",")),
			orDash(queue.Description))
	}
	t.flush()
	return nil
}

// queuesPut is the declarative half: the whole queue in one call, so a team
// keeps its queues in a script beside its score configs (#1).
func (r *run) queuesPut(ctx context.Context, args []string) error {
	var (
		configs     repeated
		description string
	)
	fs := r.flags("queues put")
	fs.Var(&configs, "config", "")
	fs.StringVar(&description, "description", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return usageErrorf("queues put needs at least one --config: a queue is the scores it asks for")
	}
	body, err := r.api.Send(ctx, "PUT", "/api/v1/queues/"+url.PathEscape(rest[0]), nil,
		map[string]any{"description": description, "score_configs": []string(configs)})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	queue, err := decode[queueObject](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s asks for %s\n", queue.Name, strings.Join(queue.ScoreConfigs, ", "))
	return nil
}

// repeated is a string flag that may be given more than once, in order. The
// order matters here: it is the order the desk asks the reviewer for the
// scores (#1).
type repeated []string

func (v *repeated) String() string { return strings.Join(*v, ",") }

func (v *repeated) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func (r *run) queuesRemove(ctx context.Context, args []string) error {
	var yes bool
	fs := r.flags("queues rm")
	fs.BoolVar(&yes, "yes", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	name := rest[0]
	body, err := r.destructive(ctx, "DELETE", "/api/v1/queues/"+url.PathEscape(name),
		nil, nil, yes, "delete queue "+name+" with its items (the scores stay)")
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	deleted, err := decode[struct {
		Name        string `json:"name"`
		WouldDelete struct {
			Items int64 `json:"items"`
		} `json:"would_delete"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "deleted %s: %s gone, the scores stay on their traces\n",
		deleted.Name, plural(int(deleted.WouldDelete.Items), "item"))
	return nil
}

// queuesAdd fills a queue: one target by hand, or everything a listing filter
// matches (#4). The two are one command because they are one act — deciding
// what deserves a human verdict — and the filter flags are the listing's own.
func (r *run) queuesAdd(ctx context.Context, args []string) error {
	var (
		filters     traceFilterFlags
		trace       string
		observation string
		fromTraces  bool
		search      string
		limit       int
	)
	fs := r.flags("queues add")
	filters.register(fs)
	fs.StringVar(&search, "search", "", "")
	fs.StringVar(&trace, "trace", "", "")
	fs.StringVar(&observation, "observation", "", "")
	fs.BoolVar(&fromTraces, "from-traces", false, "")
	fs.IntVar(&limit, "limit", 0, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	name := rest[0]
	// Exactly one of the two ways in, because they answer different
	// questions and a command that quietly picked one would enqueue
	// something nobody asked for.
	if (trace == "") == (!fromTraces) {
		return usageErrorf(
			"queues add takes either --trace ID or --from-traces with the listing's filters")
	}
	if trace != "" {
		return r.queuesAddOne(ctx, name, trace, observation)
	}
	if observation != "" {
		return usageErrorf("--observation names one target and belongs with --trace")
	}
	query, err := filters.query(r)
	if err != nil {
		return err
	}
	addSome(query, "q", search)
	if limit != 0 {
		if limit < 1 || limit > 1000 {
			return usageErrorf("--limit must be between 1 and 1000, got %d", limit)
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	body, err := r.api.Send(ctx, "POST",
		"/api/v1/queues/"+url.PathEscape(name)+"/items/from-traces", query, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	added, err := decode[struct {
		Matched  int  `json:"matched"`
		Added    int  `json:"added"`
		Existing int  `json:"existing"`
		Capped   bool `json:"capped"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%d matched, %s added, %d already queued\n",
		added.Matched, plural(added.Added, "trace"), added.Existing)
	if added.Capped {
		// The way out is in the message: the cap is not a refusal, it is
		// one callful, and the next one starts where this stopped.
		fmt.Fprintln(r.opt.Stdout,
			"more matched than the limit allowed; run it again with --until at the oldest one added")
	}
	return nil
}

func (r *run) queuesAddOne(ctx context.Context, name, trace, observation string) error {
	target := map[string]any{"trace_id": trace}
	if observation != "" {
		target["observation_id"] = observation
	}
	body, err := r.api.Post(ctx, "/api/v1/queues/"+url.PathEscape(name)+"/items", target)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	added, err := decode[struct {
		IDs      []string `json:"ids"`
		Added    int      `json:"added"`
		Existing int      `json:"existing"`
	}](body)
	if err != nil {
		return err
	}
	if added.Existing > 0 {
		fmt.Fprintf(r.opt.Stdout, "already in %s as %s\n", name, first(added.IDs))
		return nil
	}
	fmt.Fprintf(r.opt.Stdout, "added to %s as %s\n", name, first(added.IDs))
	return nil
}

func first(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return ids[0]
}

func (r *run) queuesItems(ctx context.Context, args []string) error {
	var (
		status    string
		annotator string
		cursor    string
		limit     int
		newer     bool
		total     bool
	)
	fs := r.flags("queues items")
	fs.StringVar(&status, "status", "", "")
	fs.StringVar(&annotator, "annotator", "", "")
	fs.StringVar(&cursor, "cursor", "", "")
	fs.IntVar(&limit, "limit", 0, "")
	// One direction flag, not two: this listing reads oldest first, so its
	// far end is where it starts and `--newer` is the only walk that means
	// anything (the mirror of `traces ls --oldest`).
	fs.BoolVar(&newer, "newer", false, "")
	fs.BoolVar(&total, "total", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	query := url.Values{}
	addSome(query, "status", status)
	addSome(query, "annotator", annotator)
	if err := addCursor(query, fs, cursor); err != nil {
		return err
	}
	if newer {
		if cursor == "" {
			return usageErrorf("--newer walks back from a --cursor")
		}
		query.Set("direction", "prev")
	}
	if err := addLimit(query, limit); err != nil {
		return err
	}
	if total {
		query.Set("count", "1")
	}

	body, err := r.api.Get(ctx, "/api/v1/queues/"+url.PathEscape(rest[0])+"/items", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	page, err := decode[struct {
		Items       []queueItem `json:"items"`
		NextCursor  *string     `json:"next_cursor"`
		PrevCursor  *string     `json:"prev_cursor"`
		Total       *int        `json:"total"`
		TotalCapped *bool       `json:"total_capped"`
	}](body)
	if err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no items")
		return nil
	}
	t := newTable(r.opt.Stdout, "SEQ", "ID", "TARGET", "STATUS", "BY", "WHEN", "REASON")
	for _, item := range page.Items {
		t.row(strconv.FormatInt(item.Seq, 10), item.ID, itemTarget(item),
			item.Status, orDash(item.CompletedBy), shortTime(item.CompletedAt),
			orDash(item.SkipReason))
	}
	t.flush()
	if page.Total != nil {
		fmt.Fprintf(r.opt.Stdout, "\n%s matching\n", matchCount(*page.Total, page.TotalCapped))
	}
	walkOn(r, "more", page.NextCursor, page.PrevCursor)
	return nil
}

// itemTarget names what an item points at: the trace, and the observation
// inside it when the item is about one.
func itemTarget(item queueItem) string {
	if item.ObservationID == "" {
		return item.TraceID
	}
	return item.TraceID + "/" + item.ObservationID
}

// queuesNext takes the next item and claims it (#5). It prints what to do with
// it, because the CLI's own next step is `scores add` and then `complete`.
func (r *run) queuesNext(ctx context.Context, args []string) error {
	var annotator string
	fs := r.flags("queues next")
	fs.StringVar(&annotator, "annotator", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if annotator == "" {
		return usageErrorf("queues next needs --annotator: it is what completed_by will say")
	}
	name := rest[0]
	body, err := r.api.Get(ctx, "/api/v1/queues/"+url.PathEscape(name)+"/next",
		url.Values{"annotator": {annotator}})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	answer, err := decode[struct {
		Item    *queueItem `json:"item"`
		Pending int64      `json:"pending"`
	}](body)
	if err != nil {
		return err
	}
	if answer.Item == nil {
		if answer.Pending == 0 {
			fmt.Fprintf(r.opt.Stdout, "%s is done\n", name)
			return nil
		}
		fmt.Fprintf(r.opt.Stdout, "nothing to take: %s still pending, all claimed by somebody else\n",
			plural(int(answer.Pending), "item"))
		return nil
	}
	item := *answer.Item
	fmt.Fprintf(r.opt.Stdout, "%s  %s\nclaimed until %s\n",
		item.ID, itemTarget(item), shortTime(item.ClaimedUntil))
	return nil
}

// queuesFinish is complete, skip and reopen: three verbs, one body, one shape
// of answer. `--reason` belongs to skip alone, and is refused on the other two
// rather than ignored (spec 003 #23).
func (r *run) queuesFinish(ctx context.Context, verb string, args []string) error {
	var (
		annotator string
		reason    string
	)
	fs := r.flags("queues " + verb)
	fs.StringVar(&annotator, "annotator", "", "")
	if verb == "skip" {
		fs.StringVar(&reason, "reason", "", "")
	}
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}
	if annotator == "" {
		return usageErrorf("queues %s needs --annotator", verb)
	}
	request := map[string]any{"annotator": annotator}
	if verb == "skip" {
		request["reason"] = reason
	}
	body, err := r.api.Post(ctx,
		"/api/v1/queues/"+url.PathEscape(rest[0])+"/items/"+url.PathEscape(rest[1])+"/"+verb,
		request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	item, err := decode[queueItem](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s is %s\n", item.ID, item.Status)
	return nil
}
