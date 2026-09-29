package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Annotation queues (spec 024): the list of what to review, the ways to fill
// it, the hand-out that claims one item at a time, and the bookkeeping that
// says who completed what.
//
// The endpoints below are the whole surface: the desk, the CLI and a judge
// model script all annotate through exactly these, so no client can do
// something another cannot (spec 004 #1). Two of them are worth naming here —
// `from-traces` takes the *trace listing's* filters, so "queue what I am
// looking at" needs no second grammar (#4), and `complete` is refused unless
// the scores the queue asked for are already on the target, which is what
// makes *completed* mean something (#7).

// maxItemsPerAdd bounds one add. It is the same order as a score batch: an
// add of more than this is a job for `from-traces`, which is bounded on
// purpose.
const maxItemsPerAdd = 1000

// The bounds on `from-traces` (#4). The cap is what keeps one request from
// enqueuing a month.
const (
	defaultFromTraces = 100
	maxFromTraces     = 1000
)

// maxAnnotatorLength bounds the name a reviewer types (#6).
const maxAnnotatorLength = 200

type queueRequest struct {
	Description  string   `json:"description"`
	ScoreConfigs []string `json:"score_configs"`
}

type queueResponse struct {
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	ScoreConfigs []string    `json:"score_configs"`
	Counts       queueCounts `json:"counts"`
	CreatedAt    string      `json:"created_at"`
	UpdatedAt    string      `json:"updated_at"`
}

type queueCounts struct {
	Pending   int64 `json:"pending"`
	Completed int64 `json:"completed"`
	Skipped   int64 `json:"skipped"`
}

type queueListResponse struct {
	Queues []queueResponse `json:"queues"`
}

// itemTargetRequest is one target as the add carries it: a trace, or one
// observation of it. The target need not exist yet — the rule spec 003 gave
// scores, for the same reason (#2).
type itemTargetRequest struct {
	TraceID       string `json:"trace_id"`
	ObservationID string `json:"observation_id"`
}

type itemsAddedResponse struct {
	IDs      []string `json:"ids"`
	Added    int      `json:"added"`
	Existing int      `json:"existing"`
}

type fromTracesResponse struct {
	Matched  int  `json:"matched"`
	Added    int  `json:"added"`
	Existing int  `json:"existing"`
	Capped   bool `json:"capped"`
}

type queueItemResponse struct {
	ID            string `json:"id"`
	TraceID       string `json:"trace_id"`
	ObservationID string `json:"observation_id,omitempty"`
	Status        string `json:"status"`
	Seq           int64  `json:"seq"`
	AddedAt       string `json:"added_at"`
	ClaimedBy     string `json:"claimed_by,omitempty"`
	ClaimedUntil  string `json:"claimed_until,omitempty"`
	CompletedBy   string `json:"completed_by,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
	SkipReason    string `json:"skip_reason,omitempty"`
}

type queueItemListResponse struct {
	Queue       string              `json:"queue"`
	Items       []queueItemResponse `json:"items"`
	NextCursor  *string             `json:"next_cursor"`
	PrevCursor  *string             `json:"prev_cursor"`
	Total       *int                `json:"total,omitempty"`
	TotalCapped *bool               `json:"total_capped,omitempty"`
}

// nextResponse is what the desk asks for on every step. `item` is null when
// nothing is claimable, and `pending` then counts what other people are
// holding — the number that tells a reviewer to come back rather than that
// the queue is done (#5).
type nextResponse struct {
	Item    *queueItemResponse `json:"item"`
	Pending int64              `json:"pending"`
}

// annotatorRequest is the body of complete and reopen; skip adds a reason.
type annotatorRequest struct {
	Annotator string `json:"annotator"`
}

type skipRequest struct {
	Annotator string `json:"annotator"`
	Reason    string `json:"reason"`
}

// queueItemFilters is every filter the item listing accepts, held to
// `openapi.json` by the interface's parity test (spec 016 #13).
var queueItemFilters = []string{"status", "annotator"}

// handleListQueues lists the project's queues by name, whole: a project has as
// many review programmes as somebody named, which is the reasoning spec 003
// used for score configs (#8).
func (s *Server) handleListQueues(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	queues, err := s.store.Queues(r.Context(), project.ID)
	if err != nil {
		readFailed(w, r, "failed to list the queues", err)
		return
	}
	out := make([]queueResponse, 0, len(queues))
	for _, queue := range queues {
		out = append(out, renderQueue(queue))
	}
	writeJSON(w, http.StatusOK, queueListResponse{Queues: out})
}

// handlePutQueue creates a queue or replaces it whole (#1). Declarative like
// score configs, so a team keeps its queues in a file beside them: a re-PUT of
// the same body writes nothing and answers 200.
func (s *Server) handlePutQueue(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request queueRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	configs, err := queueConfigs(request.ScoreConfigs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	write := &store.QueuePut{
		ProjectID: project.ID,
		Queue: &store.AnnotationQueue{
			Name:         name,
			Description:  request.Description,
			ScoreConfigs: configs,
		},
		Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	status := http.StatusOK
	if write.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, renderQueue(write.Stored))
}

// queueConfigs validates the list of score config names. The names have to be
// namable — the same grammar a score name is filed under — and distinct,
// because the desk draws one control per entry and two of a name would be two
// controls over one score.
func queueConfigs(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf(`"score_configs" must name at least one score config`)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for i, name := range names {
		if name == "" {
			return nil, fmt.Errorf(`"score_configs" entry %d is empty`, i)
		}
		if len(name) > maxScoreNameLength {
			return nil, fmt.Errorf(`"score_configs" entry %q must be at most %d characters`,
				name, maxScoreNameLength)
		}
		if seen[name] {
			return nil, fmt.Errorf(`"score_configs" names %q twice`, name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// handleGetQueue reads one queue with its counts.
func (s *Server) handleGetQueue(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	queue, ok := s.loadQueue(w, r, project.ID, name)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, renderQueue(queue))
}

// handleDeleteQueue takes the queue and its items behind spec 005 #8's echo
// (#8). The note is the part a reader needs: the scores written while
// annotating are not here and do not go.
func (s *Server) handleDeleteQueue(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	confirm := values.Get("confirm")
	if confirm == "" {
		queue, ok := s.loadQueue(w, r, project.ID, name)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, renderQueueDeletion(true, name, queue.Counts.Total()).
			put("confirm", name).
			put("note", queueDeletionNote))
		return
	}
	deletion := &store.QueueDelete{ProjectID: project.ID, Name: name, Confirm: confirm}
	if !s.submit(w, r, deletion) {
		return
	}
	writeJSON(w, http.StatusOK, renderQueueDeletion(false, name, deletion.Items).
		put("deleted", true))
}

const queueDeletionNote = "the scores written while annotating stay on their traces; only the list goes"

func renderQueueDeletion(dryRun bool, name string, items int64) object {
	return object{}.
		put("dry_run", dryRun).
		put("name", name).
		put("would_delete", object{}.put("items", items))
}

// handleAddItems adds one target or an array of them, all or nothing (#4).
// Adding a target the queue already holds answers with the item it already is
// and counts it as `existing`, which is what makes a retried script and a
// re-run filter safe.
func (s *Server) handleAddItems(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}
	requests, err := decodeTargets(body)
	if err != nil {
		var over *overItemCap
		if errors.As(err, &over) {
			s.counters.observeOverBatchCap(project.ID, batchQueueTargets)
		}
		writeError(w, http.StatusBadRequest, queueAddRefusal(err))
		return
	}
	if len(requests) == 0 {
		writeError(w, http.StatusBadRequest, "no targets in the request")
		return
	}

	write := &store.QueueItemsAdd{
		ProjectID: project.ID,
		Queue:     name,
		Targets:   make([]store.QueueTarget, 0, len(requests)),
		Now:       time.Now().UnixNano(),
	}
	for i, request := range requests {
		target, err := request.validate()
		if err != nil {
			writeError(w, http.StatusBadRequest, itemError(len(requests), i, err))
			return
		}
		write.Targets = append(write.Targets, target)
	}
	if !s.submit(w, r, write) {
		return
	}
	ids := make([]string, 0, len(write.Items))
	for _, item := range write.Items {
		ids = append(ids, item.ID)
	}
	writeJSON(w, http.StatusCreated, itemsAddedResponse{
		IDs: ids, Added: write.Added, Existing: write.Existing,
	})
}

// decodeTargets reads the body as one target or an array of them, as strictly
// in both shapes (spec 003 #17), and counts an array before it decodes any of
// it (spec 024 #25): an add of more than maxItemsPerAdd targets is an
// *overItemCap at the cost of a scan, not of a decoded target for each of
// the millions a body of the size of the body cap can hold.
func decodeTargets(body []byte) ([]*itemTargetRequest, error) {
	return decodeBatch[itemTargetRequest](body, "target", maxItemsPerAdd)
}

// queueAddRefusal is what a queue add says of a body decodeTargets refused.
// An add over its limit is the 400 it always was, in the words it always
// used, which say what to use instead; the 413 and its text are the ones of
// scores and items, whose limit is a different one (spec 043 #36).
func queueAddRefusal(err error) string {
	var over *overItemCap
	if errors.As(err, &over) {
		return fmt.Sprintf("an add takes at most %d targets; use items/from-traces for a filter", over.limit)
	}
	return err.Error()
}

// validate checks the shape of the two ids. Neither is looked up: an item may
// be queued for a trace still in flight (#2). The shapes are checked all the
// same, because nothing but an id of that shape can ever be a trace of this
// store, so a value of another shape names a thing that cannot arrive.
func (in *itemTargetRequest) validate() (store.QueueTarget, error) {
	if !traceID.MatchString(in.TraceID) {
		return store.QueueTarget{}, fmt.Errorf(
			`"trace_id" must be 32 lower-case hex characters, got %q`, in.TraceID)
	}
	if in.ObservationID != "" && !spanID.MatchString(in.ObservationID) {
		return store.QueueTarget{}, fmt.Errorf(
			`"observation_id" must be 16 lower-case hex characters, got %q`, in.ObservationID)
	}
	return store.QueueTarget{TraceID: in.TraceID, ObservationID: in.ObservationID}, nil
}

// handleAddItemsFromTraces fills a queue with everything a trace filter
// matches, newest first, capped (#4). The filters are the listing's own, down
// to the validation and the 400 on an unknown one, which is what makes "queue
// what I am looking at" one call.
func (s *Server) handleAddItemsFromTraces(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, traceListFilters...), "limit")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := defaultFromTraces
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxFromTraces {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"limit must be a whole number between 1 and %d", maxFromTraces))
			return
		}
		limit = parsed
	}
	filter, err := traceFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Counted exactly rather than at the listing's cap of 1000 (spec 009
	// #12): this is one deliberate act, not a page view, and "1000+" would
	// leave the caller unable to tell a filter they can finish from one
	// they cannot. It is a read, so it happens before the job rather than
	// inside the writer's transaction.
	// In a read slot and under the read deadline, as a listing's count is:
	// it scans like one (spec 043 #29).
	var matched int
	if !s.readInSlot(w, r, "failed to count the matching traces", func(ctx context.Context) (err error) {
		matched, err = s.store.CountTraces(ctx, project.ID, filter, math.MaxInt32)
		return err
	}) {
		return
	}

	write := &store.QueueItemsFromTraces{
		ProjectID: project.ID,
		Queue:     name,
		Filter:    filter,
		Limit:     limit,
		Matched:   matched,
		Now:       time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusCreated, fromTracesResponse{
		Matched: matched, Added: write.Added, Existing: write.Existing, Capped: write.Capped,
	})
}

// handleListItems serves one queue's items in `seq` order, oldest first: the
// order they were added is the order they are worked in, and a listing read
// forward pages both ways like every other (spec 016 #19).
func (s *Server) handleListQueueItems(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, queueItemFilters...), "limit", "cursor", "direction", "count")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	counting, err := wantsCount(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := store.QueueItemFilter{Annotator: values.Get("annotator")}
	if status := values.Get("status"); status != "" {
		// A spelling outside the vocabulary is a 400 rather than an
		// empty listing: "nothing is done" would be a well-formed answer
		// to a typo (spec 003 #23).
		if !slices.Contains(store.ItemStatuses, status) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("status must be one of %s, got %q",
				strings.Join(store.ItemStatuses, ", "), status))
			return
		}
		filter.Status = status
	}
	if _, ok := s.loadQueue(w, r, project.ID, name); !ok {
		return
	}

	filter.Limit = limit + 1
	filter.Backward = backward
	raw := values.Get("cursor")
	if raw != "" {
		parts, err := decodeCursor(raw, 1)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		seq, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		filter.After = &seq
	}

	items, err := s.store.QueueItems(r.Context(), project.ID, name, filter)
	if err != nil {
		readFailed(w, r, "failed to list the items", err)
		return
	}
	// `trimPage` names the ends by the direction of the *scan*, so it holds
	// unchanged for a listing read forwards: the probe row is the one the
	// scan reached last either way (the dataset items page the same way,
	// spec 016 #19).
	items, prev, next := trimPage(items, limit, backward, raw,
		func(item *store.AnnotationItem) string {
			return encodeCursor(strconv.FormatInt(item.Seq, 10))
		})
	out := make([]queueItemResponse, 0, len(items))
	for _, item := range items {
		out = append(out, renderQueueItem(item))
	}
	answer := queueItemListResponse{Queue: name, Items: out, NextCursor: next, PrevCursor: prev}
	if counting {
		total, err := s.store.CountQueueItems(r.Context(), project.ID, name, filter, countCap+1)
		if err != nil {
			readFailed(w, r, "failed to count the items", err)
			return
		}
		value, stopped := capped(total)
		answer.Total, answer.TotalCapped = &value, &stopped
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleGetItem reads one item.
func (s *Server) handleGetQueueItem(w http.ResponseWriter, r *http.Request) {
	project, name, id, ok := s.itemTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := s.store.QueueItem(r.Context(), project.ID, name, id)
	if err != nil {
		readFailed(w, r, "failed to read the item", err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("queue %q has no item %s", name, id))
		return
	}
	writeJSON(w, http.StatusOK, renderQueueItem(item))
}

// handleNextItem hands out the item to work on and claims it for ten minutes
// (#5). A read in HTTP's terms and a write in this store's: reading an item
// does not claim it, and this endpoint exists precisely to.
func (s *Server) handleNextItem(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "annotator")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	annotator, err := annotatorName(values.Get("annotator"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	write := &store.QueueNext{
		ProjectID: project.ID,
		Queue:     name,
		Annotator: annotator,
		Now:       time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	answer := nextResponse{Pending: write.Pending}
	if write.Item != nil {
		rendered := renderQueueItem(write.Item)
		answer.Item = &rendered
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleCompleteItem is Decision 7: completed means the queue's shape was
// filled, checked against the scores actually on the target. A refusal names
// what is missing, so the desk can mark those controls and a script knows what
// to post.
func (s *Server) handleCompleteItem(w http.ResponseWriter, r *http.Request) {
	project, name, id, ok := s.itemTarget(w, r)
	if !ok {
		return
	}
	annotator, ok := s.annotatorBody(w, r)
	if !ok {
		return
	}
	write := &store.QueueItemComplete{
		ProjectID: project.ID, Queue: name, ID: id,
		Annotator: annotator, Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, renderQueueItem(write.Item))
}

// handleSkipItem marks an item skipped with the reason (#7).
func (s *Server) handleSkipItem(w http.ResponseWriter, r *http.Request) {
	project, name, id, ok := s.itemTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request skipRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	annotator, err := annotatorName(request.Annotator)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	write := &store.QueueItemSkip{
		ProjectID: project.ID, Queue: name, ID: id,
		Annotator: annotator, Reason: request.Reason, Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, renderQueueItem(write.Item))
}

// handleReopenItem returns a completed or skipped item to pending (#7), and
// releases the claim on one that is pending already — the desk's *Later*
// (Decision 16).
func (s *Server) handleReopenItem(w http.ResponseWriter, r *http.Request) {
	project, name, id, ok := s.itemTarget(w, r)
	if !ok {
		return
	}
	annotator, ok := s.annotatorBody(w, r)
	if !ok {
		return
	}
	write := &store.QueueItemReopen{
		ProjectID: project.ID, Queue: name, ID: id, Annotator: annotator,
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, renderQueueItem(write.Item))
}

// handleDeleteItem removes one item, with none of the queue deletion's
// ceremony: a re-add recreates it (#8).
func (s *Server) handleDeleteQueueItem(w http.ResponseWriter, r *http.Request) {
	project, name, id, ok := s.itemTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	deletion := &store.QueueItemDelete{ProjectID: project.ID, Queue: name, ID: id}
	if !s.submit(w, r, deletion) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("id", id))
}

// annotatorBody reads the `{annotator}` every finishing write carries (#6).
func (s *Server) annotatorBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	var request annotatorRequest
	if !s.readJSON(w, r, &request) {
		return "", false
	}
	annotator, err := annotatorName(request.Annotator)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return annotator, true
}

// annotatorName checks the name a client sends. The store has no users and
// this spec does not invent them (#6): what a small team needs is a name it
// can read "who said this" off, and a name is what this checks.
func annotatorName(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("annotator is required: it is what completed_by will say")
	}
	if len(value) > maxAnnotatorLength {
		return "", fmt.Errorf("annotator must be at most %d characters", maxAnnotatorLength)
	}
	return value, nil
}

// queueTarget authenticates and validates the `{name}` in the path.
func (s *Server) queueTarget(w http.ResponseWriter, r *http.Request) (*store.Project, string, bool) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return nil, "", false
	}
	name := r.PathValue("name")
	if err := validName("queue name", name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, "", false
	}
	return project, name, true
}

// itemTarget adds the `{id}`, which is one this server minted.
func (s *Server) itemTarget(w http.ResponseWriter, r *http.Request) (*store.Project, string, string, bool) {
	project, name, ok := s.queueTarget(w, r)
	if !ok {
		return nil, "", "", false
	}
	id, ok := hexPathID(w, r, "item id")
	if !ok {
		return nil, "", "", false
	}
	return project, name, id, true
}

// loadQueue reads a queue and answers the 404 itself.
func (s *Server) loadQueue(w http.ResponseWriter, r *http.Request, projectID, name string) (*store.AnnotationQueue, bool) {
	queue, err := s.store.Queue(r.Context(), projectID, name)
	if err != nil {
		readFailed(w, r, "failed to read the queue", err)
		return nil, false
	}
	if queue == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("queue %q not found", name))
		return nil, false
	}
	return queue, true
}

func renderQueue(queue *store.AnnotationQueue) queueResponse {
	return queueResponse{
		Name:         queue.Name,
		Description:  queue.Description,
		ScoreConfigs: queue.ScoreConfigs,
		Counts: queueCounts{
			Pending:   queue.Counts.Pending,
			Completed: queue.Counts.Completed,
			Skipped:   queue.Counts.Skipped,
		},
		CreatedAt: formatTime(queue.CreatedAt),
		UpdatedAt: formatTime(queue.UpdatedAt),
	}
}

func renderQueueItem(item *store.AnnotationItem) queueItemResponse {
	rendered := queueItemResponse{
		ID:            item.ID,
		TraceID:       item.TraceID,
		ObservationID: item.ObservationID,
		Status:        item.Status,
		Seq:           item.Seq,
		AddedAt:       formatTime(item.AddedAt),
		ClaimedBy:     item.ClaimedBy,
		CompletedBy:   item.CompletedBy,
		SkipReason:    item.SkipReason,
	}
	if item.ClaimedUntil != 0 {
		rendered.ClaimedUntil = formatTime(item.ClaimedUntil)
	}
	if item.CompletedAt != 0 {
		rendered.CompletedAt = formatTime(item.CompletedAt)
	}
	return rendered
}
