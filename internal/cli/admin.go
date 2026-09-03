package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tracepad/tracepad/internal/store"
)

// The administrative commands (spec 005): `projects`, `keys`, `retention` and
// `users rm-data`. Like the rest of the CLI they are HTTP clients of the API
// and contain no logic of their own (spec 004 #1) — including the safety
// model, which is the server's: a destructive request answers with a preview
// until it is confirmed, so the CLI shows that preview and asks for the echo
// the server named.
//
// That is why `--yes` is not a bypass. It still makes both requests and still
// sends the server's own confirm value back; what it replaces is the human
// typing it. A non-interactive run without it stops with the preview on
// stderr, because a script that deletes a project by default is a script that
// deletes a project by accident (spec 005 #13).

// preview is the dry-run answer every destructive endpoint gives before it is
// confirmed (spec 005 #8).
type preview struct {
	DryRun      bool             `json:"dry_run"`
	WouldDelete map[string]int64 `json:"would_delete"`
	Oldest      string           `json:"oldest"`
	Confirm     string           `json:"confirm"`
	Note        string           `json:"note"`
	// Runs are the eval runs an erasure would take traces from
	// (spec 014 #14). Only the user-data preview carries them; every other
	// destructive endpoint leaves the field absent, and an absent field
	// prints nothing. The wire name is `affected_runs` because a dataset
	// deletion answers with a `runs` count, and this struct is what reads
	// every preview.
	Runs []affectedRun `json:"affected_runs"`
	// The dataset deletion's own counts (spec 014 #20). Pointers, because
	// this one struct reads every preview and zero is a real answer: a
	// dataset with no runs would otherwise look like an erasure preview
	// that never mentioned runs at all.
	Items        *int64 `json:"items"`
	DatasetRuns  *int64 `json:"runs"`
	PinnedTraces *int64 `json:"pinned_traces"`
}

// affectedRun is one run that loses traces to an erasure.
type affectedRun struct {
	ID      string `json:"id"`
	Dataset string `json:"dataset"`
	Traces  int64  `json:"traces"`
}

// projectView is a project as the API renders it.
type projectView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	RetentionDays    *int   `json:"retention_days"`
	RawRetentionDays *int   `json:"raw_retention_days"`
	// StatsRetentionDays is the rollup's own window. It outlives the traces
	// it summarizes, so it is a window of its own (spec 013 #6).
	StatsRetentionDays *int   `json:"stats_retention_days"`
	CreatedAt          string `json:"created_at"`
	DeletedAt          string `json:"deleted_at"`
	PurgeAt            string `json:"purge_at"`
}

func (r *run) projects(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.projectsList(ctx, rest)
	case "show":
		return r.projectsShow(ctx, rest)
	case "create":
		return r.projectsCreate(ctx, rest)
	case "rename":
		return r.projectsRename(ctx, rest)
	case "rm":
		return r.projectsRemove(ctx, rest)
	case "restore":
		return r.projectsRestore(ctx, rest)
	}
	return usageErrorf("projects takes ls, show, create, rename, rm or restore, got %q", sub)
}

func (r *run) projectsList(ctx context.Context, args []string) error {
	var deleted bool
	fs := r.flags("projects ls")
	fs.BoolVar(&deleted, "deleted", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	query := url.Values{}
	if deleted {
		query.Set("include", "deleted")
	}

	body, err := r.api.Get(ctx, "/api/v1/projects", query)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Projects []projectView `json:"projects"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Projects) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no projects")
		return nil
	}
	t := newTable(r.opt.Stdout, "ID", "NAME", "RETENTION", "RAW", "CREATED", "STATUS")
	for _, project := range listing.Projects {
		t.row(project.ID, project.Name,
			window(project.RetentionDays, "forever"),
			window(project.RawRetentionDays, "follows"),
			shortTime(project.CreatedAt),
			projectStatus(project))
	}
	t.flush()
	return nil
}

func (r *run) projectsShow(ctx context.Context, args []string) error {
	fs := r.flags("projects show")
	project := fs.String("project", "", "")
	rest, err := r.parse(fs, args, anyArgs)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageErrorf("projects show takes one project id, got %d", len(rest))
	}
	// Both forms at once name the project twice, and the positional would
	// quietly win over the flag. Refused as a form rather than compared as
	// values: the reader of `projects show a --project b` cannot tell which
	// one the command obeys, and being right by accident when the two agree
	// teaches the wrong rule (addWalk's, spec 003 #23).
	//
	// "Given" and not "non-empty", because `--project "$PROJ"` with nothing in
	// PROJ is the shape that would otherwise slip through the guard and let
	// the positional win after all (found in review of PR #24).
	if len(rest) == 1 && wasGiven(fs, "project") {
		return usageErrorf("pass either the positional id or --project, not both")
	}
	given := *project
	if len(rest) == 1 {
		// The same expansion again, this time as the positional: an
		// argument that is there and says nothing. `projectID` cannot
		// refuse it — from inside, an empty id and no id at all look the
		// same — and this is the one command where "no id" is a legal
		// question, so the difference has to be kept here (found in review
		// of PR #26). The flag's half is refused by `projectID`, which
		// every command taking `--project` goes through.
		if rest[0] == "" {
			return usageErrorf("the project id is empty; pass one, " +
				"or no argument at all to use the project this key reaches")
		}
		given = rest[0]
	}
	id, err := r.projectID(ctx, fs, given)
	if err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/projects/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	renderProject(r, view)
	return nil
}

func (r *run) projectsCreate(ctx context.Context, args []string) error {
	fs := r.flags("projects create")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPost, "/api/v1/projects", nil,
		map[string]any{"name": rest[0]})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	created, err := decode[struct {
		projectView
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "project %s created (%s)\n\n", created.Name, created.ID)
	fmt.Fprintf(r.opt.Stdout, "  TRACEPAD_API_KEY=%s\n", created.SecretKey)
	fmt.Fprintf(r.opt.Stdout, "  LANGFUSE_PUBLIC_KEY=%s\n", created.PublicKey)
	fmt.Fprintf(r.opt.Stdout, "  LANGFUSE_SECRET_KEY=%s\n\n", created.SecretKey)
	fmt.Fprintln(r.opt.Stdout, "the secret key is shown only here; only its hash is stored")
	return nil
}

func (r *run) projectsRename(ctx context.Context, args []string) error {
	fs := r.flags("projects rename")
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPatch, "/api/v1/projects/"+url.PathEscape(rest[0]), nil,
		map[string]any{"name": rest[1]})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "project %s is now named %s\n", view.ID, view.Name)
	return nil
}

// projectsRemove is the soft delete. It answers with the purge date, because
// "you have a week to change your mind" is the only part of this the operator
// needs to remember (spec 005 #9).
func (r *run) projectsRemove(ctx context.Context, args []string) error {
	var yes bool
	fs := r.flags("projects rm")
	fs.BoolVar(&yes, "yes", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	path := "/api/v1/projects/" + url.PathEscape(rest[0])

	body, err := r.destructive(ctx, http.MethodDelete, path, nil, nil, yes,
		"delete project "+rest[0])
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "project %s deleted; its data is purged at %s\n",
		view.Name, shortTime(view.PurgeAt))
	fmt.Fprintf(r.opt.Stdout, "restore it until then with: tracepad projects restore %s\n", view.ID)
	return nil
}

func (r *run) projectsRestore(ctx context.Context, args []string) error {
	fs := r.flags("projects restore")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPost,
		"/api/v1/projects/"+url.PathEscape(rest[0])+"/restore", nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "project %s restored\n", view.Name)
	return nil
}

func (r *run) keys(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.keysList(ctx, rest)
	case "create":
		return r.keysCreate(ctx, rest)
	case "rm":
		return r.keysRemove(ctx, rest)
	}
	return usageErrorf("keys takes ls, create or rm, got %q", sub)
}

func (r *run) keysList(ctx context.Context, args []string) error {
	fs := r.flags("keys ls")
	project := fs.String("project", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/projects/"+url.PathEscape(id)+"/keys", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Keys []struct {
			PublicKey string `json:"public_key"`
			CreatedAt string `json:"created_at"`
		} `json:"keys"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Keys) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no keys: this project cannot ingest until one is created")
		return nil
	}
	t := newTable(r.opt.Stdout, "PUBLIC KEY", "CREATED")
	for _, key := range listing.Keys {
		t.row(key.PublicKey, shortTime(key.CreatedAt))
	}
	t.flush()
	return nil
}

func (r *run) keysCreate(ctx context.Context, args []string) error {
	fs := r.flags("keys create")
	project := fs.String("project", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPost,
		"/api/v1/projects/"+url.PathEscape(id)+"/keys", nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	created, err := decode[struct {
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "  TRACEPAD_API_KEY=%s\n", created.SecretKey)
	fmt.Fprintf(r.opt.Stdout, "  LANGFUSE_PUBLIC_KEY=%s\n", created.PublicKey)
	fmt.Fprintf(r.opt.Stdout, "  LANGFUSE_SECRET_KEY=%s\n\n", created.SecretKey)
	fmt.Fprintln(r.opt.Stdout,
		"the secret key is shown only here; move your SDKs onto it, then revoke the old key")
	return nil
}

func (r *run) keysRemove(ctx context.Context, args []string) error {
	var yes bool
	fs := r.flags("keys rm")
	project := fs.String("project", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	path := "/api/v1/projects/" + url.PathEscape(id) + "/keys/" + url.PathEscape(rest[0])

	body, err := r.destructive(ctx, http.MethodDelete, path, nil, nil, yes,
		"revoke key "+rest[0])
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	fmt.Fprintf(r.opt.Stdout, "key %s revoked\n", rest[0])
	return nil
}

func (r *run) retention(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "show":
		return r.retentionShow(ctx, rest)
	case "set":
		return r.retentionSet(ctx, rest)
	}
	return usageErrorf("retention takes show or set, got %q", sub)
}

func (r *run) retentionShow(ctx context.Context, args []string) error {
	fs := r.flags("retention show")
	project := fs.String("project", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/projects/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s\n", view.Name)
	fmt.Fprintf(r.opt.Stdout, "  traces      %s\n", window(view.RetentionDays, "kept forever"))
	fmt.Fprintf(r.opt.Stdout, "  raw bodies  %s\n",
		window(view.RawRetentionDays, "follow the trace window"))
	fmt.Fprintf(r.opt.Stdout, "  statistics  %s\n",
		window(view.StatsRetentionDays, "kept forever"))
	return nil
}

// retentionSet moves one or both windows. The server decides whether the
// change is destructive — a window that grows applies straight away, one that
// shrinks answers with a preview — so this command does not try to guess.
func (r *run) retentionSet(ctx context.Context, args []string) error {
	var (
		days      int
		rawDays   int
		statsDays int
		forever   bool
		rawFollow bool
		statsKeep bool
		yes       bool
	)
	fs := r.flags("retention set")
	project := fs.String("project", "", "")
	fs.IntVar(&days, "days", 0, "")
	fs.IntVar(&rawDays, "raw-days", 0, "")
	fs.IntVar(&statsDays, "stats-days", 0, "")
	fs.BoolVar(&forever, "forever", false, "")
	fs.BoolVar(&rawFollow, "raw-follow", false, "")
	fs.BoolVar(&statsKeep, "stats-forever", false, "")
	fs.BoolVar(&yes, "yes", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	if days != 0 && forever {
		return usageErrorf("--days and --forever say different things; pick one")
	}
	if rawDays != 0 && rawFollow {
		return usageErrorf("--raw-days and --raw-follow say different things; pick one")
	}
	if statsDays != 0 && statsKeep {
		return usageErrorf("--stats-days and --stats-forever say different things; pick one")
	}

	request := map[string]any{}
	switch {
	case forever:
		request["retention_days"] = nil
	case days != 0:
		if err := checkWindow("--days", days); err != nil {
			return err
		}
		request["retention_days"] = days
	}
	switch {
	case rawFollow:
		request["raw_retention_days"] = nil
	case rawDays != 0:
		if err := checkWindow("--raw-days", rawDays); err != nil {
			return err
		}
		request["raw_retention_days"] = rawDays
	}
	switch {
	case statsKeep:
		request["stats_retention_days"] = nil
	case statsDays != 0:
		if err := checkWindow("--stats-days", statsDays); err != nil {
			return err
		}
		request["stats_retention_days"] = statsDays
	}
	if len(request) == 0 {
		return usageErrorf("retention set needs --days, --forever, --raw-days, --raw-follow, " +
			"--stats-days or --stats-forever")
	}

	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	body, err := r.destructive(ctx, http.MethodPatch,
		"/api/v1/projects/"+url.PathEscape(id), nil, request, yes, "shorten retention")
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	view, err := decode[projectView](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s\n", view.Name)
	fmt.Fprintf(r.opt.Stdout, "  traces      %s\n", window(view.RetentionDays, "kept forever"))
	fmt.Fprintf(r.opt.Stdout, "  raw bodies  %s\n",
		window(view.RawRetentionDays, "follow the trace window"))
	fmt.Fprintf(r.opt.Stdout, "  statistics  %s\n",
		window(view.StatsRetentionDays, "kept forever"))
	fmt.Fprintln(r.opt.Stdout, "\nthe new window takes effect on the next sweep")
	return nil
}

func (r *run) users(ctx context.Context, args []string) error {
	sub, rest := split(args)
	if sub != "rm-data" {
		return usageErrorf("users takes rm-data, got %q", sub)
	}
	var yes bool
	fs := r.flags("users rm-data")
	project := fs.String("project", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	positional, err := r.parse(fs, rest, 1)
	if err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	path := "/api/v1/projects/" + url.PathEscape(id) +
		"/users/" + url.PathEscape(positional[0]) + "/data"

	body, err := r.destructive(ctx, http.MethodDelete, path, nil, nil, yes,
		"erase the data of user "+positional[0])
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	result, err := decode[struct {
		Deleted map[string]int64 `json:"deleted"`
		UserID  string           `json:"user_id"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "erased the data of %s\n", result.UserID)
	t := newTable(r.opt.Stdout)
	for _, kind := range []string{"traces", "observations", "scores", "payloads"} {
		if count, reported := result.Deleted[kind]; reported {
			t.row("  "+kind, strconv.FormatInt(count, 10))
		}
	}
	t.flush()
	fmt.Fprintln(r.opt.Stdout,
		"\nraw OTLP bodies are not erased; they expire on the raw retention window")
	return nil
}

// destructive runs the two-step contract of spec 005 #8: ask once without a
// confirmation and see what the server says it would do, then ask again with
// the echo it named. A response that is not a dry run is returned as it is —
// which is how a retention window that grows, or a key that is not the last
// one, goes through in a single request.
func (r *run) destructive(ctx context.Context, method, path string, query url.Values,
	body any, yes bool, what string) (json.RawMessage, error) {

	answer, err := r.api.Send(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	dry, err := decode[preview](answer)
	if err != nil || !dry.DryRun {
		return answer, err
	}

	r.renderPreview(dry, what)
	switch {
	case yes:
		// Still the server's own confirm value, never one this command
		// made up: --yes replaces the typing, not the check.
	case r.opt.TTY:
		if err := r.askToConfirm(dry.Confirm); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf(
			"this would %s; it was not done. Re-run with --yes to go ahead", what)
	}

	confirmed := url.Values{}
	for key, values := range query {
		confirmed[key] = values
	}
	confirmed.Set("confirm", dry.Confirm)
	return r.api.Send(ctx, method, path, confirmed, body)
}

// renderPreview prints what the server said the request would cost. It goes to
// stderr so that a piped `--json` run still emits only the final answer on
// stdout.
func (r *run) renderPreview(dry preview, what string) {
	out := r.opt.Stderr
	fmt.Fprintf(out, "this would %s:\n", what)
	kinds := make([]string, 0, len(dry.WouldDelete))
	for kind := range dry.WouldDelete {
		kinds = append(kinds, kind)
	}
	sortStrings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(out, "  %-14s %d\n", kind, dry.WouldDelete[kind])
	}
	if dry.Oldest != "" {
		fmt.Fprintf(out, "  %-14s %s\n", "oldest", shortTime(dry.Oldest))
	}
	// A dataset deletion counts its own three things rather than a table of
	// stores, and the third is the one that matters: the traces its runs
	// were keeping out of the sweep are released, not deleted (#20).
	for _, counted := range []struct {
		label string
		value *int64
	}{
		{"items", dry.Items},
		{"runs", dry.DatasetRuns},
		{"pinned traces", dry.PinnedTraces},
	} {
		if counted.value != nil {
			fmt.Fprintf(out, "  %-14s %d\n", counted.label, *counted.value)
		}
	}
	// Erasure outranks the pin an eval run puts on its traces, and the run
	// then shows those items as missing (spec 014 #14). The operator sees
	// the hole before it opens, which is the whole reason the server names
	// the runs.
	for _, affected := range dry.Runs {
		fmt.Fprintf(out, "  %-14s %s of %s loses %d\n",
			"run", affected.ID, affected.Dataset, affected.Traces)
	}
	if dry.Note != "" {
		fmt.Fprintf(out, "%s\n", dry.Note)
	}
}

// askToConfirm reads the echo from the terminal. Typing the name is the point
// (spec 005 #8): a y/n prompt confirms that a key was pressed, and this
// confirms that the right thing was meant.
func (r *run) askToConfirm(want string) error {
	if r.opt.Stdin == nil {
		return fmt.Errorf("nothing to read a confirmation from; re-run with --yes")
	}
	fmt.Fprintf(r.opt.Stderr, "type %q to confirm: ", want)
	line, err := bufio.NewReader(r.opt.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("nothing was confirmed; nothing was done")
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("that is not %q; nothing was done", want)
	}
	return nil
}

// projectID resolves which project a command is about. An explicit --project
// wins; otherwise the API is asked, and a credential that reaches exactly one
// project answers the question by itself — which is the whole of it for the
// single-project install most deployments are.
//
// A `--project` that arrived without an id is refused rather than read as "no
// --project": an unset shell variable expands to exactly that, and falling
// back to the one project the key reaches would answer a wider question than
// the caller asked (spec 003 #23). The rule lives here so that every command
// taking the flag gets it, which is how the six besides `projects show` were
// found to be missing it.
func (r *run) projectID(ctx context.Context, fs *flag.FlagSet, given string) (string, error) {
	if given == "" && wasGiven(fs, "project") {
		return "", usageErrorf("--project needs a project id; it was passed empty")
	}
	if given != "" {
		return given, nil
	}
	body, err := r.api.Get(ctx, "/api/v1/projects", nil)
	if err != nil {
		return "", err
	}
	listing, err := decode[struct {
		Projects []projectView `json:"projects"`
	}](body)
	if err != nil {
		return "", err
	}
	switch len(listing.Projects) {
	case 0:
		return "", fmt.Errorf("this key reaches no project; pass --project with an id")
	case 1:
		return listing.Projects[0].ID, nil
	}
	names := make([]string, 0, len(listing.Projects))
	for _, project := range listing.Projects {
		names = append(names, project.Name+" ("+project.ID+")")
	}
	sortStrings(names)
	return "", usageErrorf("this key reaches %d projects; pass --project with one of: %s",
		len(listing.Projects), strings.Join(names, ", "))
}

func renderProject(r *run, view projectView) {
	fmt.Fprintf(r.opt.Stdout, "project %s\n", view.Name)
	fmt.Fprintf(r.opt.Stdout, "  id          %s\n", view.ID)
	fmt.Fprintf(r.opt.Stdout, "  created     %s\n", shortTime(view.CreatedAt))
	fmt.Fprintf(r.opt.Stdout, "  traces      %s\n", window(view.RetentionDays, "kept forever"))
	fmt.Fprintf(r.opt.Stdout, "  raw bodies  %s\n",
		window(view.RawRetentionDays, "follow the trace window"))
	fmt.Fprintf(r.opt.Stdout, "  statistics  %s\n",
		window(view.StatsRetentionDays, "kept forever"))
	if view.DeletedAt != "" {
		fmt.Fprintf(r.opt.Stdout, "  deleted     %s\n", shortTime(view.DeletedAt))
		fmt.Fprintf(r.opt.Stdout, "  purged at   %s\n", shortTime(view.PurgeAt))
	}
}

// checkWindow rejects a window the server would refuse anyway, so a typo comes
// back as a usage error rather than as a round trip. The ceiling is not taste:
// a window becomes a nanosecond cutoff, and a big enough one overflows into the
// future where it matches everything. "Forever" is `--forever`.
func checkWindow(flag string, days int) error {
	if days < 1 || days > store.MaxRetentionDays {
		return usageErrorf("%s must be between 1 and %d days, or use --forever, got %d",
			flag, store.MaxRetentionDays, days)
	}
	return nil
}

// window renders a retention setting. A nil window is not "0 days" and not
// blank: it is a policy, and saying which one is the difference between a
// table a reader trusts and one they have to look up (#2, #6).
func window(days *int, unset string) string {
	if days == nil {
		return unset
	}
	if *days == 1 {
		return "1 day"
	}
	return strconv.Itoa(*days) + " days"
}

func projectStatus(view projectView) string {
	if view.DeletedAt == "" {
		return "live"
	}
	return "deleted, purged " + shortTime(view.PurgeAt)
}
