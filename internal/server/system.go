package server

import (
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/store"
)

// Self-diagnosability (design §3.4, spec 004 #10): the numbers an agent needs
// to answer "is this server healthy and is it seeing my data" without a
// metrics subsystem, a dashboard or a human.
//
// The counters are in-memory and say so. Process-lifetime numbers that are
// honest about their scope beat a persistence story nobody asked for; the
// `since` field is what makes them readable.

// dialectCounter is one attribute dialect's share of the ingest traffic
// (spec 002 #17: the drift signal finally gets a surface).
type dialectCounter struct {
	Batches      int64
	SpansStored  int64
	SpansSkipped int64
}

// projectCounters is one tenant's share of the ingest traffic.
type projectCounters struct {
	// dialects is keyed by mapping.Dialect*.
	dialects map[string]*dialectCounter
	// rejectedBatches counts bodies that never reached a dialect: they
	// did not decode as OTLP at all.
	rejectedBatches int64
	// unreadableSpans counts ResourceSpans blocks the decoder had to skip
	// inside an otherwise readable body (spec 002 #13).
	unreadableSpans int64
	// sdkVersions is every distinct x-langfuse-ingestion-version seen,
	// with how often. An SDK that starts sending a version we have never
	// mapped shows up here before it shows up as a bug report.
	sdkVersions map[string]int64
	// orphanTraces counts trace deliveries that named a run this project
	// does not have (spec 014 #3): a harness that stamps a wrong id must
	// find out before it reads an empty run. unknownRuns is which ids,
	// so each is logged once per process rather than once per span.
	orphanTraces int64
	unknownRuns  map[string]bool
	// readsTimedOut and readsRefusedBusy count the reads the read gate
	// stopped at their deadline and refused for want of a slot (spec 043
	// #21).
	readsTimedOut    int64
	readsRefusedBusy int64
	// exportsOverSpanCap and bodiesRefusedForBudget count the exports
	// refused for carrying more spans than TRACEPAD_MAX_SPANS_PER_REQUEST
	// and the bodies refused because the body budget was spent (spec 043
	// #21).
	exportsOverSpanCap     int64
	bodiesRefusedForBudget int64
	// scoresOverRowCap, datasetItemsOverRowCap and queueAddsOverTargetCap
	// count the array writes refused for carrying more values than their
	// limit: 10,000 scores or dataset items, 1,000 queue targets (spec 043
	// #40).
	scoresOverRowCap       int64
	datasetItemsOverRowCap int64
	queueAddsOverTargetCap int64
}

// counters holds every since-start number the system endpoint reports, kept
// per project.
//
// Per project because a project key is a tenant credential: process-wide
// totals would tell one tenant how much traffic another was sending and which
// SDKs it ran (Decision 33). Every observation point is reached after the
// request has authenticated, so there is no traffic here without a project to
// file it under.
type counters struct {
	mu       sync.Mutex
	projects map[string]*projectCounters
}

func newCounters() *counters {
	return &counters{projects: map[string]*projectCounters{}}
}

// forProject returns a project's counters, creating them on first sight. The
// map is keyed by ids that came out of the database, so it is bounded by the
// number of projects rather than by anything a client sends.
func (c *counters) forProject(projectID string) *projectCounters {
	entry := c.projects[projectID]
	if entry == nil {
		entry = &projectCounters{
			dialects:    map[string]*dialectCounter{},
			sdkVersions: map[string]int64{},
			unknownRuns: map[string]bool{},
		}
		c.projects[projectID] = entry
	}
	return entry
}

// observeBatch records one accepted export.
func (c *counters) observeBatch(projectID, dialect string, stored, skipped, unreadable int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	entry := project.dialects[dialect]
	if entry == nil {
		entry = &dialectCounter{}
		project.dialects[dialect] = entry
	}
	entry.Batches++
	entry.SpansStored += stored
	entry.SpansSkipped += skipped
	project.unreadableSpans += unreadable
}

// observeRejectedBatch records a body that could not be decoded.
func (c *counters) observeRejectedBatch(projectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forProject(projectID).rejectedBatches++
}

// observeOverSpanCap records an export refused for its number of spans, which
// is a rejected batch too (spec 043 #10).
func (c *counters) observeOverSpanCap(projectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	project.rejectedBatches++
	project.exportsOverSpanCap++
}

// batchKind is which array write a refusal for its number of values was
// about.
type batchKind int

const (
	batchScores batchKind = iota
	batchDatasetItems
	batchQueueTargets
)

// observeOverBatchCap records an array write refused for carrying more values
// than its limit (spec 043 #40). Each route has a counter of its own, because
// their limits are different ones and a client sending a thousand and one
// targets is not one sending ten thousand and one scores.
func (c *counters) observeOverBatchCap(projectID string, kind batchKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	switch kind {
	case batchScores:
		project.scoresOverRowCap++
	case batchDatasetItems:
		project.datasetItemsOverRowCap++
	case batchQueueTargets:
		project.queueAddsOverTargetCap++
	}
}

// observeBodyRefused records a body the budget could not hold, in the project
// it was about only; one about no project is counted nowhere, as a read is.
func (c *counters) observeBodyRefused(projectID string) {
	if projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forProject(projectID).bodiesRefusedForBudget++
}

// observeSDKVersion records an ingestion-version header.
func (c *counters) observeSDKVersion(projectID, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	// Bounded on purpose: the header is client-controlled, and an
	// unbounded map keyed by it is a memory leak a client can trigger.
	if len(project.sdkVersions) >= maxTrackedSDKVersions {
		if _, known := project.sdkVersions[version]; !known {
			return
		}
	}
	project.sdkVersions[version]++
}

// maxTrackedSDKVersions bounds the distinct header values kept per project.
const maxTrackedSDKVersions = 64

// observeOrphanRuns records the traces of one export that named a run the
// project does not have, and returns the ids not seen before, for the caller
// to log once each (spec 014 #3).
func (c *counters) observeOrphanRuns(projectID string, runIDs []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	project.orphanTraces += int64(len(runIDs))
	var fresh []string
	seen := make(map[string]bool, len(runIDs))
	for _, id := range runIDs {
		// Two passes of dedup, because they answer different questions.
		// `seen` is this export's own: a batch of a hundred traces
		// stamped with one wrong id is one mistake and one log line,
		// which has to hold whether or not the id fits in the map below.
		if seen[id] || project.unknownRuns[id] {
			continue
		}
		seen[id] = true
		// Bounded like the SDK versions: the id is client-controlled,
		// and an unbounded map keyed by it is a memory leak a client can
		// trigger. Past the bound an id is logged again on its next
		// export — noisier, and the failure that cannot be turned into
		// a leak.
		if len(project.unknownRuns) < maxTrackedUnknownRuns {
			project.unknownRuns[id] = true
		}
		fresh = append(fresh, id)
	}
	return fresh
}

// maxTrackedUnknownRuns bounds the distinct unknown run ids kept per project.
const maxTrackedUnknownRuns = 256

// snapshot renders one project's counters for the system endpoint. A project
// that has never exported anything gets zeroes, not an absence: "nothing has
// arrived" is an answer.
func (c *counters) snapshot(projectID string) object {
	c.mu.Lock()
	defer c.mu.Unlock()

	project := c.projects[projectID]
	if project == nil {
		project = &projectCounters{}
	}

	names := make([]string, 0, len(project.dialects))
	for name := range project.dialects {
		names = append(names, name)
	}
	sort.Strings(names)
	dialects := object{}
	for _, name := range names {
		entry := project.dialects[name]
		dialects = dialects.put(name, object{}.
			put("batches", entry.Batches).
			put("spans_stored", entry.SpansStored).
			put("spans_skipped", entry.SpansSkipped))
	}

	versions := make([]string, 0, len(project.sdkVersions))
	for version := range project.sdkVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)

	return object{}.
		put("dialects", dialects).
		put("rejected_batches", project.rejectedBatches).
		put("unreadable_resource_spans", project.unreadableSpans).
		put("langfuse_ingestion_versions", versions).
		put("exports_over_span_cap", project.exportsOverSpanCap).
		put("bodies_refused_for_budget", project.bodiesRefusedForBudget).
		put("reads_timed_out", project.readsTimedOut).
		put("reads_refused_busy", project.readsRefusedBusy).
		put("scores_over_row_cap", project.scoresOverRowCap).
		put("dataset_items_over_row_cap", project.datasetItemsOverRowCap).
		put("queue_adds_over_target_cap", project.queueAddsOverTargetCap)
}

// orphanTraces reports one project's count of trace deliveries that named a
// run it does not have.
func (c *counters) orphanTraces(projectID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if project := c.projects[projectID]; project != nil {
		return project.orphanTraces
	}
	return 0
}

// handleSystem reports what this process knows about itself.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Row counts are the asking project's own. A project key is a tenant
	// credential, and whole-table counts told one tenant how much data the
	// others held and how many keys they had; cross-project access is what
	// the admin token of design §6.3 is for, and it does not exist yet
	// (Decision 33).
	tables, err := s.store.TableCounts(r.Context(), project.ID)
	if err != nil {
		readFailed(w, r, "failed to read the database counts", err)
		return
	}
	rows := object{}
	for _, table := range tables {
		rows = rows.put(table.Table, table.Rows)
	}

	queue := object{}
	if writer, ok := s.writer.(queueReporter); ok {
		waiting, capacity := writer.QueueDepth()
		queue = queue.put("waiting", waiting).put("capacity", capacity)
	}
	// How many traces the project's runs keep out of the sweep (spec 014
	// #13) — the size of retention's one exception, so the operator sees
	// why the file did not shrink — beside how many traces named a run
	// that does not exist (spec 014 #3).
	pinned, err := s.store.PinnedTraces(r.Context(), project.ID)
	if err != nil {
		readFailed(w, r, "failed to count the pinned traces", err)
		return
	}

	// What the archive holds and how far back it reaches (spec 019 #4):
	// the numbers the export's report ends with, and the ones an operator
	// deciding whether to shorten `raw_retention_days` needs before.
	raw, err := s.rawBlock(r.Context(), project.ID)
	if err != nil {
		readFailed(w, r, "failed to summarise the raw archive", err)
		return
	}

	// What media costs this project, beside the setting that decides it
	// (spec 041 #11).
	media, err := s.mediaBlock(r.Context(), project)
	if err != nil {
		readFailed(w, r, "failed to summarise the media", err)
		return
	}

	compaction, err := s.store.Compaction(r.Context())
	if err != nil {
		readFailed(w, r, "failed to read the compaction state", err)
		return
	}

	body := object{}.
		put("version", s.version).
		put("go_version", runtime.Version()).
		put("started_at", formatTime(s.startedAt.UnixNano())).
		put("uptime_seconds", int64(time.Since(s.startedAt).Seconds())).
		put("database", object{}.
			put("size_bytes", s.store.FileSize()).
			put("rows", rows)).
		put("writer_queue", queue).
		// Request bodies held in memory now against
		// TRACEPAD_BODY_BUDGET_BYTES — the deployment's, like the queue
		// (spec 043 #21).
		put("body_budget", object{}.
			put("held_bytes", s.bodies.heldBytes()).
			put("capacity_bytes", s.bodies.capacityBytes())).
		// Reads being served now against TRACEPAD_READ_CONCURRENCY — the
		// deployment's, like the queue (spec 043 #21).
		put("read_slots", object{}.
			put("busy", s.reads.busy()).
			put("capacity", s.reads.capacity())).
		// The endpoint map deliberately does not advertise /mcp — it is
		// a transport, not an endpoint of this API — so this is where a
		// caller finds out whether it is being served (Decision 27).
		put("mcp", object{}.
			put("enabled", s.mcp).
			put("path", mcpserver.Path).
			put("protocol_version", mcpserver.ProtocolVersion)).
		// The source this very request counts as for the limit on password
		// checks — the one question every proxy deployment has to answer,
		// and cannot answer without asking the server. Only the asker's own:
		// the limit's counts are every tenant's sign-ins, and the list of
		// trusted proxies is the deployment's topology (spec 046 #13, #16).
		put("source", sourceText(sourceOf(s.clientAddress(r)))).
		put("response_budget_bytes", s.responseBudget).
		// What retention has done and when it runs next (spec 005 #14).
		// There is no endpoint to run it now: an immediate sweep would be
		// a destructive trigger without the dry run the rest of the admin
		// surface insists on, and the cadence *is* the margin in which a
		// mistaken retention change can be corrected.
		put("sweeper", s.sweeperStatus(project.ID)).
		put("runs", object{}.
			put("pinned_traces", pinned).
			put("orphan_traces", s.counters.orphanTraces(project.ID))).
		put("raw", raw).
		put("media", media).
		// Whether an explicit deletion is still waiting for the pass that
		// overwrites what it unlinked, and when one last finished
		// (spec 044 #11). The deployment's, not the project's: one file,
		// one compaction, and neither stamp names anybody.
		put("compaction", compactionBlock(compaction)).
		// The counters are since this process started and say so: an
		// honest process-lifetime number now beats a metrics subsystem
		// later (#10). They are this project's, for the same reason the
		// row counts are (Decision 33).
		put("counters", s.counters.snapshot(project.ID).
			put("since", formatTime(s.startedAt.UnixNano())))
	writeJSON(w, http.StatusOK, body)
}

// sweeperStatus renders the retention sweeper's state. The deletion counts are
// the asking project's own, for the same reason the row counts are
// (spec 004 Decision 33); the schedule is an operator fact and names nobody.
func (s *Server) sweeperStatus(projectID string) object {
	if s.sweeper == nil {
		return object{}.put("enabled", false)
	}
	status := s.sweeper.Status(projectID)
	body := object{}.
		put("enabled", true).
		put("interval_seconds", int64(status.Interval.Seconds())).
		put("next_run", formatTime(status.NextRun)).
		put("since", formatTime(status.Since)).
		put("traces_deleted", status.TracesDeleted).
		put("raw_batches_deleted", status.RawBatchesDeleted)
	// A sweeper that has not run yet says so rather than reporting the
	// epoch, which would read as "ran in 1970".
	if status.LastRun == 0 {
		return body.put("last_run", nil)
	}
	return body.put("last_run", formatTime(status.LastRun))
}

// compactionBlock renders the deployment's compaction state for `/system`:
// each stamp RFC 3339, or null for none.
func compactionBlock(state store.CompactionState) object {
	return object{}.
		put("requested_at", formatInstant(state.RequestedAt)).
		put("completed_at", formatInstant(state.CompletedAt))
}

// compactionAnswer is what the confirmed answer of an explicit deletion says
// about the compaction *it* asked for (spec 044 #11): when it asked, and the
// pass that will have run it. Both null when this request deleted nothing and
// so asked for nothing — whatever another request left pending is not its
// answer to give.
func (s *Server) compactionAnswer(requested int64) object {
	// Each key once: `object` is a list, and a second `put` of a key writes
	// it twice rather than replacing it.
	var requestedAt, expectedBy any
	if requested != 0 {
		requestedAt = formatTime(requested)
		if s.sweeper != nil {
			expectedBy = formatTime(s.sweeper.ExpectedBy())
		}
	}
	return object{}.put("requested_at", requestedAt).put("expected_by", expectedBy)
}

// backupAnswer names the newest pre-migration backup — the one copy of the
// database an erasure does not reach — with the moment after which the
// sweeper's next pass removes it (spec 044 #12). "After", not "at": a pass
// runs on its interval, and not at all while the server is down. Nil when
// there is none, and the field is then absent.
func (s *Server) backupAnswer() any {
	backup := s.store.PreMigrationBackup()
	if backup == nil {
		return nil
	}
	return object{}.
		put("created_at", formatTime(backup.CreatedAt)).
		put("remove_after", formatTime(backup.RemoveAfter))
}

// queueReporter is the part of *store.Writer the system endpoint needs. The
// handlers take a JobWriter interface, and a test writer need not report a
// queue for the endpoint to work.
type queueReporter interface {
	QueueDepth() (waiting, capacity int)
}
