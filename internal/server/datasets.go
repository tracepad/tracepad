package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Datasets, items and runs (spec 014). A dataset is a named, versioned set of
// test cases; a run is the container a harness opens around one pass over it;
// the link from a trace to its run rides in on the trace itself (#2). The
// store executes nothing — every affordance here is reachable with curl and
// one attribute on a span (#1).

// datasetRequest is the body of PUT /api/v1/datasets/{name}: the envelope
// only, never the items. Both fields are optional; a PUT replaces the whole
// envelope, so a field left out is cleared.
type datasetRequest struct {
	Description string          `json:"description"`
	Metadata    json.RawMessage `json:"metadata"`
}

type datasetResponse struct {
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Metadata    json.RawMessage `json:"metadata"`
	Version     int             `json:"version"`
	ItemCount   int64           `json:"item_count"`
	RunCount    int64           `json:"run_count"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

type datasetListResponse struct {
	Datasets   []datasetResponse `json:"datasets"`
	NextCursor *string           `json:"next_cursor"`
	PrevCursor *string           `json:"prev_cursor"`
}

// itemRequest is one item as POST /api/v1/datasets/{name}/items carries it.
// `input` is required; the store never reads inside any of the three bodies
// (#4). The source pair is a note, not a reference: a string, checked for
// nothing but emptiness.
type itemRequest struct {
	ID                  *string         `json:"id"`
	Input               json.RawMessage `json:"input"`
	ExpectedOutput      json.RawMessage `json:"expected_output"`
	Metadata            json.RawMessage `json:"metadata"`
	SourceTraceID       *string         `json:"source_trace_id"`
	SourceObservationID *string         `json:"source_observation_id"`
}

type itemsWrittenResponse struct {
	IDs     []string `json:"ids"`
	Version int      `json:"version"`
	Changed int      `json:"changed"`
}

type itemResponse struct {
	ID                  string          `json:"id"`
	Seq                 int64           `json:"seq"`
	Version             int             `json:"version"`
	Archived            *bool           `json:"archived,omitempty"`
	Input               json.RawMessage `json:"input"`
	ExpectedOutput      json.RawMessage `json:"expected_output"`
	Metadata            json.RawMessage `json:"metadata"`
	SourceTraceID       *string         `json:"source_trace_id"`
	SourceObservationID *string         `json:"source_observation_id"`
	CreatedAt           string          `json:"created_at"`
}

type itemListResponse struct {
	Dataset    string         `json:"dataset"`
	Version    int            `json:"version"`
	Items      []itemResponse `json:"items"`
	NextCursor *string        `json:"next_cursor"`
	PrevCursor *string        `json:"prev_cursor"`
}

type itemVersionsResponse struct {
	Dataset  string         `json:"dataset"`
	ID       string         `json:"id"`
	Versions []itemResponse `json:"versions"`
}

type itemArchivedResponse struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// runRequest is the body of POST /api/v1/datasets/{name}/runs (#7, #9).
type runRequest struct {
	ID             *string         `json:"id"`
	Name           string          `json:"name"`
	Metadata       json.RawMessage `json:"metadata"`
	DatasetVersion *int            `json:"dataset_version"`
}

// runFinishRequest is the body of POST /api/v1/runs/{id}/finish (#8): empty
// for a run that finished, or `failed` with the harness's reason.
type runFinishRequest struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// runResponse is one run. `summary` rides only on `GET /api/v1/runs/{id}`:
// the listing is for choosing a run, and summarizing every row of a page would
// make choosing cost what reading costs (API contract → Runs).
type runResponse struct {
	ID             string          `json:"id"`
	Dataset        string          `json:"dataset"`
	DatasetVersion int             `json:"dataset_version"`
	Name           *string         `json:"name"`
	Metadata       json.RawMessage `json:"metadata"`
	Status         string          `json:"status"`
	Error          *string         `json:"error"`
	CreatedAt      string          `json:"created_at"`
	FinishedAt     *string         `json:"finished_at"`
	Summary        any             `json:"summary,omitempty"`
}

// runListResponse is a page of either run listing. The count rides only on the
// project-wide one, and only when asked for (spec 009): the per-dataset page
// has `run_count` on its dataset, exact, and a capped second number beside it
// would be worse than none.
type runListResponse struct {
	Runs        []runResponse `json:"runs"`
	NextCursor  *string       `json:"next_cursor"`
	PrevCursor  *string       `json:"prev_cursor"`
	Total       *int          `json:"total,omitempty"`
	TotalCapped *bool         `json:"total_capped,omitempty"`
}

type runDeletedResponse struct {
	ID             string `json:"id"`
	ReleasedTraces int64  `json:"released_traces"`
}

// handleListDatasets lists a project's datasets by name.
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction")
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
	after := ""
	raw := values.Get("cursor")
	if raw != "" {
		parts, err := decodeCursor(raw, 1)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		after = parts[0]
	}

	datasets, err := s.store.Datasets(r.Context(), project.ID, limit+1, after, backward)
	if err != nil {
		readFailed(w, r, "failed to list datasets", err)
		return
	}
	datasets, prev, next := trimPage(datasets, limit, backward, raw,
		func(d *store.Dataset) string { return encodeCursor(d.Name) })
	out := make([]datasetResponse, 0, len(datasets))
	for _, dataset := range datasets {
		out = append(out, renderDataset(dataset))
	}
	writeJSON(w, http.StatusOK, datasetListResponse{Datasets: out, NextCursor: next, PrevCursor: prev})
}

// handlePutDataset creates a dataset or replaces its description and
// metadata. The items and the version clock are untouched: a dataset's
// envelope is not one of its versions.
func (s *Server) handlePutDataset(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request datasetRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	metadata, err := optionalObject("metadata", request.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	write := &store.DatasetUpsert{
		ProjectID:   project.ID,
		Name:        name,
		Description: request.Description,
		Metadata:    metadata,
		Now:         time.Now().UnixNano(),
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, renderDataset(write.Dataset))
}

// handleGetDataset reads one dataset with its counts at the current version.
func (s *Server) handleGetDataset(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dataset, ok := s.loadDataset(w, r, project.ID, name)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, renderDataset(dataset))
}

// handleDeleteDataset is destructive in the spec 005 sense (#20): it cascades
// every run and releases every pinned trace, so it is a dry run until
// `?confirm=` echoes the name.
func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
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
		dataset, counts, err := s.store.DatasetPreview(r.Context(), project.ID, name)
		if err != nil {
			readFailed(w, r, "failed to read what the dataset holds", err)
			return
		}
		if dataset == nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q not found", name))
			return
		}
		writeJSON(w, http.StatusOK, renderDatasetCounts(true, name, counts).
			put("confirm", name).
			put("note", "the runs go with the dataset; their traces are not deleted but return to the retention window"))
		return
	}
	deletion := &store.DatasetDelete{ProjectID: project.ID, Name: name, Confirm: confirm}
	if !s.submit(w, r, deletion) {
		return
	}
	writeJSON(w, http.StatusOK, renderDatasetCounts(false, name, deletion.Counts))
}

func renderDatasetCounts(dryRun bool, name string, counts store.DatasetCounts) object {
	return object{}.
		put("dry_run", dryRun).
		put("dataset", name).
		put("items", counts.Items).
		put("runs", counts.Runs).
		put("pinned_traces", counts.PinnedTraces)
}

// handleCreateItems accepts one item or an array, all or nothing, and
// advances the dataset's version by one if anything changed (#5, #6). The
// dataset comes into being on the first write to its name.
func (s *Server) handleCreateItems(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
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
	requests, err := decodeItems(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(requests) == 0 {
		writeError(w, http.StatusBadRequest, "no items in the request")
		return
	}

	write := &store.DatasetItemsWrite{
		ProjectID: project.ID,
		Dataset:   name,
		Items:     make([]*store.DatasetItemInput, 0, len(requests)),
		Now:       time.Now().UnixNano(),
	}
	ids := make([]string, 0, len(requests))
	seen := make(map[string]int, len(requests))
	for i, request := range requests {
		item, err := request.validate()
		if err != nil {
			writeError(w, http.StatusBadRequest, indexedError("item", len(requests), i, err))
			return
		}
		if item.ID == "" {
			generated, err := store.NewID()
			if err != nil {
				slog.Error("id generation failed", "err", err)
				writeError(w, http.StatusInternalServerError, "cannot generate an item id")
				return
			}
			item.ID = generated
		}
		// Two items with one id in a batch would be one row with two
		// claims on it, and `changed` would count wrong (spec 003 #23).
		if first, duplicate := seen[item.ID]; duplicate {
			writeError(w, http.StatusBadRequest, indexedError("item", len(requests), i,
				fmt.Errorf("id %s is already used by the item at index %d", item.ID, first)))
			return
		}
		seen[item.ID] = i
		write.Items = append(write.Items, item)
		ids = append(ids, item.ID)
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusCreated, itemsWrittenResponse{IDs: ids, Version: write.Version, Changed: write.Changed})
}

// decodeItems reads the body as one item or an array of them, as strictly in
// both shapes (spec 003 #17).
func decodeItems(body []byte) ([]*itemRequest, error) {
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
		var requests []*itemRequest
		if err := decodeStrict(body, &requests); err != nil {
			return nil, err
		}
		for i, request := range requests {
			if request == nil {
				return nil, fmt.Errorf("item at index %d is null", i)
			}
		}
		return requests, nil
	}
	var request itemRequest
	if err := decodeStrict(body, &request); err != nil {
		return nil, err
	}
	return []*itemRequest{&request}, nil
}

// indexedError names which item of an array was refused; a single object
// speaks for itself.
func indexedError(kind string, total, index int, err error) string {
	if total == 1 {
		return err.Error()
	}
	return fmt.Sprintf("%s at index %d: %s", kind, index, err)
}

// validate turns an item request into what the store writes: the bodies
// compacted, so what comes back out is not padded by how it was sent, and the
// id checked or minted. Whether a body changed is the store's call, made on
// the JSON value (spec 014 #32).
func (in *itemRequest) validate() (*store.DatasetItemInput, error) {
	if !jsonValue(in.Input) {
		return nil, fmt.Errorf(`"input" is required`)
	}
	item := &store.DatasetItemInput{Input: compactJSON(in.Input)}
	if jsonValue(in.ExpectedOutput) {
		item.ExpectedOutput = compactJSON(in.ExpectedOutput)
	}
	if jsonValue(in.Metadata) {
		item.Metadata = compactJSON(in.Metadata)
	}
	for _, source := range []struct {
		field  string
		value  *string
		target *string
	}{
		{"source_trace_id", in.SourceTraceID, &item.SourceTraceID},
		{"source_observation_id", in.SourceObservationID, &item.SourceObservationID},
	} {
		if source.value == nil {
			continue
		}
		if *source.value == "" {
			return nil, fmt.Errorf("%q must not be empty", source.field)
		}
		*source.target = *source.value
	}
	// An id the client did not send is minted by the caller, not here: a
	// generator that failed is this server's problem and a 500, while
	// everything else validate can say is the client's and a 400 (found in
	// review of PR #30).
	if in.ID != nil {
		if !hexID.MatchString(*in.ID) {
			return nil, fmt.Errorf(`"id" must be 32 lower-case hex characters, got %q`, *in.ID)
		}
		item.ID = *in.ID
	}
	return item, nil
}

// handleListItems serves the dataset at a version, whole: the consumer is the
// harness, and a truncated test case is a different test case (#19). Bounded
// by the page and by what the store accepted at write time, never by the
// response budget.
func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "version", "limit", "cursor", "direction")
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
	dataset, ok := s.loadDataset(w, r, project.ID, name)
	if !ok {
		return
	}
	version, ok := requestedVersion(w, values, dataset)
	if !ok {
		return
	}
	filter := store.DatasetItemFilter{Version: version, Limit: limit + 1, Backward: backward}
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

	items, err := s.store.DatasetItems(r.Context(), project.ID, name, filter)
	if err != nil {
		readFailed(w, r, "failed to list the items", err)
		return
	}
	items, prev, next := trimPage(items, limit, backward, raw,
		func(item *store.DatasetItem) string { return encodeCursor(strconv.FormatInt(item.Seq, 10)) })
	out := make([]itemResponse, 0, len(items))
	for _, item := range items {
		out = append(out, renderItem(item, false))
	}
	writeJSON(w, http.StatusOK, itemListResponse{
		Dataset: name, Version: version, Items: out, NextCursor: next, PrevCursor: prev,
	})
}

// handleGetItem serves one item as of a version.
func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "version")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "item id")
	if !ok {
		return
	}
	dataset, ok := s.loadDataset(w, r, project.ID, name)
	if !ok {
		return
	}
	version, ok := requestedVersion(w, values, dataset)
	if !ok {
		return
	}
	item, err := s.store.DatasetItem(r.Context(), project.ID, name, id, version)
	if err != nil {
		readFailed(w, r, "failed to read the item", err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("dataset %q has no item %s at version %d", name, id, version))
		return
	}
	writeJSON(w, http.StatusOK, renderItem(item, false))
}

// handleListItemVersions serves every row of one item's history, newest
// first, archived rows included: an edit is a row and a delete is a row (#5).
func (s *Server) handleListItemVersions(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "item id")
	if !ok {
		return
	}
	versions, err := s.store.DatasetItemVersions(r.Context(), project.ID, name, id)
	if err != nil {
		readFailed(w, r, "failed to list the item's versions", err)
		return
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q has no item %s", name, id))
		return
	}
	out := make([]itemResponse, 0, len(versions))
	for _, item := range versions {
		out = append(out, renderItem(item, true))
	}
	writeJSON(w, http.StatusOK, itemVersionsResponse{Dataset: name, ID: id, Versions: out})
}

// handleDeleteItem archives an item at a new version. It is a plain DELETE:
// nothing is destroyed, the item is there at every earlier version (#5).
func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "item id")
	if !ok {
		return
	}
	archive := &store.DatasetItemArchive{
		ProjectID: project.ID, Dataset: name, ItemID: id, Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, archive) {
		return
	}
	writeJSON(w, http.StatusOK, itemArchivedResponse{ID: id, Version: archive.Version})
}

// handleCreateRun opens a run (#3, #7). The response carries the dataset
// version the run pinned, which is the version the harness must fetch its
// items at. A known id answers 200 with the existing run: creating a run is
// retry-safe (#9).
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request runRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	create := &store.RunCreate{
		ProjectID: project.ID, Dataset: name, Name: request.Name,
		DatasetVersion: request.DatasetVersion, Now: time.Now().UnixNano(),
	}
	if request.DatasetVersion != nil && *request.DatasetVersion < 0 {
		writeError(w, http.StatusBadRequest, `"dataset_version" must be a whole number of at least 0`)
		return
	}
	metadata, err := optionalObject("metadata", request.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	create.Metadata = metadata
	if request.ID != nil {
		if !hexID.MatchString(*request.ID) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf(`"id" must be 32 lower-case hex characters, got %q`, *request.ID))
			return
		}
		create.ID = *request.ID
	} else {
		generated, err := store.NewID()
		if err != nil {
			slog.Error("id generation failed", "err", err)
			writeError(w, http.StatusInternalServerError, "cannot generate a run id")
			return
		}
		create.ID = generated
	}
	if !s.submit(w, r, create) {
		return
	}
	status := http.StatusCreated
	if create.Existed {
		status = http.StatusOK
	}
	writeJSON(w, status, renderRun(create.Run))
}

// handleListRuns lists a dataset's runs newest first.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.datasetTarget(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction")
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
	if _, ok := s.loadDataset(w, r, project.ID, name); !ok {
		return
	}
	var after *store.RunCursor
	raw := values.Get("cursor")
	if raw != "" {
		if after, err = decodeRunCursor(raw); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	runs, err := s.store.Runs(r.Context(), project.ID, name, limit+1, after, backward)
	if err != nil {
		readFailed(w, r, "failed to list the runs", err)
		return
	}
	runs, prev, next := trimPage(runs, limit, backward, raw, func(run *store.DatasetRun) string {
		return encodeCursor(strconv.FormatInt(run.CreatedAt, 10), run.ID)
	})
	out := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		out = append(out, renderRun(run))
	}
	writeJSON(w, http.StatusOK, runListResponse{Runs: out, NextCursor: next, PrevCursor: prev})
}

// projectRunFilters is every filter the project-wide run listing accepts.
var projectRunFilters = []string{"dataset", "status"}

// handleListProjectRuns lists the project's runs across every dataset, newest
// first (spec 016 #2). The rows are the per-dataset listing's rows — they
// already carry `dataset` — and the count is the capped one every listing
// that a screen sits on offers (spec 009).
func (s *Server) handleListProjectRuns(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, projectRunFilters...), "limit", "cursor", "direction", "count")
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
	filter := store.RunFilter{Dataset: values.Get("dataset"), Status: values.Get("status")}
	// A dataset name outside the grammar can match nothing, and a status
	// outside the three the schema admits likewise: both are refused rather
	// than answered with an empty page (spec 003 #23).
	if filter.Dataset != "" {
		if err := validName("dataset", filter.Dataset); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	switch filter.Status {
	case "", store.RunRunning, store.RunFinished, store.RunFailed:
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("status must be %s, %s or %s, got %q",
				store.RunRunning, store.RunFinished, store.RunFailed, filter.Status))
		return
	}
	filter.Limit = limit + 1
	filter.Backward = backward
	raw := values.Get("cursor")
	if raw != "" {
		after, err := decodeRunCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = after
	}

	runs, err := s.store.ListRuns(r.Context(), project.ID, filter)
	if err != nil {
		readFailed(w, r, "failed to list the runs", err)
		return
	}
	runs, prev, next := trimPage(runs, limit, backward, raw, func(run *store.DatasetRun) string {
		return encodeCursor(strconv.FormatInt(run.CreatedAt, 10), run.ID)
	})
	answer := runListResponse{Runs: make([]runResponse, 0, len(runs)), NextCursor: next, PrevCursor: prev}
	for _, run := range runs {
		answer.Runs = append(answer.Runs, renderRun(run))
	}
	if counting {
		total, err := s.store.CountRuns(r.Context(), project.ID, filter, countCap+1)
		if err != nil {
			readFailed(w, r, "failed to count the runs", err)
			return
		}
		value, stopped := capped(total)
		answer.Total, answer.TotalCapped = &value, &stopped
	}
	writeJSON(w, http.StatusOK, answer)
}

// decodeRunCursor restores the `(created_at, id)` keyset a page of runs ends
// on, for both run listings.
func decodeRunCursor(raw string) (*store.RunCursor, error) {
	parts, err := decodeCursor(raw, 2)
	if err != nil {
		return nil, err
	}
	createdAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &store.RunCursor{CreatedAt: createdAt, ID: parts[1]}, nil
}

// handleGetRun serves one run with its summary: the coverage, the traffic, the
// score aggregates and what it actually ran. The listing leaves the summary
// out — a page of runs would pay for a summary nobody read.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "run id")
	if !ok {
		return
	}
	run, ok := s.loadRun(w, r, project.ID, id)
	if !ok {
		return
	}
	summary, err := s.store.RunSummary(r.Context(), project.ID, run)
	if err != nil {
		readFailed(w, r, "failed to summarize the run", err)
		return
	}
	body := renderRun(run)
	body.Summary = renderSummary(summary)
	writeJSON(w, http.StatusOK, body)
}

// handleFinishRun closes a run the way the harness says it ended (#8). The
// store never infers completion, and a run closed twice is a 409.
func (s *Server) handleFinishRun(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "run id")
	if !ok {
		return
	}
	var request runFinishRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	finish := &store.RunFinish{ProjectID: project.ID, ID: id, Now: time.Now().UnixNano()}
	switch request.Status {
	case "", store.RunFinished:
		if request.Error != "" {
			writeError(w, http.StatusBadRequest, `"error" belongs to a run that failed; send "status": "failed" with it`)
			return
		}
		finish.Status = store.RunFinished
	case store.RunFailed:
		finish.Status, finish.Error = store.RunFailed, request.Error
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf(`"status" must be %q or %q, got %q`, store.RunFinished, store.RunFailed, request.Status))
		return
	}
	if !s.submit(w, r, finish) {
		return
	}
	writeJSON(w, http.StatusOK, renderRun(finish.Run))
}

// handleDeleteRun removes one row of bookkeeping and releases the traces it
// held to the retention window; nothing else is deleted (#20).
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "run id")
	if !ok {
		return
	}
	deletion := &store.RunDelete{ProjectID: project.ID, ID: id}
	if !s.submit(w, r, deletion) {
		return
	}
	writeJSON(w, http.StatusOK, runDeletedResponse{ID: id, ReleasedTraces: deletion.Released})
}

// datasetTarget authenticates and validates the {name} every dataset route
// starts from. The grammar is the prompt name's (#9).
func (s *Server) datasetTarget(w http.ResponseWriter, r *http.Request) (*store.Project, string, bool) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return nil, "", false
	}
	name := r.PathValue("name")
	if err := validName("dataset name", name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, "", false
	}
	return project, name, true
}

// hexPathID validates a 32-hex {id} path segment.
func hexPathID(w http.ResponseWriter, r *http.Request, kind string) (string, bool) {
	return hexPathValue(w, r, "id", kind)
}

// hexPathValue is hexPathID for a route with more than one id in it — the
// comparison, whose two runs are `{a}` and `{b}`.
func hexPathValue(w http.ResponseWriter, r *http.Request, name, kind string) (string, bool) {
	id := r.PathValue(name)
	if !hexID.MatchString(id) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s must be 32 lower-case hex characters, got %q", kind, id))
		return "", false
	}
	return id, true
}

// loadDataset reads a dataset and answers the 404 itself.
func (s *Server) loadDataset(w http.ResponseWriter, r *http.Request, projectID, name string) (*store.Dataset, bool) {
	dataset, err := s.store.Dataset(r.Context(), projectID, name)
	if err != nil {
		readFailed(w, r, "failed to read the dataset", err)
		return nil, false
	}
	if dataset == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q not found", name))
		return nil, false
	}
	return dataset, true
}

// loadRun reads a run and answers the 404 itself.
func (s *Server) loadRun(w http.ResponseWriter, r *http.Request, projectID, id string) (*store.DatasetRun, bool) {
	run, err := s.store.Run(r.Context(), projectID, id)
	if err != nil {
		readFailed(w, r, "failed to read the run", err)
		return nil, false
	}
	if run == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("run %s not found", id))
		return nil, false
	}
	return run, true
}

// requestedVersion reads `?version=`: the dataset's current version when
// absent, a 400 when above it (#7). Zero is the dataset before its first
// item and is a legitimate thing to ask for.
func requestedVersion(w http.ResponseWriter, values interface{ Get(string) string }, dataset *store.Dataset) (int, bool) {
	raw := values.Get("version")
	if raw == "" {
		return dataset.Version, true
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version < 0 {
		writeError(w, http.StatusBadRequest, "version must be a whole number of at least 0")
		return 0, false
	}
	if version > dataset.Version {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("dataset %q is at version %d; there is no version %d", dataset.Name, dataset.Version, version))
		return 0, false
	}
	return version, true
}

// optionalObject reads a metadata field that must be a JSON object when it is
// present at all, compacted for storage.
func optionalObject(field string, raw json.RawMessage) ([]byte, error) {
	if !jsonValue(raw) {
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%q must be a JSON object", field)
	}
	return compactJSON(raw), nil
}

func renderDataset(dataset *store.Dataset) datasetResponse {
	return datasetResponse{
		Name:        dataset.Name,
		Description: nullable(dataset.Description),
		Metadata:    json.RawMessage(dataset.Metadata),
		Version:     dataset.Version,
		ItemCount:   dataset.ItemCount,
		RunCount:    dataset.RunCount,
		CreatedAt:   formatTime(dataset.CreatedAt),
		UpdatedAt:   formatTime(dataset.UpdatedAt),
	}
}

// renderItem renders one row. `archived` is shown only on the history
// listing, where a row may be one; a listing at a version never holds one.
func renderItem(item *store.DatasetItem, history bool) itemResponse {
	out := itemResponse{
		ID:                  item.ID,
		Seq:                 item.Seq,
		Version:             item.Version,
		Input:               json.RawMessage(item.Input),
		ExpectedOutput:      json.RawMessage(item.ExpectedOutput),
		Metadata:            json.RawMessage(item.Metadata),
		SourceTraceID:       nullable(item.SourceTraceID),
		SourceObservationID: nullable(item.SourceObservationID),
		CreatedAt:           formatTime(item.CreatedAt),
	}
	if history {
		archived := item.Archived
		out.Archived = &archived
	}
	return out
}

func renderRun(run *store.DatasetRun) runResponse {
	out := runResponse{
		ID:             run.ID,
		Dataset:        run.Dataset,
		DatasetVersion: run.DatasetVersion,
		Name:           nullable(run.Name),
		Metadata:       json.RawMessage(run.Metadata),
		Status:         run.Status,
		Error:          nullable(run.Error),
		CreatedAt:      formatTime(run.CreatedAt),
	}
	if run.FinishedAt != 0 {
		finished := formatTime(run.FinishedAt)
		out.FinishedAt = &finished
	}
	return out
}

// nullable renders an empty string as JSON null: on these objects every key
// is always present, so a consumer reads one shape, and null is how "never
// set" is spelled.
func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
