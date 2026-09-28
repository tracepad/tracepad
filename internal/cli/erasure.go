package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/termsafe"
)

// An erasure is a task on the server (spec 047 #6): `rm-data` asks for it to
// be answered within thirty seconds, and when it is not, watches it until it
// ends (#18). `users erasure` and `users erasures` read what the server
// records of them.

// erasureWait is the `wait` a confirmed erasure asks for: the most the server
// grants (#7).
const erasureWait = "30"

// erasurePoll is how often a running erasure is read again.
var erasurePoll = 2 * time.Second

// erasureView is the erasure resource as the CLI renders it.
type erasureView struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	Phase      string  `json:"phase"`
	UserID     string  `json:"user_id"`
	CreatedAt  string  `json:"created_at"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
	Error      *string `json:"error"`
	Progress   struct {
		TracesAtStart *int64 `json:"traces_at_start"`
		TracesDeleted int64  `json:"traces_deleted"`
	} `json:"progress"`
	Deleted    map[string]int64 `json:"deleted"`
	Compaction struct {
		ExpectedBy *string `json:"expected_by"`
	} `json:"compaction"`
	Backup *struct {
		CreatedAt   string `json:"created_at"`
		RemoveAfter string `json:"remove_after"`
	} `json:"pre_migration_backup"`
}

// ended reports an erasure that is done or failed. An answer without a state
// is a server from before erasures were tasks, whose answer was the end.
func (e erasureView) ended() bool {
	return e.State == "done" || e.State == "failed" || e.State == ""
}

// progress is the erasure's stage in words: the phase and, once the parsed
// phase has a whole to count against, how far into it.
func (e erasureView) progress() string {
	if e.Phase == "" {
		return e.State
	}
	if e.Progress.TracesAtStart == nil {
		return e.Phase
	}
	return fmt.Sprintf("%s %d/%d traces", e.Phase, e.Progress.TracesDeleted, *e.Progress.TracesAtStart)
}

func erasurePath(projectID, id string) string {
	return "/api/v1/projects/" + url.PathEscape(projectID) + "/erasures/" + url.PathEscape(id)
}

func (r *run) usersRemoveData(ctx context.Context, rest []string) error {
	var yes, noWait bool
	fs := r.flags("users rm-data")
	project := fs.String("project", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	fs.BoolVar(&noWait, "no-wait", false, "")
	positional, err := r.parse(fs, rest, 1)
	if err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	user := positional[0]
	path := "/api/v1/projects/" + url.PathEscape(id) + "/users/" + url.PathEscape(user) + "/data"

	body, confirmed, err := r.confirmDestructive(ctx, http.MethodDelete, path, nil, nil, yes,
		"erase the data of user "+user)
	if err != nil {
		return err
	}
	if confirmed != nil {
		if !noWait {
			confirmed.Set("wait", erasureWait)
		}
		body, err = confirmErasure(ctx, erasuresCommand(fs), func(ctx context.Context) (json.RawMessage, error) {
			return r.api.Send(ctx, http.MethodDelete, path, confirmed, nil)
		})
		if err != nil {
			return err
		}
	}
	erasure, err := decode[erasureView](body)
	if err != nil {
		return err
	}
	if confirmed != nil && noWait {
		if r.wantJSON() {
			return r.emit(body)
		}
		fmt.Fprintf(r.opt.Stdout, "erasure %s %s; tracepad users erasure %s\n",
			termsafe.String(erasure.ID), termsafe.String(erasure.State), termsafe.String(erasure.ID))
		return nil
	}
	if !erasure.ended() {
		if body, err = r.watchErasure(ctx, id, erasure); err != nil {
			return err
		}
		if erasure, err = decode[erasureView](body); err != nil {
			return err
		}
	}
	if erasure.State == "failed" {
		if r.wantJSON() {
			if err := r.emit(body); err != nil {
				return err
			}
		}
		return fmt.Errorf("the erasure %s failed: %s", erasure.ID, failure(erasure.Error))
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	fmt.Fprintf(r.opt.Stdout, "erased the data of %s\n", termsafe.String(user))
	r.renderErased(erasure)
	return nil
}

// watchErasure reads a running erasure until it ends, saying on stderr where
// it is each time that changes.
func (r *run) watchErasure(ctx context.Context, projectID string, erasure erasureView) (json.RawMessage, error) {
	said := ""
	for {
		if stage := erasure.progress(); stage != said {
			fmt.Fprintf(r.opt.Stderr, "erasing: %s\n", termsafe.String(stage))
			said = stage
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("stopped watching; the erasure goes on on the server: tracepad users erasure %s",
				erasure.ID)
		case <-time.After(erasurePoll):
		}
		body, err := r.api.Get(ctx, erasurePath(projectID, erasure.ID), nil)
		if err != nil {
			return nil, fmt.Errorf("%w; the erasure goes on on the server: tracepad users erasure %s", err, erasure.ID)
		}
		if erasure, err = decode[erasureView](body); err != nil {
			return nil, err
		}
		if erasure.ended() {
			return body, nil
		}
	}
}

// renderErased prints what an erasure took: the counts, the raw archive, and
// when the bytes it freed are overwritten.
func (r *run) renderErased(erasure erasureView) {
	t := newTable(r.opt.Stdout)
	for _, kind := range []string{"traces", "observations", "scores", "session_scores", "payloads",
		"annotation_items", "dataset_items", "media"} {
		if count, reported := erasure.Deleted[kind]; reported {
			t.row("  "+kind, strconv.FormatInt(count, 10))
		}
	}
	t.flush()
	// The user's spans in the raw archive (spec 044 #2): every batch that
	// held one was rewritten without it, or deleted. A server that does not
	// report it is one that does not erase there, and zeros would say it
	// looked and found nothing.
	if spans, reported := erasure.Deleted["raw_spans"]; reported {
		rewritten, deleted := erasure.Deleted["raw_batches_rewritten"], erasure.Deleted["raw_batches_deleted"]
		fmt.Fprintf(r.opt.Stdout, "\nremoved %d spans from %d raw batches, %d deleted\n",
			spans, rewritten+deleted, deleted)
	} else {
		fmt.Fprintf(r.opt.Stdout, "\nthe server reported nothing about its raw archive: "+
			"a server that predates erasing it keeps the user's spans there until the batches expire\n")
	}
	// What the rows left in the file is overwritten by the next pass, and
	// the one copy of the database an erasure does not rewrite goes on its
	// own date (spec 044 #11, #12).
	if erasure.Compaction.ExpectedBy != nil {
		fmt.Fprintf(r.opt.Stdout, "freed bytes are overwritten by the next sweep, expected by %s\n",
			shortTime(*erasure.Compaction.ExpectedBy))
	}
	if erasure.Backup != nil {
		fmt.Fprintf(r.opt.Stdout, "the pre-migration backup of %s is not rewritten; the first sweep after %s removes it\n",
			shortTime(erasure.Backup.CreatedAt), shortTime(erasure.Backup.RemoveAfter))
	}
}

func failure(sentence *string) string {
	if sentence == nil {
		return "the server gave no reason"
	}
	return termsafe.Text(*sentence)
}

// confirmErasure sends the confirmed erasure and says what a lost answer
// means. The server answers within `wait` whatever the erasure's length (spec
// 047 #7), so no answer is not news about a long erasure any more; but a
// connection lost after the request was written — or a proxy in front of the
// server answering 502 or 504 — leaves the caller not knowing whether the
// server recorded it, and the listing says. Before that — a refused
// connection, a name that does not resolve, a handshake that fails, an
// interrupt — nothing reached the server, and the error is passed on as it
// is; so is a refusal the server did send, a 503 included: the server answers
// that one before recording anything.
func confirmErasure(ctx context.Context, listing string,
	send func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	var written atomic.Bool
	body, err := send(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// Per attempt: the transport retries a request whose reused
		// connection turned out closed, and only the last attempt says
		// whether the server has it.
		GetConn: func(string) { written.Store(false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			written.Store(info.Err == nil)
		},
	}))
	if err == nil || !written.Load() {
		return body, err
	}
	var refusal *client.Error
	if errors.As(err, &refusal) && refusal.Status != http.StatusBadGateway &&
		refusal.Status != http.StatusGatewayTimeout {
		return body, err
	}
	return nil, fmt.Errorf("no answer from the server (%w); it may have accepted the erasure — "+
		"`%s` shows whether it did", err, listing)
}

// usersErasure prints one erasure (spec 047 #14).
func (r *run) usersErasure(ctx context.Context, args []string) error {
	fs := r.flags("users erasure")
	project := fs.String("project", "", "")
	positional, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, erasurePath(id, positional[0]), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	erasure, err := decode[erasureView](body)
	if err != nil {
		return err
	}
	out := r.opt.Stdout
	fmt.Fprintf(out, "erasure %s\n", termsafe.String(erasure.ID))
	fmt.Fprintf(out, "  state     %s\n", termsafe.String(erasure.progress()))
	// The record forgets whom it erased when it ends (#9).
	if erasure.UserID != "" {
		fmt.Fprintf(out, "  user      %s\n", termsafe.String(erasure.UserID))
	}
	fmt.Fprintf(out, "  created   %s\n", shortTime(erasure.CreatedAt))
	if erasure.StartedAt != "" {
		fmt.Fprintf(out, "  started   %s\n", shortTime(erasure.StartedAt))
	}
	if erasure.FinishedAt != "" {
		fmt.Fprintf(out, "  finished  %s\n", shortTime(erasure.FinishedAt))
	}
	if erasure.Error != nil {
		fmt.Fprintf(out, "  error     %s\n", failure(erasure.Error))
	}
	r.renderErased(erasure)
	return nil
}

// usersErasures lists the project's erasures, newest first (spec 047 #14).
func (r *run) usersErasures(ctx context.Context, args []string) error {
	fs := r.flags("users erasures")
	project := fs.String("project", "", "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}
	id, err := r.projectID(ctx, fs, *project)
	if err != nil {
		return err
	}
	body, err := r.api.Get(ctx, "/api/v1/projects/"+url.PathEscape(id)+"/erasures", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[struct {
		Erasures []erasureView `json:"erasures"`
	}](body)
	if err != nil {
		return err
	}
	if len(listing.Erasures) == 0 {
		fmt.Fprintln(r.opt.Stdout, "no erasures in the last 30 days")
		return nil
	}
	t := newTable(r.opt.Stdout, "ID", "STATE", "TRACES", "CREATED", "USER")
	for _, e := range listing.Erasures {
		t.row(termsafe.String(e.ID), termsafe.String(e.progress()),
			strconv.FormatInt(e.Deleted["traces"], 10), shortTime(e.CreatedAt), termsafe.String(e.UserID))
	}
	t.flush()
	return nil
}
