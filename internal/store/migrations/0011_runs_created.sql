-- A project's runs, newest first, whatever the dataset (spec 016 #2).
--
-- The runs screen asks "what ran lately" across every dataset of a project,
-- and spec 014 only indexed the per-dataset order. This is that index's
-- sibling with the dataset left out: `GET /api/v1/runs` walks it by
-- `(created_at, id)`, the same keyset the per-dataset listing pages by, so
-- one cursor shape serves both.
CREATE INDEX idx_dataset_runs_created ON dataset_runs(project_id, created_at DESC, id DESC);
