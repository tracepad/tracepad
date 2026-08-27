package server

import (
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"
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

// counters holds every since-start number the system endpoint reports.
type counters struct {
	mu sync.Mutex
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

func newCounters() *counters {
	return &counters{dialects: map[string]*dialectCounter{}, sdkVersions: map[string]int64{}}
}

// observeBatch records one accepted export.
func (c *counters) observeBatch(dialect string, stored, skipped, unreadable int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.dialects[dialect]
	if entry == nil {
		entry = &dialectCounter{}
		c.dialects[dialect] = entry
	}
	entry.Batches++
	entry.SpansStored += stored
	entry.SpansSkipped += skipped
	c.unreadableSpans += unreadable
}

// observeRejectedBatch records a body that could not be decoded.
func (c *counters) observeRejectedBatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejectedBatches++
}

// observeSDKVersion records an ingestion-version header.
func (c *counters) observeSDKVersion(version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Bounded on purpose: the header is client-controlled, and an
	// unbounded map keyed by it is a memory leak a client can trigger.
	if len(c.sdkVersions) >= maxTrackedSDKVersions {
		if _, known := c.sdkVersions[version]; !known {
			return
		}
	}
	c.sdkVersions[version]++
}

// maxTrackedSDKVersions bounds the distinct header values kept.
const maxTrackedSDKVersions = 64

// snapshot renders the counters for the system endpoint.
func (c *counters) snapshot() object {
	c.mu.Lock()
	defer c.mu.Unlock()

	names := make([]string, 0, len(c.dialects))
	for name := range c.dialects {
		names = append(names, name)
	}
	sort.Strings(names)
	dialects := object{}
	for _, name := range names {
		entry := c.dialects[name]
		dialects = dialects.put(name, object{}.
			put("batches", entry.Batches).
			put("spans_stored", entry.SpansStored).
			put("spans_skipped", entry.SpansSkipped))
	}

	versions := make([]string, 0, len(c.sdkVersions))
	for version := range c.sdkVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)

	return object{}.
		put("dialects", dialects).
		put("rejected_batches", c.rejectedBatches).
		put("unreadable_resource_spans", c.unreadableSpans).
		put("langfuse_ingestion_versions", versions)
}

// handleSystem reports what this process knows about itself.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	_ = project
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tables, err := s.store.TableCounts()
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
		put("response_budget_bytes", s.responseBudget).
		// The counters are since this process started and say so: an
		// honest process-lifetime number now beats a metrics subsystem
		// later (#10).
		put("counters", s.counters.snapshot().put("since", formatTime(s.startedAt.UnixNano())))
	writeJSON(w, http.StatusOK, body)
}

// queueReporter is the part of *store.Writer the system endpoint needs. The
// handlers take a JobWriter interface, and a test writer need not report a
// queue for the endpoint to work.
type queueReporter interface {
	QueueDepth() (waiting, capacity int)
}
