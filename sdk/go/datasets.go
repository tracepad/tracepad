package tracepad

import (
	"context"
	"encoding/json"
	"iter"
	"net/url"
	"strconv"
)

// Datasets and their items (spec 018 #1, spec 033 #10). NewDataset has made
// no request: it is a name, not a network call. Everything below is
// synchronous and returns errors — the harness is a script, and a script
// wants a value or an error.

// Item is one case. Tracepad never reads inside Input (docs/datasets.md).
type Item struct {
	ID             string `json:"id,omitempty"`
	Input          any    `json:"input,omitempty"`
	ExpectedOutput any    `json:"expected_output,omitempty"`
	Metadata       any    `json:"metadata,omitempty"`
	// DatasetVersion is the version this row was written at — the store
	// calls it `version`. Read back, never sent.
	DatasetVersion      int    `json:"-"`
	SourceTraceID       string `json:"source_trace_id,omitempty"`
	SourceObservationID string `json:"source_observation_id,omitempty"`
}

func readItem(row map[string]any) (Item, error) {
	var item Item
	raw, err := json.Marshal(row)
	if err == nil {
		err = json.Unmarshal(raw, &item)
	}
	if version, ok := row["version"].(float64); ok {
		item.DatasetVersion = int(version)
	}
	return item, err
}

// Dataset is a named set of cases, and the runs over it.
type Dataset struct {
	Name string
	path string
}

// NewDataset is a dataset by name. No request is made here. (The function
// is not named Dataset because the type is, and Go has one namespace for
// both.)
func NewDataset(name string) *Dataset {
	return &Dataset{Name: name, path: "/api/v1/datasets/" + url.PathEscape(name)}
}

// Create creates the dataset, or replaces its description and metadata; an
// empty description and a nil metadata are left unsent.
func (d *Dataset) Create(ctx context.Context, description string, metadata any) (map[string]any, error) {
	body := map[string]any{}
	if description != "" {
		body["description"] = description
	}
	if metadata != nil {
		body["metadata"] = metadata
	}
	c, err := current()
	if err != nil {
		return nil, err
	}
	answer, err := request(ctx, c, "PUT", d.path, body, nil)
	if err != nil {
		return nil, err
	}
	return object(answer), nil
}

// PutItems pushes the cases whole and reports the version and how many
// changed. The same cases again write nothing and leave the version where
// it was (changed == 0), so this belongs at the top of every CI run.
func (d *Dataset) PutItems(ctx context.Context, items []Item) (version, changed int, err error) {
	if items == nil {
		items = []Item{}
	}
	answer, err := post(ctx, d.path+"/items", items)
	if err != nil {
		return 0, 0, err
	}
	v, _ := answer["version"].(float64)
	n, _ := answer["changed"].(float64)
	return int(v), int(n), nil
}

// CurrentVersion asks Items for the dataset as it is now rather than at a
// version. It is not 0: version 0 is the dataset before its first item, a
// real version a run opened before PutItems is pinned to.
const CurrentVersion = -1

// Items is the cases at a version — the run's, not "the current one", unless
// CurrentVersion says so. Every page is walked; an error ends the sequence
// with it.
func (d *Dataset) Items(ctx context.Context, version int) iter.Seq2[Item, error] {
	params := url.Values{"limit": {strconv.Itoa(page)}}
	if version != CurrentVersion {
		params.Set("version", strconv.Itoa(version))
	}
	return func(yield func(Item, error) bool) {
		for row, err := range pages(ctx, d.path+"/items", params, "items") {
			if err != nil {
				yield(Item{}, err)
				return
			}
			item, err := readItem(row)
			if !yield(item, err) {
				return
			}
		}
	}
}

// RunOption configures Dataset.Run.
type RunOption func(map[string]any)

// WithRunMetadata is free metadata on the run — the prompt, the model.
func WithRunMetadata(metadata any) RunOption {
	return func(b map[string]any) { b["metadata"] = metadata }
}

// WithRunID names the run's id, for an id of your own (ItemID derives one).
func WithRunID(id string) RunOption { return func(b map[string]any) { b["id"] = id } }

// WithDatasetVersion pins the run to a version other than the current one.
func WithDatasetVersion(version int) RunOption {
	return func(b map[string]any) { b["dataset_version"] = version }
}

// Run opens a run, pinned to a version it hands back (spec 018 #2). Close it
// with Finish or Fail: a run left running is reported as such forever.
func (d *Dataset) Run(ctx context.Context, name string, opts ...RunOption) (*Run, error) {
	body := map[string]any{"name": name}
	for _, opt := range opts {
		opt(body)
	}
	answer, err := post(ctx, d.path+"/runs", body)
	if err != nil {
		return nil, err
	}
	run := &Run{dataset: d}
	run.ID, _ = answer["id"].(string)
	run.Name, _ = answer["name"].(string)
	version, _ := answer["dataset_version"].(float64)
	run.DatasetVersion = int(version)
	return run, nil
}

// Runs is this dataset's runs, newest first, as the server's JSON.
func (d *Dataset) Runs(ctx context.Context) iter.Seq2[map[string]any, error] {
	return pages(ctx, d.path+"/runs", url.Values{"limit": {strconv.Itoa(page)}}, "runs")
}

// Delete deletes the dataset with its items and runs. The name must be
// echoed, as the API asks; with an empty confirm the call is the API's dry
// run, and the answer says what would go.
func (d *Dataset) Delete(ctx context.Context, confirm string) (map[string]any, error) {
	c, err := current()
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if confirm != "" {
		params.Set("confirm", confirm)
	}
	answer, err := request(ctx, c, "DELETE", d.path, nil, params)
	if err != nil {
		return nil, err
	}
	return object(answer), nil
}
