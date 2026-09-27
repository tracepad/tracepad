-- A trace's tree reads the prefix of its observations in (start_time, id)
-- order, at most 10,000 of them (spec 043 #18). Served by an index on
-- (project_id, trace_id) alone, that LIMIT bounded the rows returned and not
-- the work: SQLite read and sorted every observation of the trace first, so a
-- trace of millions could not be opened within the read deadline. With the
-- order in the index the prefix is a bounded seek. The old index is a prefix
-- of the new one and goes; every query it served is served by this one, under
-- the same name (spec 043 #28).
DROP INDEX idx_observations_trace;
CREATE INDEX idx_observations_trace ON observations(project_id, trace_id, start_time, id);
