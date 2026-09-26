-- The Langfuse upload channel's lifecycle (spec 041 #29, #31).

-- Upload grants issued at or before this instant are void (#29): set by every
-- erasure and trace deletion inside its transaction, compared by the upload
-- PUT. Unix nanoseconds; 0 voids nothing.
ALTER TABLE projects ADD COLUMN media_grants_after INTEGER NOT NULL DEFAULT 0;

-- The pending refs of one project (#31): the cap counts them without reading
-- the project's settled refs.
CREATE INDEX idx_media_refs_project_pending ON media_refs(project_id) WHERE pending = 1;
