-- The compaction an explicit deletion asks for (spec 044 #11): the next
-- sweeper pass merges the search index to one segment, drains the freelist and
-- truncates the write-ahead log, so what a deletion unlinked is overwritten
-- rather than left in the file. One row for the deployment: requests coalesce,
-- and what an operator needs is whether one is pending and when one last ran.
CREATE TABLE compaction (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    requested_at INTEGER,   -- Unix ns of the latest pending request; NULL when none is
    completed_at INTEGER    -- Unix ns of the last completed compaction
) STRICT;

-- The first pass after the upgrade compacts what deletions before it left:
-- search-index segments holding deleted text, and freed pages that were never
-- zeroed because secure_delete was not on yet.
INSERT INTO compaction (id, requested_at)
VALUES (1, CAST(unixepoch('subsec') * 1000 AS INTEGER) * 1000000);
