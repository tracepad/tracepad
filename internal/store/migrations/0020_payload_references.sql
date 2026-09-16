-- Indexes on the columns that reference payloads(id) (spec 035 #13).
--
-- Every delete of a payload row is a foreign-key check under
-- `foreign_keys=ON`: SQLite looks for a row that still points at it, in
-- every referencing column. None of the four was indexed, so each check was a
-- scan of `observations` (three times over) and of `traces` — measured on a
-- copy of a real database at ~6 ms per payload, which is ~36 ms per trace
-- deleted and the reason a deletion of a few hundred traces ran past the
-- interface's thirty-second clock. Erasure and the retention sweep delete
-- payloads by the same statement and paid the same price. With the indexes
-- the check is four seeks: 100 payloads went from 650 ms to 22 ms.
--
-- Partial, because a NULL reference is not a reference: the check looks for
-- a row whose column *equals* the deleted id, which `IS NOT NULL` implies, so
-- the planner takes the partial index (asserted by the plan test) — and an
-- observation with no metadata, or a trace with none, which is most of them,
-- writes nothing into it at ingest.
CREATE INDEX idx_observations_input_payload ON observations(input_id) WHERE input_id IS NOT NULL;
CREATE INDEX idx_observations_output_payload ON observations(output_id) WHERE output_id IS NOT NULL;
CREATE INDEX idx_observations_metadata_payload ON observations(metadata_id) WHERE metadata_id IS NOT NULL;
CREATE INDEX idx_traces_metadata_payload ON traces(metadata_id) WHERE metadata_id IS NOT NULL;
