-- Rebuild the search index over the leaves of the payloads rather than over
-- the JSON text around them (spec 011 #14).
--
-- No schema change: the index built by 0006 holds every key of every message
-- array as a word, which is a corpus of the envelope rather than of what the
-- observations carried. There is no way to edit an index into a different
-- opinion of what a word is, so both halves are emptied and the marker of
-- spec 011 #8 is put back, and the first start after this upgrade rebuilds
-- the index with the backfill that already exists — before the server listens,
-- at the rate `docs/retention.md` publishes.

-- The FTS rows first, by FTS5's own bulk delete: on a contentless table a
-- `DELETE` needs the rowids `search_entries` is about to lose, and
-- `delete-all` needs nothing at all.
INSERT INTO search_fts(search_fts) VALUES('delete-all');
DELETE FROM search_entries;

-- Not done, so the backfill walks every trace again.
UPDATE search_backfill SET done_at = NULL WHERE id = 1;
