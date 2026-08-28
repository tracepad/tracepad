package server

import (
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/mcpserver"
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
		put("langfuse_ingestion_versions", versions)
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
	tables, err := s.store.TableCounts(project.ID)
	if err != nil {
		slog.Error("read table counts failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the database counts")
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

	body := object{}.
		put("version", s.version).
		put("go_version", runtime.Version()).
		put("started_at", formatTime(s.startedAt.UnixNano())).
		put("uptime_seconds", int64(time.Since(s.startedAt).Seconds())).
		put("database", object{}.
			put("size_bytes", s.store.FileSize()).
			put("rows", rows)).
		put("writer_queue", queue).
		// The endpoint map deliberately does not advertise /mcp — it is
		// a transport, not an endpoint of this API — so this is where a
		// caller finds out whether it is being served (Decision 27).
		put("mcp", object{}.
			put("enabled", s.mcp).
			put("path", mcpserver.Path).
			put("protocol_version", mcpserver.ProtocolVersion)).
		put("response_budget_bytes", s.responseBudget).
		// What retention has done and when it runs next (spec 005 #14).
		// There is no endpoint to run it now: an immediate sweep would be
		// a destructive trigger without the dry run the rest of the admin
		// surface insists on, and the cadence *is* the margin in which a
		// mistaken retention change can be corrected.
		put("sweeper", s.sweeperStatus(project.ID)).
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

// queueReporter is the part of *store.Writer the system endpoint needs. The
// handlers take a JobWriter interface, and a test writer need not report a
// queue for the endpoint to work.
type queueReporter interface {
	QueueDepth() (waiting, capacity int)
}
