package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The six eval tools (spec 014 #22), all read-only. "Why did the eval regress"
// is an agent question of the same kind as "why did the last run fail"
// (spec 004 #17), and compare_runs is the answer to it; creating runs and
// posting items stays in the CLI and the HTTP API, where a hallucinated call
// has a human or a script to answer to.
//
// Like the other ten, each maps 1:1 onto one endpoint and returns its bytes
// unchanged as structuredContent (#16), so the tool and the API can never
// drift into two answers.

// registerEvals adds the dataset and run tools to the server.
func registerEvals(server *mcp.Server, t *toolset) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_datasets",
		Annotations: readOnly("List datasets"),
		Description: "Find the eval datasets a project has — the user asks what test sets exist, " +
			"how big one is, or how many runs it has had. " +
			"Returns a page of datasets by name with their current version and their item and run counts. " +
			"Does NOT return the cases themselves: call get_dataset_items with a name for those.",
		InputSchema: object(pagingProperties(map[string]*jsonschema.Schema{})),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"datasets": list(object(map[string]*jsonschema.Schema{
				"name":        text("The dataset's name, unique in the project."),
				"description": text("What it is for, as its author wrote it."),
				"version":     integer("Where its version clock stands; every changing write advances it by one."),
				"item_count":  integer("Live items at that version."),
				"run_count":   integer("Runs over it, in any state."),
				"created_at":  timestamp("When it was created."),
				"updated_at":  timestamp("When it last changed."),
			}, "name", "version", "item_count", "run_count"), "The page, alphabetical by name."),
			"next_cursor": text("Pass back as `cursor` for the next page; null on the last."),
		}, "datasets"),
	}, t.listDatasets)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_dataset_items",
		Annotations: readOnly("Get a dataset's cases"),
		Description: "Read the cases in a dataset — the user asks what a test set actually contains, " +
			"or what a particular case expects. " +
			"Returns the items at a version, whole: input, expected output and metadata, in first-appearance order. " +
			"Pass version to read the dataset as it was, which is what a run pinned; without it you get the current one. " +
			"Does NOT show what a run answered: call get_run_items for the attempts.",
		InputSchema: object(pagingProperties(map[string]*jsonschema.Schema{
			"name":    text("The dataset's name."),
			"version": integer("The dataset version to read at. Default: the current one."),
		}), "name"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"dataset": text("The dataset's name."),
			"version": integer("The version these items were resolved at."),
			"items": list(object(map[string]*jsonschema.Schema{
				"id":                    text("The case's id, which a trace stamps as tracepad.item_id."),
				"seq":                   integer("Its place in first-appearance order."),
				"version":               integer("The dataset version this row of the case was written at."),
				"input":                 anything("What the case feeds the system under test."),
				"expected_output":       anything("What a correct answer looks like, when the author wrote one."),
				"metadata":              anything("Whatever the author attached."),
				"source_trace_id":       text("The production trace this case was cut from, when it was."),
				"source_observation_id": text("The observation inside it."),
				"created_at":            timestamp("When this row of the case was written."),
			}, "id", "seq", "version"), "The page, in first-appearance order."),
			"next_cursor": text("Pass back as `cursor` for the next page; null on the last."),
			"prev_cursor": text("Pass back as `cursor` with `direction=prev`; null on the first."),
		}, "dataset", "version", "items"),
	}, t.getDatasetItems)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_runs",
		Annotations: readOnly("List runs"),
		Description: "Find eval runs — the user asks what has been tried, when, what ran lately, or which run to look at. " +
			"Returns a page of runs newest first, each with its dataset, the version it pinned, its status and its metadata. " +
			"Pass dataset to list one dataset's runs; without it the whole project's runs come back, across every dataset. " +
			"Does NOT include the summaries: call get_run with an id for coverage, scores, models and cost, " +
			"or compare_runs with two ids to see what changed between them.",
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"dataset": text("The dataset whose runs to list. Omit for every dataset's runs."),
		}), false)),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"runs":        list(runSchema(), "The page, newest first."),
			"next_cursor": text("Pass back as `cursor` for the next page; null on the oldest."),
			"prev_cursor": text("Pass back as `cursor` with `direction=prev`; null on the newest."),
		}, "runs"),
	}, t.listRuns)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_run",
		Annotations: readOnly("Get one run with its summary"),
		Description: "Read how one eval run went — the user names a run, or you picked one out of list_runs. " +
			"Returns the run with its summary: how much of the dataset it covered, how many traces it took and what they cost, " +
			"the mean, range or distribution of every score name, and the models and prompts it actually ran. " +
			"Does NOT show per-case results: call get_run_items for the attempts, " +
			"or compare_runs to see this run against another.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"id": matching(traceIDPattern, "The 32 lower-case hex characters of the run id."),
		}, "id"),
		OutputSchema: runWithSummarySchema(),
	}, t.getRun)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_run_items",
		Annotations: readOnly("Get a run's cases with their attempts"),
		Description: "Read what a run answered, case by case — the user asks which cases failed, what the model actually said, " +
			"or why a score is low. " +
			"Returns the dataset's items with the traces that answered each: the expected output beside the answer produced, " +
			"with the scores the attempt got. " +
			"A payload too large for the response budget arrives as {\"truncated\": true, …} carrying trace_id and observation_id: " +
			"pass that pair to get_observation_io to read it whole. " +
			"Pass unknown=true to also see the run's traces that name a case the dataset does not have at that version. " +
			"Does NOT aggregate: call get_run for the totals.",
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"id":      matching(traceIDPattern, "The 32 lower-case hex characters of the run id."),
			"unknown": oneOf("\"true\" appends the traces whose case is not in the run's dataset version.", "true", "false"),
		}), false), "id"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"run":             text("The run's id."),
			"dataset":         text("The dataset it ran."),
			"dataset_version": integer("The version it pinned."),
			"items":           list(runItemSchema(), "The page, in first-appearance order."),
			"next_cursor":     text("Pass back as `cursor` for the next page; null on the last."),
			"prev_cursor":     text("Pass back as `cursor` with `direction=prev`; null on the first."),
		}, "run", "dataset", "dataset_version", "items"),
	}, t.getRunItems)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compare_runs",
		Annotations: readOnly("Compare two runs"),
		Description: "Answer \"did this change make it better\" — the user has two runs of one dataset and wants the difference. " +
			"Returns, per score name, both means with the delta and how many cases improved, regressed or stayed the same; " +
			"the traffic and cost of both; the metadata keys the two runs disagree about; and then the cases, " +
			"each with its two values and a verdict. " +
			"A name says improved or regressed only when its config gives it a direction; otherwise the verdict is changed or same. " +
			"Runs of different datasets, and a run against itself, are refused. " +
			"Does NOT show the text of an answer: call get_run_items or get_observation_io for that.",
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"a": matching(traceIDPattern, "The baseline run's id."),
			"b": matching(traceIDPattern, "The run to judge against it."),
		}), false), "a", "b"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"a":            anything("The baseline run: id, name, dataset version, status, created_at."),
			"b":            anything("The other run, the same shape."),
			"dataset":      text("The dataset both ran."),
			"same_version": boolean("False when the dataset moved between the two runs."),
			"metadata":     anything("Only the metadata keys the two runs disagree about, both values."),
			"models":       anything("The models each run actually used."),
			"prompts":      anything("The prompts each run actually ran."),
			"traces":       anything("Both runs' counts, failures, cost and latency percentiles, with the cost delta."),
			"scores": list(object(map[string]*jsonschema.Schema{
				"name":      text("The score name."),
				"data_type": oneOf("How to read the values.", "numeric", "boolean", "categorical", "text"),
				"direction": text("Which way is better: higher, lower, none, or null when the name has no config."),
				"a":         anything("The baseline's aggregate for this name; null when it never carried it."),
				"b":         anything("The other run's aggregate."),
				"delta":     number("b's mean minus a's; null unless both are numbers."),
				"improved":  integer("Cases that moved the better way. Only for names with a direction."),
				"regressed": integer("Cases that moved the other way."),
				"changed":   integer("Cases that moved at all. Only for names without a direction."),
				"same":      integer("Cases whose value did not move. Equality is exact."),
			}, "name", "data_type", "direction", "a", "b", "delta", "same"), "One row per score name either run carried."),
			"items": list(object(map[string]*jsonschema.Schema{
				"id":  text("The case's id."),
				"seq": integer("Its place in the dataset's order."),
				"in": oneOf("Which run had the case. The version labels come first: an item missing from the other "+
					"run's version was never that run's to answer.",
					"both", "a", "b", "only_in_version_a", "only_in_version_b"),
				"scores": anything("Per score name both runs scored: the two values, the delta for numbers, and the verdict."),
			}, "id", "seq", "in", "scores"), "The cases at least one run attempted, in the dataset's order."),
			"next_cursor": text("Pass back as `cursor` for the next page of cases; null on the last."),
			"prev_cursor": text("Pass back as `cursor` with `direction=prev`; null on the first."),
		}, "a", "b", "dataset", "same_version", "scores", "items"),
	}, t.compareRuns)
}

// runSchema is a run without its summary, as the listing renders it.
func runSchema() *jsonschema.Schema {
	return object(map[string]*jsonschema.Schema{
		"id":              text("The run's id, which its traces stamp as tracepad.run_id."),
		"dataset":         text("The dataset it ran."),
		"dataset_version": integer("The version it pinned when it opened."),
		"name":            text("A free label the harness gave it."),
		"metadata":        anything("What was being tried, in the harness's own terms."),
		"status":          oneOf("What the harness said, never inferred.", "running", "finished", "failed"),
		"error":           text("Why it failed, when it did."),
		"created_at":      timestamp("When it opened."),
		"finished_at":     timestamp("When it closed; null while it is running."),
	}, "id", "dataset", "dataset_version", "status", "created_at")
}

// runWithSummarySchema is one run as `get_run` returns it: the same fields plus
// the summary, which is the reason to call it.
func runWithSummarySchema() *jsonschema.Schema {
	run := runSchema()
	run.Properties["summary"] = object(map[string]*jsonschema.Schema{
		"items": object(map[string]*jsonschema.Schema{
			"total":   integer("Live cases at the run's dataset version."),
			"covered": integer("How many of them a trace of the run named."),
			"missing": integer("How many got no trace at all."),
			"unknown": integer("Traces of the run that no case of that version accounts for."),
		}, "total", "covered", "missing", "unknown"),
		"traces": object(map[string]*jsonschema.Schema{
			"count":        integer("Traces the run holds."),
			"attempts_max": integer("The largest number of traces one case got."),
			"error_count":  integer("How many of the traces failed."),
			"total_cost":   number("Summed over the traces that carried a cost; null when none did."),
			"latency_ms": object(map[string]*jsonschema.Schema{
				"p50": integer("Median latency, exact over the run's traces."),
				"p95": integer("95th percentile, exact."),
			}),
		}, "count", "attempts_max", "error_count", "latency_ms"),
		"scores":  anything("Per score name: data type, direction, count, and mean/min/max or a distribution."),
		"models":  list(text("A model an observation of the run ran."), "Derived from the traces, not declared."),
		"prompts": list(anything("A prompt name and version."), "Derived from the traces."),
	}, "items", "traces", "scores", "models", "prompts")
	run.Required = append(run.Required, "summary")
	return run
}

// runItemSchema is one case with the attempts a run made at it.
func runItemSchema() *jsonschema.Schema {
	return object(map[string]*jsonschema.Schema{
		"id":              text("The case's id; null for a trace that named no case."),
		"seq":             integer("Its place in the dataset's order; absent for an unknown case."),
		"unknown":         boolean("True when the case is not in the run's dataset version."),
		"input":           anything("What the case feeds the system under test."),
		"expected_output": anything("What a correct answer looks like. Budgeted: may arrive truncated."),
		"attempts": list(object(map[string]*jsonschema.Schema{
			"trace_id":    text("The trace that answered."),
			"timestamp":   timestamp("When it ran."),
			"error_count": integer("Failed observations inside it."),
			"total_cost":  number("What it cost, when the client reported one."),
			"latency_ms":  integer("How long it took."),
			"output": anything("The trace's root observation's output. Budgeted: a large one arrives as " +
				"{\"truncated\": true, …} with the pair get_observation_io takes."),
			"scores": list(anything("A score: name, data type and value."), "What this attempt scored."),
		}, "trace_id", "error_count", "scores"), "The traces of the run that answered this case."),
	}, "id", "attempts")
}

type listDatasetsInput struct {
	pagingInput
}

func (t *toolset) listDatasets(ctx context.Context, req *mcp.CallToolRequest,
	in listDatasetsInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req, "/api/v1/datasets", in.apply(url.Values{}), summarizeDatasets)
}

type getDatasetItemsInput struct {
	pagingInput
	Name    string `json:"name"`
	Version *int   `json:"version"`
}

func (t *toolset) getDatasetItems(ctx context.Context, req *mcp.CallToolRequest,
	in getDatasetItemsInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	if in.Version != nil {
		query.Set("version", fmt.Sprint(*in.Version))
	}
	return t.call(ctx, req, "/api/v1/datasets/"+url.PathEscape(in.Name)+"/items",
		in.apply(query), summarizeItems)
}

type listRunsInput struct {
	pagingInput
	walkInput
	Dataset string `json:"dataset"`
}

// listRuns reads one dataset's runs, or — with no dataset — the project's
// (spec 016 #2). Two endpoints under one tool, because the question is the
// same one and the rows are the same object; the dataset only narrows it.
func (t *toolset) listRuns(ctx context.Context, req *mcp.CallToolRequest,
	in listRunsInput) (*mcp.CallToolResult, any, error) {
	path := "/api/v1/runs"
	if in.Dataset != "" {
		path = "/api/v1/datasets/" + url.PathEscape(in.Dataset) + "/runs"
	}
	return t.call(ctx, req, path,
		in.walkInput.apply(in.pagingInput.apply(url.Values{})), summarizeRuns)
}

type getRunInput struct {
	ID string `json:"id"`
}

func (t *toolset) getRun(ctx context.Context, req *mcp.CallToolRequest,
	in getRunInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req, "/api/v1/runs/"+url.PathEscape(in.ID), nil, summarizeRun)
}

type getRunItemsInput struct {
	pagingInput
	walkInput
	ID      string `json:"id"`
	Unknown string `json:"unknown"`
}

func (t *toolset) getRunItems(ctx context.Context, req *mcp.CallToolRequest,
	in getRunItemsInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "unknown", in.Unknown)
	return t.call(ctx, req, "/api/v1/runs/"+url.PathEscape(in.ID)+"/items",
		in.walkInput.apply(in.pagingInput.apply(query)), summarizeRunItems)
}

type compareRunsInput struct {
	pagingInput
	walkInput
	A string `json:"a"`
	B string `json:"b"`
}

func (t *toolset) compareRuns(ctx context.Context, req *mcp.CallToolRequest,
	in compareRunsInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req,
		"/api/v1/runs/"+url.PathEscape(in.A)+"/compare/"+url.PathEscape(in.B),
		in.walkInput.apply(in.pagingInput.apply(url.Values{})), summarizeComparison)
}

// The summaries below are the one line a client that reads `content` sees
// (#18). Each says what came back and what the obvious next call is, and none
// of them repeats the structured answer.

func summarizeDatasets(body json.RawMessage) string {
	var parsed struct {
		Datasets []struct {
			Name string `json:"name"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of datasets"
	}
	if len(parsed.Datasets) == 0 {
		return "This project has no datasets."
	}
	return fmt.Sprintf("%d datasets, first %q.", len(parsed.Datasets), parsed.Datasets[0].Name)
}

func summarizeItems(body json.RawMessage) string {
	var parsed struct {
		Dataset string `json:"dataset"`
		Version int    `json:"version"`
		Items   []any  `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of cases"
	}
	return fmt.Sprintf("%d cases of %q at version %d.", len(parsed.Items), parsed.Dataset, parsed.Version)
}

func summarizeRuns(body json.RawMessage) string {
	var parsed struct {
		Runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of runs"
	}
	if len(parsed.Runs) == 0 {
		return "No runs yet."
	}
	return fmt.Sprintf("%d runs, newest %s (%s).", len(parsed.Runs), parsed.Runs[0].ID, parsed.Runs[0].Status)
}

func summarizeRun(body json.RawMessage) string {
	var parsed struct {
		Dataset string `json:"dataset"`
		Status  string `json:"status"`
		Summary struct {
			Items struct {
				Covered int64 `json:"covered"`
				Total   int64 `json:"total"`
			} `json:"items"`
			Traces struct {
				Count      int64 `json:"count"`
				ErrorCount int64 `json:"error_count"`
			} `json:"traces"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one run"
	}
	return fmt.Sprintf("A %s run of %q: %d of %d cases covered by %d traces, %d failed.",
		parsed.Status, parsed.Dataset, parsed.Summary.Items.Covered, parsed.Summary.Items.Total,
		parsed.Summary.Traces.Count, parsed.Summary.Traces.ErrorCount)
}

func summarizeRunItems(body json.RawMessage) string {
	var parsed struct {
		Items []struct {
			Attempts []any `json:"attempts"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of cases"
	}
	attempts := 0
	for _, item := range parsed.Items {
		attempts += len(item.Attempts)
	}
	return fmt.Sprintf("%d cases with %d attempts between them.", len(parsed.Items), attempts)
}

func summarizeComparison(body json.RawMessage) string {
	var parsed struct {
		Dataset string `json:"dataset"`
		Scores  []struct {
			Name      string `json:"name"`
			Improved  int64  `json:"improved"`
			Regressed int64  `json:"regressed"`
			Changed   int64  `json:"changed"`
		} `json:"scores"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a comparison of two runs"
	}
	if len(parsed.Scores) == 0 {
		return fmt.Sprintf("Two runs of %q with no score in common.", parsed.Dataset)
	}
	first := parsed.Scores[0]
	if first.Improved+first.Regressed == 0 {
		return fmt.Sprintf("Two runs of %q: %q changed on %d cases.",
			parsed.Dataset, first.Name, first.Changed)
	}
	return fmt.Sprintf("Two runs of %q: %q improved on %d cases and regressed on %d.",
		parsed.Dataset, first.Name, first.Improved, first.Regressed)
}
