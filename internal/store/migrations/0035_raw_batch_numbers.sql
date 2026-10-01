-- A raw batch's number is its project's own (spec 019 #17). The id the API
-- showed was the table's rowid, one sequence for every tenant: the gaps in a
-- project's own listing were how many batches the others had sent, and when.
-- The rowid stays the table's key and what the media refs point at; it no
-- longer leaves the server.

-- The number, 1, 2, 3… within each project, in the order the batches were
-- stored: the rowid's order, which is the order the counter below keeps.
ALTER TABLE raw_batches ADD COLUMN number INTEGER;
UPDATE raw_batches
   SET number = numbered.number
  FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY project_id ORDER BY id) AS number
          FROM raw_batches) AS numbered
 WHERE raw_batches.id = numbered.id;

-- The last number each project has issued. A counter rather than the highest
-- number held: a batch the sweep or an erasure took leaves its number spent,
-- so a number never names two batches.
ALTER TABLE projects ADD COLUMN raw_batches_numbered INTEGER NOT NULL DEFAULT 0;
UPDATE projects
   SET raw_batches_numbered = (SELECT COALESCE(MAX(number), 0) FROM raw_batches
                                WHERE raw_batches.project_id = projects.id);

-- One project's batch by its number, and its listing in arrival order with the
-- number as the tiebreak. The old index is a prefix of the new one and goes;
-- every query it served is served by this one, under the same name.
CREATE UNIQUE INDEX idx_raw_batches_number ON raw_batches(project_id, number);
DROP INDEX idx_raw_batches_received;
CREATE INDEX idx_raw_batches_received ON raw_batches(project_id, received_at, number);

-- The column is nullable only because SQLite adds a column that way; a batch
-- stored without a number would be one the API cannot name.
CREATE TRIGGER raw_batches_need_a_number BEFORE INSERT ON raw_batches
  WHEN NEW.number IS NULL
BEGIN
  SELECT RAISE(ABORT, 'raw_batches.number is required');
END;
