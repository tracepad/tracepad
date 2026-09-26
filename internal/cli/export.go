package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/termsafe"
)

// `tracepad export --otlp` (spec 019 #1, #5, #6): the way out. It replays the
// archive — the bytes of every accepted export, as received — into an OTLP
// receiver or onto disk, in arrival order, resumably, and says what it could
// not cover.
//
// Through the API like every other command (#2): two endpoints, the project's
// own keys, and no second reader of the SQLite file the server is writing. It
// synthesizes nothing: a trace older than the raw window is parsed rows only,
// and the number of those is reported rather than silently made up (#1).

// exportPageSize is how many rows one listing request asks for. The maximum,
// because nobody reads this listing: a page is a round trip on the way to the
// bodies, and the bodies are fetched one at a time regardless.
const exportPageSize = 500

// Retry bounds for a receiver that asks for time (spec 019 #6). A nightly
// export must not die at 03:14 for a fifteen-second blip; six attempts over
// half a minute of backoff is the difference between a blip and an outage.
const (
	defaultRetryBackoff = time.Second
	maxRetryBackoff     = 30 * time.Second
	maxAttempts         = 6
)

// progressEvery is how often the walk says where it is, on stderr, so that
// stdout stays the summary and nothing else.
const progressEvery = 100

// headerList collects a repeatable `--header k=v`.
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ",") }

func (h *headerList) Set(value string) error {
	*h = append(*h, value)
	return nil
}

// export runs the command.
func (r *run) export(ctx context.Context, args []string) error {
	fs := r.flags("export")
	var (
		otlp     bool
		to       string
		dir      string
		headers  headerList
		compress bool
		since    string
		until    string
		after    string
		dryRun   bool
		allowKey bool
	)
	fs.BoolVar(&otlp, "otlp", false, "")
	fs.StringVar(&to, "to", "", "")
	fs.StringVar(&dir, "dir", "", "")
	fs.Var(&headers, "header", "")
	fs.BoolVar(&compress, "gzip", false, "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	fs.StringVar(&after, "after", "", "")
	fs.BoolVar(&dryRun, "dry-run", false, "")
	fs.BoolVar(&allowKey, "allow-tracepad-key", false, "")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	// `--otlp` is required rather than assumed. It is the only format
	// there is today, and naming it is what keeps the command line the
	// same when there is a second (spec 019, CLI contract).
	if !otlp {
		return usageErrorf("export needs --otlp: it says what is being exported, " +
			"and the archive is OTLP")
	}
	switch {
	case to == "" && dir == "":
		return usageErrorf("export needs a destination: --to <url> or --dir <path>")
	case to != "" && dir != "":
		return usageErrorf("export takes one destination, not both --to and --dir")
	case dir != "" && len(headers) > 0:
		return usageErrorf("--header applies to --to; a directory takes no headers")
	case dir != "" && compress:
		return usageErrorf("--gzip applies to --to; a directory holds the bodies as they are")
	case dir != "" && allowKey:
		return usageErrorf("--allow-tracepad-key applies to --to; a directory is sent no headers")
	}
	// Resolved and checked before anything is asked of either server, so a
	// header carrying a Tracepad key is refused before a byte is sent
	// (spec 019 #14).
	var resolved map[string]string
	if to != "" {
		var err error
		if resolved, err = r.exportHeaders(to, headers, allowKey); err != nil {
			return err
		}
	}
	if after == "" && wasGiven(fs, "after") {
		return usageErrorf("--after needs the cursor the previous run printed; it was passed empty")
	}

	window, err := r.exportWindow(since, until)
	if err != nil {
		return err
	}

	// What the archive holds, before anything is sent: the report ends with
	// these numbers, and the refusal below is one of them (spec 019 #4).
	archive, err := r.rawArchive(ctx)
	if err != nil {
		return err
	}
	summary := exportSummary{TracesBeforeWindow: archive.TracesBeforeWindow, LastCursor: after}

	if archive.Batches == 0 && !archive.Enabled {
		// Nothing was ever kept, so there is nothing to replay and no
		// amount of retrying will change that. A dry run says the same
		// thing and exits 0, because asking was not a mistake.
		r.reportExport(summary, dryRun)
		if dryRun {
			return nil
		}
		return errors.New("raw storage is off; nothing to replay " +
			"(set TRACEPAD_STORE_RAW=1 on the server to start keeping bodies)")
	}

	if dryRun {
		matching, capped, err := r.countBatches(ctx, window)
		if err != nil {
			return err
		}
		summary.MatchingWindow = &matching
		summary.MatchingCapped = capped
		summary.Resuming = after != ""
		r.reportExport(summary, true)
		return nil
	}

	sink, err := r.destination(to, dir, resolved, compress, after != "")
	if err != nil {
		return err
	}
	defer sink.close()

	err = r.replay(ctx, sink, window, after, &summary)
	r.reportExport(summary, false)
	return err
}

// exportWindow resolves `--since` and `--until` into the listing's parameters.
type exportWindow struct{ since, until string }

func (r *run) exportWindow(since, until string) (exportWindow, error) {
	var window exportWindow
	var err error
	if window.since, err = r.instant("--since", since); err != nil {
		return window, err
	}
	if window.until, err = r.instant("--until", until); err != nil {
		return window, err
	}
	return window, nil
}

// rawBatchRow is one row of `GET /api/v1/raw`, and one line of a `--dir`
// manifest: the same shape, because the manifest is the listing.
type rawBatchRow struct {
	ID              int64  `json:"id"`
	ReceivedAt      string `json:"received_at"`
	Dialect         string `json:"dialect"`
	ContentType     string `json:"content_type"`
	ContentEncoding string `json:"content_encoding"`
	SizeBytes       int64  `json:"size_bytes"`
}

type rawListing struct {
	Batches     []rawBatchRow `json:"batches"`
	NextCursor  *string       `json:"next_cursor"`
	Total       int64         `json:"total"`
	TotalCapped bool          `json:"total_capped"`
}

// rawArchive is the `raw` block of `GET /api/v1/system` (spec 019 #4).
type rawArchive struct {
	Enabled            bool    `json:"enabled"`
	Batches            int64   `json:"batches"`
	Bytes              int64   `json:"bytes"`
	OldestReceivedAt   *string `json:"oldest_received_at"`
	NewestReceivedAt   *string `json:"newest_received_at"`
	TracesBeforeWindow int64   `json:"traces_before_window"`
}

func (r *run) rawArchive(ctx context.Context) (rawArchive, error) {
	body, err := r.api.Get(ctx, "/api/v1/system", nil)
	if err != nil {
		return rawArchive{}, err
	}
	system, err := decode[struct {
		Raw rawArchive `json:"raw"`
	}](body)
	if err != nil {
		return rawArchive{}, err
	}
	return system.Raw, nil
}

// countBatches answers what a dry run reports: how many batches the **window**
// holds, without fetching one.
//
// The window and not the remainder. The API's count is over the filters and
// never over the page — one meaning for `count` across the whole read API
// (spec 009 #4) — so a cursor does not narrow it, and a dry run resumed with
// `--after` would otherwise read as "this many left to send" when it is "this
// many in the window". The cursor is therefore not sent at all, and both the
// summary's field name and the line printed beside it say which number this is.
func (r *run) countBatches(ctx context.Context, window exportWindow) (int64, bool, error) {
	query := url.Values{"limit": {"1"}, "count": {"1"}}
	addSome(query, "since", window.since)
	addSome(query, "until", window.until)
	body, err := r.api.Get(ctx, "/api/v1/raw", query)
	if err != nil {
		return 0, false, listingError(err)
	}
	listing, err := decode[rawListing](body)
	if err != nil {
		return 0, false, err
	}
	return listing.Total, listing.TotalCapped, nil
}

// replay walks the archive and hands each body to the destination. It is the
// whole of Decision 6: nothing is skipped except a batch the sweeper took, and
// a stop leaves a cursor that resumes at the batch that failed.
func (r *run) replay(ctx context.Context, sink destination, window exportWindow,
	after string, summary *exportSummary) error {
	cursor := after
	for {
		query := url.Values{"limit": {strconv.Itoa(exportPageSize)}}
		addSome(query, "since", window.since)
		addSome(query, "until", window.until)
		addSome(query, "cursor", cursor)
		body, err := r.api.Get(ctx, "/api/v1/raw", query)
		if err != nil {
			return listingError(err)
		}
		listing, err := decode[rawListing](body)
		if err != nil {
			return err
		}

		for _, row := range listing.Batches {
			if err := r.replayOne(ctx, sink, row, summary); err != nil {
				return err
			}
			// Only after the batch is through: the cursor is where a
			// resume starts *after*, so it may never name a batch the
			// receiver has not taken (spec 019 #6). A row whose arrival
			// will not parse leaves the previous cursor standing, which
			// resumes earlier rather than later — the safe direction,
			// because re-sending is safe and a gap is not.
			if at := cursorOf(row); at != "" {
				summary.LastCursor = at
			}
		}
		if listing.NextCursor == nil {
			return nil
		}
		cursor = *listing.NextCursor
	}
}

// replayOne fetches one body and sends it.
func (r *run) replayOne(ctx context.Context, sink destination, row rawBatchRow,
	summary *exportSummary) error {
	body, _, err := r.api.Fetch(ctx, fmt.Sprintf("/api/v1/raw/%d", row.ID))
	if err != nil {
		var refusal *client.Error
		if errors.As(err, &refusal) && refusal.Status == 404 {
			// The archive moved under us, not the receiver: the
			// sweeper took this batch between the page and the
			// fetch. It is the one skip this command makes, and it
			// is named in the summary (spec 019, edge cases).
			summary.Swept++
			fmt.Fprintf(r.opt.Stderr,
				"tracepad: batch %d was swept while this export was running; skipping it\n", row.ID)
			return nil
		}
		return err
	}

	partial, err := sink.send(ctx, row, body)
	if err != nil {
		var stop *stopError
		if errors.As(err, &stop) {
			summary.StoppedAt = &stoppedAt{ID: row.ID, Status: stop.status, Message: stop.message}
		}
		return err
	}
	if partial != "" {
		// A 2xx that reports rejected spans is the receiver describing
		// its own mapping, not a failure of this transfer: it has the
		// bytes. Counted and passed on, never retried (spec 019 #6).
		summary.PartialSuccess++
		fmt.Fprintf(r.opt.Stderr, "tracepad: batch %d: the receiver reported %s\n", row.ID, termsafe.String(partial))
	}

	summary.Sent++
	summary.Bytes += int64(len(body))
	if summary.FirstReceivedAt == "" {
		summary.FirstReceivedAt = row.ReceivedAt
	}
	summary.LastReceivedAt = row.ReceivedAt
	if summary.Sent%progressEvery == 0 {
		fmt.Fprintf(r.opt.Stderr, "tracepad: %d batches, %s, through %s\n",
			summary.Sent, byteSize(int(summary.Bytes)), termsafe.String(row.ReceivedAt))
	}
	return nil
}

// cursorOf rebuilds the listing's cursor for a row. The grammar is the
// server's — base64 of `received_at:id` in nanoseconds — and it is rebuilt
// here rather than taken from the page because a resume point has to name a
// *row*, and a page's `next_cursor` names its last one.
func cursorOf(row rawBatchRow) string {
	at, err := time.Parse(time.RFC3339Nano, row.ReceivedAt)
	if err != nil {
		return ""
	}
	return encodeRawCursor(at.UnixNano(), row.ID)
}

// exportSummary is what the command answers with (spec 019, CLI contract).
type exportSummary struct {
	Sent  int64 `json:"sent"`
	Bytes int64 `json:"bytes"`
	// Swept counts the batches the sweeper took while this export ran: the
	// only thing it skips, named so that a gap is never a surprise.
	Swept int64 `json:"swept"`
	// PartialSuccess counts the batches a receiver took and reported spans
	// of its own that it would not keep.
	PartialSuccess     int64      `json:"partial_success"`
	FirstReceivedAt    string     `json:"first_received_at"`
	LastReceivedAt     string     `json:"last_received_at"`
	LastCursor         string     `json:"last_cursor"`
	TracesBeforeWindow int64      `json:"traces_before_window"`
	StoppedAt          *stoppedAt `json:"stopped_at"`
	// MatchingWindow is a dry run's answer and is absent from a real one,
	// where `sent` is the number that matters. Named for the window rather
	// than "matching" because that is exactly what it counts: `--after`
	// does not narrow it, and a field called `matching` beside a cursor
	// would read as the remainder (spec 009 #4).
	MatchingWindow *int64 `json:"matching_window,omitempty"`
	// MatchingCapped says the count stopped at the API's cap, so the number
	// above is a floor rather than the total.
	MatchingCapped bool `json:"matching_window_capped,omitempty"`
	// Resuming records that `--after` was given, which is what makes the
	// window count larger than what this run would actually send.
	Resuming bool `json:"resuming,omitempty"`
}

type stoppedAt struct {
	ID      int64  `json:"id"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// reportExport prints the summary: JSON when asked or when stdout is not a
// terminal, and otherwise a few lines a person reads. The cursor is the last
// line in both, because it is what a resume is typed from.
func (r *run) reportExport(summary exportSummary, dryRun bool) {
	if r.wantJSON() {
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return
		}
		_ = r.emit(encoded)
		return
	}

	if dryRun {
		switch {
		case summary.MatchingWindow == nil:
			fmt.Fprintln(r.opt.Stdout, "the archive is empty; nothing to send")
		default:
			// Named as the window's count, never as a remainder: the
			// API's count is over the filters and not over the page, so
			// `--after` does not narrow it.
			count := fmt.Sprintf("%d", *summary.MatchingWindow)
			if summary.MatchingCapped {
				count += "+"
			}
			if summary.Resuming {
				fmt.Fprintf(r.opt.Stdout,
					"%s batches in the window; the resume starts inside it, so fewer will be sent\n",
					count)
			} else {
				fmt.Fprintf(r.opt.Stdout, "%s batches in the window\n", count)
			}
			fmt.Fprintln(r.opt.Stdout, "  nothing was sent")
		}
	} else {
		fmt.Fprintf(r.opt.Stdout, "%d batches, %s\n", summary.Sent, byteSize(int(summary.Bytes)))
		if summary.FirstReceivedAt != "" {
			fmt.Fprintf(r.opt.Stdout, "  covering   %s .. %s\n",
				shortTime(summary.FirstReceivedAt), shortTime(summary.LastReceivedAt))
		}
	}
	if summary.Swept > 0 {
		fmt.Fprintf(r.opt.Stdout, "  swept      %d batches went to retention while this ran\n", summary.Swept)
	}
	if summary.PartialSuccess > 0 {
		fmt.Fprintf(r.opt.Stdout, "  partial    %d batches the receiver took and reported spans of\n",
			summary.PartialSuccess)
	}
	// The honest edge of the promise (spec 019 #1): a trace older than the
	// raw window is parsed rows only, and the operator learns it here
	// rather than from a gap at the receiver.
	fmt.Fprintf(r.opt.Stdout, "  not covered %d traces started before the archive begins\n",
		summary.TracesBeforeWindow)
	if stop := summary.StoppedAt; stop != nil {
		// A status of zero is a destination that never answered — a
		// transport error, or a directory that could not be written —
		// and printing "0" for it would read as a status code.
		if stop.Status == 0 {
			fmt.Fprintf(r.opt.Stdout, "  stopped at batch %d: %s\n", stop.ID, termsafe.String(stop.Message))
		} else {
			fmt.Fprintf(r.opt.Stdout, "  stopped at batch %d: %d %s\n",
				stop.ID, stop.Status, termsafe.String(stop.Message))
		}
	}
	if summary.LastCursor != "" {
		fmt.Fprintf(r.opt.Stdout, "--after %s\n", termsafe.String(summary.LastCursor))
	}
}

// listingError turns the archive listing's refusals into the exit code they
// deserve. A 400 from it is always a mistake in how the command was typed — a
// window that does not parse, a cursor that contradicts it — so it is a usage
// error rather than a failure of the transfer (spec 019, edge cases).
func listingError(err error) error {
	var refusal *client.Error
	if errors.As(err, &refusal) && refusal.Status == 400 {
		return usageErrorf("%s", refusal.Message)
	}
	return err
}
