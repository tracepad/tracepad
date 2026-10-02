-- A raw batch's number is its project's own (spec 019 #17). The id the API
-- showed was the table's rowid, one sequence for every tenant: the gaps in a
-- project's own listing were how many batches the others had sent, and when.
-- The rowid stays the table's key and what the media refs point at; it no
-- longer leaves the server.
--
-- The numbers live in a table of their own rather than a column of
-- raw_batches. A column would be filled by rewriting every row, and a row of
-- that table is a whole export body: the upgrade would copy the archive
-- through the write-ahead log. This table is filled from the arrival index
-- alone, and the listing pages on it.
CREATE TABLE raw_batch_numbers (
    batch_id    INTEGER PRIMARY KEY REFERENCES raw_batches(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL,
    number      INTEGER NOT NULL,     -- 1, 2, 3… within the project
    received_at INTEGER NOT NULL      -- the batch's, for the listing's keyset
) STRICT;

-- The archive as it stands, numbered within each project in the order it was
-- stored: the rowid's order, which is the order the counter below keeps.
INSERT INTO raw_batch_numbers (batch_id, project_id, number, received_at)
SELECT id, project_id, ROW_NUMBER() OVER (PARTITION BY project_id ORDER BY id), received_at
  FROM raw_batches;

-- One project's batch by its number, and its listing in arrival order with the
-- number as the tiebreak.
CREATE UNIQUE INDEX idx_raw_batch_numbers_number ON raw_batch_numbers(project_id, number);
CREATE INDEX idx_raw_batch_numbers_received ON raw_batch_numbers(project_id, received_at, number);

-- The highest number a project has seen go. The next number is one past the
-- higher of this and the highest number held, so a batch the sweep or an
-- erasure took leaves its number spent and a number never names two batches.
-- The mark moves when a batch is deleted — by any path, the cascade included —
-- and never on ingest, which writes no projects row for it.
ALTER TABLE projects ADD COLUMN raw_batches_numbered INTEGER NOT NULL DEFAULT 0;
UPDATE projects
   SET raw_batches_numbered = (SELECT COALESCE(MAX(number), 0) FROM raw_batch_numbers
                                WHERE raw_batch_numbers.project_id = projects.id);
CREATE TRIGGER raw_batch_numbers_spent AFTER DELETE ON raw_batch_numbers
  WHEN OLD.number > (SELECT raw_batches_numbered FROM projects WHERE id = OLD.project_id)
BEGIN
  UPDATE projects SET raw_batches_numbered = OLD.number WHERE id = OLD.project_id;
END;
