# Spec 011 — Search: find the trace by what was said in it

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Every filter so far asks about a trace's labels — who, when, where, how
> much. The question people actually arrive with is a piece of text: the
> error message a user pasted, a sentence the model should never have said,
> the order id from a support ticket. This spec adds full-text search over
> what the observations carried — prompts, completions, metadata, names,
> status messages — as one more filter on the trace listing, in all three
> clients, with the row saying *where* it matched.

---

## Overview

Deliverables:

- `q=` on `GET /api/v1/traces` (and `/traces/last`): word-based full-text
  search over observation input, output, metadata, name and status message,
  and over the trace name. A trace matches when one field of one of its
  observations matches.
- Each matching row carries `match`: the observation and field the search
  hit, and a short snippet of the text around the hit.
- A SQLite FTS5 index, contentless, written in the ingest transaction and
  maintained by every path that deletes observations; existing databases are
  indexed once, on the first start after the upgrade.
- The same capability in the CLI (`--search`) and as the MCP tool `search`
  that spec 004 #17 promised would arrive with this.
- A search box on the Traces screen, with the snippet under the row and a
  click that opens the panel on the observation that matched.

Not here: relevance ranking, substring search, search over sessions or
scores, a query language beyond words, phrases and a trailing `*`.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-30** — The engine is **SQLite FTS5**, tokenizer `unicode61` with `remove_diacritics 2`, positions kept (`detail=full`). Search is **word-based**: `error` does not match `errors`, `rror` matches nothing | FTS5 ships inside `modernc.org/sqlite` v1.57.0 (SQLite 3.53.3) — verified on the driver itself: `unicode61` and `trigram` both load, `contentless_delete` is available, and a phrase query behaves exactly as the reference platform's own text index does (`"refund failed"` matches, `"failed refund"` does not, `gpt-4o` is found whole and by part). No second engine, no CGO, no sidecar. Word-based rather than trigram because the index is what this feature costs: a trigram index is three to five times the size of the text and cannot answer a phrase; a positional word index is a fraction of it and can. The reference platform made the same call for its API and forbids substring on payload text for the same reason. |
| 2 | **2026-08-30** — What is indexed, per observation: `input`, `output`, `metadata` (the stored JSON text of each), `name`, `status_message`; and per trace: `name`. Each payload contributes its **first 64 KiB** (`SearchIndexCap`); the rest is stored and readable but not searched | The five observation fields are where text lives; the trace name is the one trace-level string somebody types. The cap is a size decision, not a quality one: a 500 KB document on one observation's input would otherwise cost the index as much as a hundred ordinary traces, and design §5.6's thresholds are written for the ordinary ones. Error messages, refusals and answers live in the first kilobytes; what lives past 64 KiB is an embedded document, and `/observations/{id}/io` still reads all of it. The cap is stated in the docs beside the thresholds, so nobody discovers it by a search that came back empty. |
| 3 | **2026-08-30** — The index is a **contentless** FTS5 table (`content=''`, `contentless_delete=1`) with one row per `(observation, field)`, owned through a side table `search_entries(id INTEGER PRIMARY KEY, project_id, trace_id, observation_id, field)` whose `id` is the FTS rowid. Payloads stay where they are, zstd-compressed | A content-bearing FTS table would store every indexed payload a second time, uncompressed — the one thing the payload table's design exists to avoid (design §5.2). Contentless keeps only the index, and `contentless_delete` makes a delete a rowid delete rather than a re-supply of the original text, which for a compressed payload would mean decompressing it to forget it. The side table is what scopes the index to a project and maps a hit back to its trace: a contentless table can return nothing but its rowid. It is keyed by its own `INTEGER PRIMARY KEY` and not by `observations.rowid`, because `observations` has no explicit integer key and **VACUUM may renumber such rowids** — and this store vacuums (spec 005 #5, migration backups). |
| 4 | **2026-08-30** — The query string is **built by the server** from the user's text and never passed to FTS5 as written. Bare words are quoted tokens joined by AND; text inside double quotes is a phrase; a bare word ending in `*` is a prefix; everything else — `AND`, `OR`, `NOT`, parentheses, `:`, `^`, `-` — is literal text or is dropped by the tokenizer. A query with no word left is a 400 | FTS5's own syntax is an internal interface: a stray parenthesis is a syntax error, a column name followed by a colon is a column filter, and `NOT` in a user's sentence would invert it. Every user-facing search box that passes syntax through ends up documenting an error message instead of a feature. Words-AND-phrases-and-prefix is what people already know from every search box, and it is expressible as a FTS5 query that cannot fail to parse. |
| 5 | **2026-08-30** — `q` is a **filter on the trace listing**, not a new endpoint or a new ordering. Rows stay newest first, paged by the same keyset, counted by the same capped count. A trace matches when **all the words of the query occur in one field of one observation** (or in the trace name) | Search narrows the listing; it does not replace it. Relevance ordering would break the cursor contract every client is built on (spec 004 #4, spec 009 #2): a cursor is a position in one ordering, and a ranking is a different one per query. Newest-first is also the right order for the question being asked — "when did this last happen?". The match unit is one field of one observation because that is what a FTS row is (Decision 3); two words that occur in different observations of one trace are not a hit, and the docs say so. |
| 6 | **2026-08-30** — Each row carries **`match`**: `{observation_id, field, snippet}` for the observation and field with the best `bm25` among the trace's hits; `observation_id` is `null` and `field` is `trace_name` when the trace name matched. The snippet is a window of at most `SearchSnippetLength` (160) characters around the first hit of the first query term, cut on word boundaries, computed by the server from the stored payload; hits are not marked up | What people want from a search is not "which trace" but "where in it". `bm25` is available on a contentless index and picks the observation; the snippet cannot come from FTS5's `snippet()` — a contentless table has no text to cut it from — so the server decompresses the one field it needs and cuts the window itself. One payload per row, capped, on a page of at most 500 rows: the cost is bounded and paid only when `q` is set. Hits are not marked in the snippet because the API answers with data, not markup; clients that highlight fold the query terms the same way the tokenizer does. `match` is a row field, selectable through `?fields=`, absent when there is no `q`. |
| 7 | **2026-08-30** — The index is written **in the ingest transaction**, entries first deleted then re-inserted on an observation upsert; and it is **deleted in the same transaction** by every path that deletes observations — the retention sweep, user-data erasure and project purge (spec 005). A test asserts no orphan entry after each | An index that lags the data answers with traces that are not there, and an index that outlives a deletion is a retention promise broken: text erased under spec 005 must not remain findable. The write cost is bounded (at most six FTS inserts per observation, in the batch that already writes it), and it is measured before this ships (Testing). |
| 8 | **2026-08-30** — Existing databases are indexed **synchronously on the first start** after the migration that creates the tables, before the server listens; the backfill is idempotent and resumable, and reports progress in the log | Every migration so far leaves the database consistent before a single request is served, and this one keeps that: a search that answers "nothing" because the index is half-built is worse than a start that takes a minute. The backfill is Go code, not SQL — it decompresses payloads — so it runs after `migrate()` the way `ensureIncrementalVacuum` does, guarded by a marker it writes only when done, skipping observations already indexed; a crash halfway resumes. The rate is measured on a fixture and written into the retention docs beside the thresholds, so an operator with a large store knows what the first start costs. |
| 9 | **2026-08-30** — Three clients in one PR (REPOS §2): the API filter; `traces ls --search` and `traces last --search` in the CLI, the snippet printed under the row; an MCP tool **`search`** that is the trace listing with `q` required and `match` in its rows. `list_traces` does not grow `q` | Spec 004 #17 declined a `search` tool until there was a search endpoint, so that the tool would never claim a capability the server lacked; this is the endpoint. A tool of its own rather than one more parameter on `list_traces` because the descriptions are triggers (spec 004 #17): "the user quotes text they saw" is a different question from "the user asks what ran", and a model choosing tools by description is better served by two. Both still map onto one endpoint. |
| 10 | **2026-08-30** — No `q` on the sessions listing, no search over scores, prompts or raw batches | A session is a grouping of traces (spec 007 #2); "sessions whose traces mention X" is a reasonable question and a later spec's, once the trace-level search has been used. Scores and prompts are small and structured; their filters are exact. Raw batches are not user text. |
| 11 | **2026-08-30** (from the implementation) — Schema 0006 carries two things the data contract below did not name: `idx_search_entries_trace` is `(project_id, trace_id, observation_id)` rather than the first two columns alone, and a one-row `search_backfill(id, done_at)` table holds the marker Decision 8 asks for | The third column is what the ingest path addresses an entry by: every delivery replaces one span's entries (Decision 7), and on the first two columns alone that delete is a scan of the whole trace's entries — quadratic inside a batch of twenty spans of one trace, which is the ordinary shape of an export. The two columns the deletion paths and the listing use are still the index's prefix, so nothing else changes. The marker had to live somewhere, and a table of its own says what it is: a row in `schema_migrations` would have made the backfill look like a migration, which is precisely what it is not — it runs *after* `migrate()`, because it decompresses payloads. |
| 12 | **2026-08-30** (from the implementation) — `GET /api/v1/traces/last` takes `q` as a filter, and its answer carries no `match` | The shortcut returns a trace rather than a listing row, and with `?expand=io` it returns the payloads themselves — a snippet cut from text the response already carries in full would be a second, worse copy of it. The edge case that gives this endpoint a `q` asks for "the newest matching trace", and that is what it answers; `docs/api.md` says so rather than leaving a reader to notice the field is missing. |
| 13 | **2026-08-30** (from the implementation) — `search_entries` joins the tables `GET /api/v1/system` reports row counts for | The index is a store of its own beside the payloads, and an operator deciding whether their disk can carry it has nothing else to read it from: `size_bytes` is the whole file and every other count is of rows they already knew about. It qualifies where `payloads` does not for exactly the reason spec 004 Decision 33 gives — it carries a project id, so it is counted within the asking project rather than telling one tenant how much the others hold. |

## API contract

`GET /api/v1/traces` and `GET /api/v1/traces/last` accept:

| Parameter | Meaning |
|---|---|
| `q` | Search text: words (all must occur), `"quoted phrases"`, `prefix*`. Matched against one field of one observation — input, output, metadata, name, status message — or the trace name. At most 512 characters; a `q` with no word in it is a `400`. |

With `q`, every row of the response carries:

```json
{
  "id": "…",
  "…": "…",
  "match": {
    "observation_id": "9f92a1c4b0e7d3f5",
    "field": "output",
    "snippet": "…the refund failed for the order because the card issuer declined the…"
  }
}
```

`field` is one of `input`, `output`, `metadata`, `name`, `status_message`,
`trace_name`; with `trace_name`, `observation_id` is `null`. `match` is a
row field for `?fields=` and is never present without `q`. Ordering, paging
and `count` are exactly those of the listing (Decision 5): `q` is one more
condition, and a cursor taken with a `q` is valid only with the same `q`,
which is already how every other filter behaves.

What is and is not matched, written once and repeated in `docs/api.md`:

- Words, not substrings: `error` does not find `errors`; `err*` finds both.
- Case and diacritics are folded: `Refund`, `refund` and `réfund` are one word.
- `refund order` finds a field containing both words in any order;
  `"refund order"` finds them adjacent, in that order.
- Identifiers split on punctuation and are found whole or by part:
  `user_id_42` is found by `user_id_42`, `user` and `42`.
- Only the first 64 KiB of each payload is indexed (Decision 2).
- All words must occur in the **same** field of the **same** observation.

## CLI contract

`traces ls --search "…"` and `traces last --search "…"` map to `q`. With
`--search`, the table gains a second line under each row: the field and the
snippet, indented, dimmed. `tail` does not take `--search` (it follows the
newest page and is not a question about text). The usage lines grow with it,
and so does the parity spec 004 #9 gave the HTTP surface, pointed at the
command line for the first time: a test walks the usage text and refuses a
flag it offers that the command does not define.

## MCP contract

Tool `search` — read-only, input: `q` (required) plus the filters, paging and
field selection of `list_traces`; output: the listing's shape with `match` on
each row. Description written as a trigger: *the user quotes or paraphrases
text they saw — an error message, a phrase in an answer, an id from a
ticket — and wants the traces where it appears; returns where each match
was, so the next call can be `get_observation_io` on that observation.*
`list_traces` is unchanged; `docs/mcp.md` drops the paragraph that says there
is no search tool.

## Data contract (schema 0006)

```sql
CREATE TABLE search_entries (
    id             INTEGER PRIMARY KEY,
    project_id     TEXT NOT NULL,
    trace_id       TEXT NOT NULL,
    observation_id TEXT,               -- NULL for the trace name
    field          TEXT NOT NULL
) STRICT;
CREATE INDEX idx_search_entries_trace
    ON search_entries(project_id, trace_id, observation_id);   -- Decision 11

CREATE VIRTUAL TABLE search_fts USING fts5(
    body,
    content='', contentless_delete=1,
    tokenize='unicode61 remove_diacritics 2'
);

-- Whether the one-off backfill has finished (Decision 8, Decision 11).
CREATE TABLE search_backfill (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    done_at INTEGER
) STRICT;
```

`search_fts.rowid = search_entries.id`. No foreign key: FTS5 virtual tables
cannot carry one, and the deletion paths of Decision 7 are what keep the two
in step; the sweeper's orphan pass (spec 005) learns to drop entries whose
observation is gone, as a belt to those braces.

The listing with `q` is, in shape:

```sql
SELECT … FROM traces
 WHERE project_id = ? AND … AND id IN (
   SELECT trace_id FROM search_entries
    WHERE project_id = ? AND id IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?))
 ORDER BY timestamp DESC, id DESC LIMIT ?
```

`EXPLAIN QUERY PLAN` must show the outer scan still seeking
`idx_traces_timestamp` with the keyset (spec 003 #25's method); the
subquery materializes once per statement. The index is one table across all
projects; a hit in another project is discarded by the join and never
returned, and the side table's `project_id` is the only scope there is.

## Application contract

On the Traces screen, a search box beside the range picker — first-class,
not inside the filter popover: `q` in the URL like every filter, committed on
Enter and on blur, never per keystroke (the convention of spec 007). Its
placeholder says what it searches: *Search prompts, answers, errors…*. With a
`q`, each row shows the snippet under the name in muted monospace, the query
terms marked with `<mark>` by folding both the same way the tokenizer does;
clicking the row opens the peek panel on the matched observation
(`?peek={trace}&obs={observation_id}`), and on the trace itself when the
trace name matched. The empty state names the query: *Nothing matches
"…"* with a clear control. The count in the header is the count with `q`
(spec 009 #4). No search on Sessions (Decision 10).

The line budget is measured at the end (spec 010 #7); the search box, the
snippet and the highlight are the whole of the UI growth.

## Testing

- **Go, query builder** (table-driven): each operator and punctuation form
  of FTS5 syntax in the input comes out neutralized; phrase, prefix and a
  phrase with an inner quote round-trip; a query of only punctuation is an
  error; 512 characters is the limit.
- **Go, index contract**: the list under *API contract* asserted literally
  on a fixture — `error`/`errors`, `err*`, case and diacritics, phrase order
  and adjacency, `user_id_42` whole and by part, the 64 KiB cap (a word at
  63 KiB is found, the same word at 65 KiB is not), and the same-field rule
  (two words split across two observations of one trace do not match).
- **Go, lifecycle**: a re-delivered observation leaves exactly one set of
  entries; after the retention sweep, user-data erasure and project purge
  respectively, `search_entries` holds nothing for the deleted rows and
  `INSERT INTO search_fts(search_fts) VALUES('integrity-check')` passes.
- **Go, backfill**: a database written before the migration is fully
  searchable after `Open`; a second `Open` does no indexing work (asserted by
  a counter); an `Open` interrupted after part of the work resumes and
  finishes.
- **Go, plan and cost**: `EXPLAIN QUERY PLAN` on the shipped listing with
  `q` shows the keyset seek. Ingest throughput with the index on, measured
  on the store's benchmark fixture against the same run without it, both
  numbers in the PR; the backfill rate on the same fixture, in the docs.
- **Go, match**: the snippet is cut on word boundaries around the first term
  and never exceeds the length; `trace_name` matches carry a `null`
  observation id; `match` is absent without `q` and selectable with
  `?fields=`.
- **CLI / MCP**: flag-and-usage parity; the `search` tool's schema names `q`
  as required and its output carries `match`.
- **UI**: the filter round-trip with `q`; the highlight function folds case
  and diacritics like the tokenizer; E2E — search a phrase seeded into a
  fixture payload, see the snippet, click the row, land on that observation
  in the panel; the empty state names the query.
- **Chrome (DoD)**: both themes, 375 px, console clean, one listing request
  per search; the network panel shows no request on opening the panel
  (spec 010 invariant 1 still holds with `q` in the URL).

## Edge cases

- `q` and `name` together: both apply; `name` stays exact.
- A trace whose only hit is on an observation the response budget truncates:
  the snippet is cut from the stored payload, not from the response, so it
  is unaffected.
- A payload that is not valid UTF-8 after decompression: indexed as SQLite
  reads it; the snippet is cut on rune boundaries.
- The trace name matches *and* an observation matches: `bm25` decides, with
  the trace name treated as one more entry.
- `traces/last` with `q`: the newest matching trace, 404 when none.

## Out of scope

- Relevance ranking; sorting by anything but time (spec 009).
- Substring or fuzzy search; a query language beyond Decision 4.
- Search over sessions, scores, prompts, raw batches (Decision 10).
- Highlight offsets in the API; the snippet is plain text.
- Re-indexing on demand (a `reindex` command) — the backfill is the only
  rebuild, and the integrity check is the only diagnosis, until one is needed.
