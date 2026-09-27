-- The Langfuse upload channel's lifecycle (spec 041 #29, #31).

-- The project's upload generation (#29): every erasure and trace deletion adds
-- one inside its transaction, a grant carries the generation it was issued
-- in, and the upload PUT refuses a grant from an earlier one. A counter, not
-- an instant, so that a clock stepped back cannot void the grants issued
-- after a deletion.
ALTER TABLE projects ADD COLUMN media_generation INTEGER NOT NULL DEFAULT 0;

-- The pending refs of one project (#31): the cap counts them without reading
-- the project's settled refs.
CREATE INDEX idx_media_refs_project_pending ON media_refs(project_id) WHERE pending = 1;
