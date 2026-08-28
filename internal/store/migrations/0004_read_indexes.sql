-- Read-path indexes (spec 004). No new tables.

-- The trace list sorts by (timestamp DESC, id DESC) and pages by that same
-- key, so the tie-break belongs in the index: without it SQLite sorts every
-- page through a temporary B-tree and cannot seek to the cursor, which is the
-- one thing keyset pagination exists to avoid (spec 003 #25, spec 004 #4).
DROP INDEX idx_traces_timestamp;
CREATE INDEX idx_traces_timestamp ON traces(project_id, timestamp DESC, id DESC);

-- `GET /api/v1/observations/{id}/io` addresses a span by its 16-hex id alone;
-- without this index that lookup is a full scan of the project's spans, and
-- the ambiguity check (which traces carry this span id?) is the same scan
-- again.
CREATE INDEX idx_observations_span ON observations(project_id, id);

-- A trace whose every span carried an unset start time used to leave
-- `timestamp` NULL, and a NULL sort key is invisible to the keyset predicate
-- `(timestamp, id) < (?, ?)`: such a trace would be listed only while it fit
-- on the first page and then silently disappear from every later one
-- (spec 004 Decision 26). Ingest now falls back to the smallest start time it
-- has; this backfills the rows written before it did.
UPDATE traces SET timestamp = COALESCE(
        (SELECT MIN(o.start_time) FROM observations o
         WHERE o.project_id = traces.project_id AND o.trace_id = traces.id),
        0)
 WHERE timestamp IS NULL;
