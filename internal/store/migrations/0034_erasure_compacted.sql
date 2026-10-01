-- When the compaction an erasure asked for completed (spec 044 #22): the
-- erasure's own answer, where `/system`'s compaction stamps are the
-- deployment's and no project's to read. Stamped by the pass that covered the
-- erasure's latest request, NULL until then.
ALTER TABLE erasures ADD COLUMN compacted_at INTEGER;

-- What the store already knows: with nothing pending, every request was
-- covered by a pass, and the latest pass's completion is when that was true at
-- the latest. With a request pending, an erasure's own may be in it, so it is
-- left for the next pass to stamp.
UPDATE erasures
   SET compacted_at = (SELECT completed_at FROM compaction WHERE id = 1)
 WHERE compaction > 0
   AND EXISTS (SELECT 1 FROM compaction WHERE id = 1 AND requested_at IS NULL AND completed_at IS NOT NULL);
