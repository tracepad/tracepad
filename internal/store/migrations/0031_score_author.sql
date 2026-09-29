-- The author of a score (spec 048): the credential that wrote the row, stamped
-- by the server from the caller and never from the body (#1). An account's id
-- or a key's public key, plain text rather than a foreign key, beside a copy
-- of how it was named when it wrote (#2): an account and a key are both
-- deleted outright, and deleting either takes nothing else with it (spec 028
-- #12, spec 045 #8). The last writer is the author, as it is the value's (#3).
--
-- A row written before this migration has no author and never will (#6): all
-- four are NULL together, and a row written after has all four.
ALTER TABLE scores ADD COLUMN author_kind TEXT
    CHECK (author_kind IN ('account', 'key'));
ALTER TABLE scores ADD COLUMN author_id TEXT
    CHECK ((author_id IS NULL) = (author_kind IS NULL));
ALTER TABLE scores ADD COLUMN author_name TEXT
    CHECK ((author_name IS NULL) = (author_kind IS NULL));
-- '' for a key: a program has no address (#2).
ALTER TABLE scores ADD COLUMN author_email TEXT
    CHECK ((author_email IS NULL) = (author_kind IS NULL));

-- "What have I scored" (#9), paged in the listing's own order. Partial, so
-- that the rows from before cost nothing.
CREATE INDEX idx_scores_author ON scores(project_id, author_id, timestamp DESC, id DESC)
    WHERE author_id IS NOT NULL;
