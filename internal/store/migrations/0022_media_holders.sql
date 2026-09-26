-- Media per project (spec 041 #25, #26). The bytes stay one row per content
-- (#2); what a project sees of a body — the type it declared and when its
-- hold began — is its own row here, and whether it holds the body at all is a
-- seek on (sha256, project_id), never a walk of its raw batches.

-- A raw ref names its project (#26), so a project's hold is a seek on
-- (sha256, project_id) in both ref tables. Filled from the batch, which is
-- exact: a batch belongs to one project.
ALTER TABLE media_raw_refs ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
UPDATE media_raw_refs
   SET project_id = (SELECT project_id FROM raw_batches WHERE id = raw_batch_id);
CREATE INDEX idx_media_raw_refs_holder ON media_raw_refs(sha256, project_id);

-- A project's own view of a body it holds (#25): the type this project stored
-- it under and when its hold began (Unix nanoseconds). One row per content
-- and holder, written with the project's first ref to the body and deleted
-- with its last, and gone with the body by cascade.
CREATE TABLE media_holders (
    sha256     TEXT NOT NULL REFERENCES media(sha256) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    mime_type  TEXT NOT NULL,
    first_at   INTEGER NOT NULL,
    PRIMARY KEY (sha256, project_id)
) STRICT, WITHOUT ROWID;

-- Which type each project declared before this migration is kept only inside
-- the payloads' references, so every existing holder gets the body's stored
-- type: its own, for a body one project holds. `first_at` is the earliest of
-- the project's refs and raw batches naming the body. No body is rewritten.
INSERT INTO media_holders (sha256, project_id, mime_type, first_at)
SELECT h.sha256, h.project_id, m.mime_type, MIN(h.at)
  FROM (SELECT sha256, project_id, created_at AS at FROM media_refs
        UNION ALL
        SELECT rr.sha256, rr.project_id, b.received_at
          FROM media_raw_refs rr JOIN raw_batches b ON b.id = rr.raw_batch_id) h
  JOIN media m ON m.sha256 = h.sha256
 GROUP BY h.sha256, h.project_id;
