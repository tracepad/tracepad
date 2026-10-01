-- When the compaction an erasure asked for completed (spec 044 #22): the
-- erasure's own answer, where `/system`'s compaction stamps are the
-- deployment's and no project's to read.
--
-- Which pass covered which request is counted, not timed. `requested_at` is a
-- clock, and a clock steps back; `requests` only goes up. Every request takes
-- the next number, an erasure keeps the number of its latest, and a pass covers
-- every number up to the one it read when it started.
ALTER TABLE compaction ADD COLUMN requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE erasures ADD COLUMN compaction_request INTEGER NOT NULL DEFAULT 0;
ALTER TABLE erasures ADD COLUMN compacted_at INTEGER;

-- What the store already knows. With nothing pending, every request was
-- covered by a pass, and the latest pass's completion is when that was true at
-- the latest. With a request pending, an erasure's own may be in it: it is
-- request 1, and the next pass, which reads 1, stamps it.
UPDATE compaction SET requests = CASE WHEN requested_at IS NULL THEN 0 ELSE 1 END WHERE id = 1;
UPDATE erasures
   SET compacted_at = (SELECT completed_at FROM compaction WHERE id = 1)
 WHERE compaction > 0
   AND EXISTS (SELECT 1 FROM compaction WHERE id = 1 AND requested_at IS NULL AND completed_at IS NOT NULL);
UPDATE erasures SET compaction_request = 1
 WHERE compaction > 0 AND compacted_at IS NULL
   AND EXISTS (SELECT 1 FROM compaction WHERE id = 1 AND requested_at IS NOT NULL);

-- The erasures a finished pass may stamp, which are the few still waiting: a
-- seek inside the writer's transaction, never a walk of every erasure there
-- has been.
CREATE INDEX idx_erasures_awaiting_compaction ON erasures(compaction_request)
    WHERE compacted_at IS NULL AND compaction_request > 0;
