-- An erasure takes a user's traces in the order they started, whole hours at a
-- time (spec 047 #1). Served by an index on (project_id, user_id) alone, a
-- chunk read them in arrival order and kept one hour's worth of the first
-- 500: for a client that exports out of start order that was about two
-- traces, one commit and one whole-hour roll per chunk. With the start in the
-- index the hours come contiguous. The old index is a prefix of the new one
-- and goes; every query it served is served by this one, under the same name.
DROP INDEX idx_traces_user;
CREATE INDEX idx_traces_user ON traces(project_id, user_id, timestamp);
