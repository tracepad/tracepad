package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/model"
)

// Search (spec 011, Testing): what the query builder produces, what the index
// matches, that a deletion takes the index with it, and that the listing with
// `q` still seeks its keyset.

// TestSearchQueryBuilder is Testing #1: every operator and punctuation form of
// FTS5 syntax comes out neutralized, and what comes out is a query FTS5 cannot
// fail to parse — which is asserted against a real FTS5 table rather than
// argued about, because "cannot fail to parse" is a claim about SQLite.
func TestSearchQueryBuilder(t *testing.T) {
	s, _ := readStore(t)

	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"a bare word", "refund", `"refund"`},
		{"two words are ANDed", "refund order", `"refund" AND "order"`},
		{"a quoted phrase stays one", `"refund order"`, `"refund order"`},
		{"a trailing star is a prefix", "err*", `"err"*`},
		{"a star mid-word is punctuation", "er*r", `"er r"`},
		{"an identifier splits on punctuation", "user_id_42", `"user id 42"`},
		{"a colon is not a column filter", "output:refund", `"output refund"`},
		{"AND is a word", "cats AND dogs", `"cats" AND "AND" AND "dogs"`},
		{"NOT is a word", "not found", `"not" AND "found"`},
		{"OR is a word", "a OR b", `"a" AND "OR" AND "b"`},
		{"parentheses are dropped", "(refund)", `"refund"`},
		{"a leading dash is dropped", "-refund", `"refund"`},
		{"a caret is dropped", "^refund", `"refund"`},
		{"a quote inside a sentence cannot escape", `say "hello" now`, `"say" AND "hello" AND "now"`},
		{"an unclosed quote runs to the end", `"refund failed`, `"refund failed"`},
		{"a phrase keeps its inner punctuation as words", `"gpt-4o mini"`, `"gpt 4o mini"`},
		{"case and diacritics are the tokenizer's business", "Réfund", `"Réfund"`},
		{"a star on a phrase is not a prefix", `"refund"*`, `"refund"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, err := ParseSearch(tc.input)
			if err != nil {
				t.Fatalf("ParseSearch(%q): %v", tc.input, err)
			}
			if query.Match != tc.want {
				t.Errorf("ParseSearch(%q) = %s, want %s", tc.input, query.Match, tc.want)
			}
			// The whole point of building it ourselves: SQLite accepts it.
			var n int
			if err := s.db.QueryRow(
				`SELECT count(*) FROM search_fts WHERE search_fts MATCH ?`, query.Match).Scan(&n); err != nil {
				t.Fatalf("FTS5 refused %s (from %q): %v", query.Match, tc.input, err)
			}
		})
	}
}

func TestSearchQueryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"nothing at all", ""},
		{"only spaces", "   "},
		{"only punctuation", "()-:^*"},
		{"only an empty phrase", `""`},
		{"past the length limit", strings.Repeat("a", MaxSearchQueryLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSearch(tc.input); err == nil {
				t.Fatalf("ParseSearch(%q) was accepted; a query with no word is a 400", tc.input)
			}
		})
	}
	// Exactly the limit is inside it.
	if _, err := ParseSearch(strings.Repeat("a", MaxSearchQueryLength)); err != nil {
		t.Fatalf("a query of exactly %d characters was refused: %v", MaxSearchQueryLength, err)
	}
}

// searchCorpus seeds the fixture the index contract is asserted on.
func searchCorpus(t *testing.T, s *Store, projectID string) {
	t.Helper()
	seed := func(n int, name string, o *model.Observation) {
		o.TraceID = hexTrace(n)
		o.ID = hexSpan(n)
		o.Type = model.TypeSpan
		if o.Level == "" {
			o.Level = model.LevelDefault
		}
		o.StartTime = int64(n) * day
		o.EndTime = o.StartTime + 1
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(n), Name: name}, o)
	}
	seed(1, "support-chat", &model.Observation{
		Output: "the refund failed for the order because the card issuer declined the charge",
	})
	seed(2, "", &model.Observation{Output: "two errors were logged"})
	seed(3, "", &model.Observation{Input: "Réfund requested by user_id_42 using gpt-4o"})
	// One trace whose two observations hold one word each: the same-field
	// rule is what makes this not a match for both words together.
	{
		first := &model.Observation{TraceID: hexTrace(4), ID: hexSpan(41), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: 4 * day, EndTime: 4*day + 1, Input: "alpha"}
		second := &model.Observation{TraceID: hexTrace(4), ID: hexSpan(42), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: 4 * day, EndTime: 4*day + 1, Input: "beta"}
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(4)}, first, second)
	}
	// The 64 KiB cap: the same word inside the window and past it.
	inside := strings.Repeat("filler ", 9000) + " capneedle"
	outside := strings.Repeat("filler ", 9700) + " capneedle"
	seed(5, "", &model.Observation{Input: inside})
	seed(6, "", &model.Observation{Input: outside})
	// A status message and a name, which are indexed like a payload.
	seed(7, "", &model.Observation{Name: "charge-card",
		Level: model.LevelError, StatusMessage: "upstream timed out with an error"})
}

// matchingTraces runs the shipped listing with `q` and returns the ids it kept.
func matchingTraces(t *testing.T, s *Store, projectID, q string) []string {
	t.Helper()
	query, err := ParseSearch(q)
	if err != nil {
		t.Fatalf("ParseSearch(%q): %v", q, err)
	}
	rows, err := s.Traces(projectID, TraceFilter{Limit: 100, Search: query})
	if err != nil {
		t.Fatalf("Traces(q=%q): %v", q, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// TestSearchIndexContract is Testing #2: the list under *API contract*,
// asserted literally.
func TestSearchIndexContract(t *testing.T) {
	s, project := readStore(t)
	searchCorpus(t, s, project.ID)

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"words, not substrings", "error", []string{hexTrace(7)}},
		{"the plural is a different word", "errors", []string{hexTrace(2)}},
		{"a prefix finds both", "err*", []string{hexTrace(7), hexTrace(2)}},
		{"case is folded", "REFUND", []string{hexTrace(3), hexTrace(1)}},
		{"diacritics are folded", "réfund", []string{hexTrace(3), hexTrace(1)}},
		{"two words in any order", "refund order", []string{hexTrace(1)}},
		{"a phrase needs adjacency", `"refund failed"`, []string{hexTrace(1)}},
		{"a phrase in the wrong order matches nothing", `"failed refund"`, nil},
		{"an identifier whole", "user_id_42", []string{hexTrace(3)}},
		{"an identifier by part", "user", []string{hexTrace(3)}},
		{"an identifier's number", "42", []string{hexTrace(3)}},
		{"a model name whole", "gpt-4o", []string{hexTrace(3)}},
		{"a model name by part", "gpt", []string{hexTrace(3)}},
		{"the trace name", "support-chat", []string{hexTrace(1)}},
		{"an observation name", "charge-card", []string{hexTrace(7)}},
		{"a status message", "upstream timed out", []string{hexTrace(7)}},
		{"inside the index cap", "capneedle", []string{hexTrace(5)}},
		{"both words in one field", "alpha", []string{hexTrace(4)}},
		{"the words split across two observations", "alpha beta", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := matchingTraces(t, s, project.ID, tc.query)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("q=%q matched %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSearchIsScopedToItsProject: the index is one table across every project,
// and the side table's project id is the only scope there is.
func TestSearchIsScopedToItsProject(t *testing.T) {
	s, project := readStore(t)
	other, err := s.CreateProject("other", KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	seedTrace(t, s, other.ID, &model.Trace{ID: hexTrace(9)},
		&model.Observation{TraceID: hexTrace(9), ID: hexSpan(9), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: day, EndTime: day + 1,
			Output: "a neighbour's secret"})

	if got := matchingTraces(t, s, project.ID, "secret"); len(got) != 0 {
		t.Fatalf("another project's text is findable: %v", got)
	}
	if got := matchingTraces(t, s, other.ID, "secret"); len(got) != 1 {
		t.Fatalf("the owning project cannot find its own text: %v", got)
	}
}

// countEntries is how many index entries a project holds right now.
func countEntries(t *testing.T, s *Store, projectID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM search_entries WHERE project_id = ?`,
		projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// checkIntegrity runs FTS5's own audit of the index (spec 011, Testing #3).
func checkIntegrity(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO search_fts(search_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("the search index is corrupt: %v", err)
	}
}

// arriveWithText is `arrive` with text worth searching for: the sweep fixture's
// own payloads are filler by design, and these tests are about the words.
func (f *sweepFixture) arriveWithText(t *testing.T, traceID string, at int64, name, output string) {
	t.Helper()
	batch := &IngestBatch{
		ProjectID:  f.project.ID,
		IngestedAt: at,
		Traces:     []*model.Trace{{ID: traceID, Name: name, UserID: "u1"}},
		Observations: []*model.Observation{{
			TraceID: traceID, ID: traceID[:16], Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: at, EndTime: at + 1_000_000, Output: output,
		}},
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

// TestSearchIndexIsRewrittenOnRedelivery is the first half of Testing #3: a
// re-delivered observation leaves exactly one set of entries, not two.
func TestSearchIndexIsRewrittenOnRedelivery(t *testing.T) {
	s, project := readStore(t)
	deliver := func(output string) {
		seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(1), Name: "chat"},
			&model.Observation{TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: day, EndTime: day + 1, Output: output})
	}
	deliver("the first answer")
	first := countEntries(t, s, project.ID)
	deliver("the second answer")
	if again := countEntries(t, s, project.ID); again != first {
		t.Errorf("entries = %d after a re-delivery, want the same %d", again, first)
	}
	if got := matchingTraces(t, s, project.ID, "first"); len(got) != 0 {
		t.Errorf("the previous delivery's text is still findable: %v", got)
	}
	if got := matchingTraces(t, s, project.ID, "second"); len(got) != 1 {
		t.Errorf("the latest delivery is not findable: %v", got)
	}
	checkIntegrity(t, s)
}

// TestSearchIndexFollowsTheSweep is Testing #3 for retention: after the sweep,
// nothing of the deleted traces is left in either half of the index.
func TestSearchIndexFollowsTheSweep(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(30), nil)
	f.arriveWithText(t, hexTrace(1), daysAgo(40), "", "the refund failed")
	f.arriveWithText(t, hexTrace(2), daysAgo(1), "", "the refund succeeded")
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := matchingTraces(t, f.store, f.project.ID, "refund")
	if len(got) != 1 || got[0] != hexTrace(2) {
		t.Errorf("after the sweep, q=refund matched %v, want only the trace that stayed", got)
	}
	assertNoEntries(t, f.store, f.project.ID, hexTrace(1))
	checkIntegrity(t, f.store)
}

// TestSearchIndexFollowsErasure is Testing #3 for spec 005 #7: text erased on a
// user's behalf must not remain findable.
func TestSearchIndexFollowsErasure(t *testing.T) {
	f := newSweepFixture(t)
	f.arriveWithText(t, hexTrace(1), daysAgo(1), "", "everything about this person")
	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "u1", Confirm: "u1", Limit: 100}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	if erase.Counts.Traces != 1 {
		t.Fatalf("erased %d traces, want the one", erase.Counts.Traces)
	}
	if got := matchingTraces(t, f.store, f.project.ID, "person"); len(got) != 0 {
		t.Errorf("erased text is still findable: %v", got)
	}
	assertNoEntries(t, f.store, f.project.ID, hexTrace(1))
	checkIntegrity(t, f.store)
}

// TestSearchIndexFollowsThePurge is Testing #3 for the project purge: the one
// path that deletes a project's rows through a cascade the index cannot follow.
func TestSearchIndexFollowsThePurge(t *testing.T) {
	f := newSweepFixture(t)
	f.arriveWithText(t, hexTrace(1), daysAgo(1), "", "the last thing this project knew")
	deleted := sweepNow.Add(-GraceWindow - time.Hour).UnixNano()
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		deleted, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if project, _ := f.store.ProjectByID(f.project.ID); project != nil {
		t.Fatalf("the project survived its grace window: %+v", project)
	}
	if left := countEntries(t, f.store, f.project.ID); left != 0 {
		t.Errorf("the purge left %d index entries behind", left)
	}
	checkIntegrity(t, f.store)
}

// TestSweeperCollectsOrphanedSearchEntries is the belt to those braces: an
// entry whose observation is gone is found by the orphan pass, whatever put it
// there.
func TestSweeperCollectsOrphanedSearchEntries(t *testing.T) {
	f := newSweepFixture(t)
	f.arriveWithText(t, hexTrace(1), daysAgo(1), "", "still here")
	// A row nothing points at, exactly as a hand-edited database would
	// leave it: the observation half deleted without the index half.
	if err := insertEntries(f.store.db, f.project.ID, hexTrace(1), hexSpan(99),
		[]searchEntry{{FieldOutput, "orphaned text"}}); err != nil {
		t.Fatal(err)
	}
	if got := matchingTraces(t, f.store, f.project.ID, "orphaned"); len(got) == 0 {
		t.Fatal("the fixture did not take: the orphan is not findable to begin with")
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := matchingTraces(t, f.store, f.project.ID, "orphaned"); len(got) != 0 {
		t.Errorf("the orphan pass left the entry behind: %v", got)
	}
	if got := matchingTraces(t, f.store, f.project.ID, "still"); len(got) != 1 {
		t.Errorf("the orphan pass took a live entry with it: %v", got)
	}
	checkIntegrity(t, f.store)
}

func assertNoEntries(t *testing.T, s *Store, projectID, traceID string) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM search_entries WHERE project_id = ? AND trace_id = ?`,
		projectID, traceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d index entries survive the deletion of trace %s", n, traceID)
	}
}

// TestSearchBackfill is Testing #4: a database written before the migration is
// fully searchable after Open, a second Open does no work, and an interrupted
// one resumes.
func TestSearchBackfill(t *testing.T) {
	s, project := readStore(t)
	searchCorpus(t, s, project.ID)

	// The state a database written before schema 0006 is in: rows, no index.
	if _, err := s.db.Exec(`DELETE FROM search_fts`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM search_entries`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE search_backfill SET done_at = NULL WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := matchingTraces(t, s, project.ID, "refund"); len(got) != 0 {
		t.Fatalf("the fixture did not take: %v is still indexed", got)
	}

	if err := s.backfillSearchIndex(); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := matchingTraces(t, s, project.ID, "refund"); len(got) != 2 {
		t.Errorf("after the backfill, q=refund matched %v, want both traces", got)
	}
	if got := matchingTraces(t, s, project.ID, "support-chat"); len(got) != 1 {
		t.Errorf("the backfill did not index the trace names: %v", got)
	}
	if got := matchingTraces(t, s, project.ID, "capneedle"); len(got) != 1 {
		t.Errorf("the backfill did not apply the index cap: %v", got)
	}
	indexed := countEntries(t, s, project.ID)

	// A second run does nothing at all, which the marker is what buys: the
	// entry count is unchanged and no trace is offered for indexing.
	if err := s.backfillSearchIndex(); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if again := countEntries(t, s, project.ID); again != indexed {
		t.Errorf("a second backfill wrote %d entries where the first left %d", again-indexed, indexed)
	}
	pending, err := s.unindexedTraces([2]string{"", ""}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("the backfill left %d traces unindexed", len(pending))
	}
	checkIntegrity(t, s)
}

func TestSearchBackfillResumes(t *testing.T) {
	s, project := readStore(t)
	searchCorpus(t, s, project.ID)
	if _, err := s.db.Exec(`DELETE FROM search_fts`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM search_entries`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE search_backfill SET done_at = NULL WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	// A crash after part of the work: one batch committed, no marker.
	batch, err := s.unindexedTraces([2]string{"", ""}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.indexTraces(batch); err != nil {
		t.Fatal(err)
	}
	partial := countEntries(t, s, project.ID)
	if partial == 0 {
		t.Fatal("the interrupted run indexed nothing to resume from")
	}

	if err := s.backfillSearchIndex(); err != nil {
		t.Fatalf("resumed backfill: %v", err)
	}
	if got := matchingTraces(t, s, project.ID, "refund"); len(got) != 2 {
		t.Errorf("the resumed backfill matched %v, want both traces", got)
	}
	// And the work already done was not done twice.
	var duplicates int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM (SELECT trace_id, observation_id, field, COUNT(*) AS n
		   FROM search_entries WHERE project_id = ?
		  GROUP BY trace_id, observation_id, field HAVING n > 1)`, project.ID).
		Scan(&duplicates); err != nil {
		t.Fatal(err)
	}
	if duplicates != 0 {
		t.Errorf("the resumed backfill re-indexed %d fields it had already done", duplicates)
	}
	checkIntegrity(t, s)
}

// TestSearchListingStillSeeksTheKeyset is Testing #5: `q` is one more
// condition, so the outer scan is still the keyset seek of spec 004 #4. A
// search that made the listing sort would have changed what a cursor means.
func TestSearchListingStillSeeksTheKeyset(t *testing.T) {
	s, project := readStore(t)
	query, err := ParseSearch("refund failed")
	if err != nil {
		t.Fatal(err)
	}
	sql, args := traceQuery(project.ID, TraceFilter{
		Limit:  50,
		Search: query,
		After:  &TraceCursor{Timestamp: 1, ID: hexTrace(1)},
	})
	plan, err := s.explainQueryPlan(sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_traces_timestamp") {
		t.Errorf("the listing with q does not use the keyset index:\n%s", joined)
	}
	if !strings.Contains(strings.ReplaceAll(joined, " ", ""), "(timestamp,id)<(?,?)") {
		t.Errorf("the cursor is not part of the index seek:\n%s", joined)
	}
	if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("the listing with q sorts rather than reads the index:\n%s", joined)
	}
	if !strings.Contains(joined, "search_fts") {
		t.Errorf("the plan does not reach the index at all:\n%s", joined)
	}
}

// TestSearchMatch is Testing #6: which observation and field a row matched, and
// a snippet cut on word boundaries around the first term.
func TestSearchMatch(t *testing.T) {
	s, project := readStore(t)
	searchCorpus(t, s, project.ID)

	query, err := ParseSearch("refund")
	if err != nil {
		t.Fatal(err)
	}
	match, err := s.SearchMatch(project.ID, hexTrace(1), query)
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("a trace the listing matched carries no match")
	}
	if match.ObservationID != hexSpan(1) || match.Field != FieldOutput {
		t.Errorf("match = %+v, want the observation's output", match)
	}
	if !strings.Contains(match.Snippet, "refund failed") {
		t.Errorf("snippet = %q, want the text around the hit", match.Snippet)
	}

	// A trace matched by its own name carries no observation id.
	nameQuery, err := ParseSearch("support-chat")
	if err != nil {
		t.Fatal(err)
	}
	byName, err := s.SearchMatch(project.ID, hexTrace(1), nameQuery)
	if err != nil {
		t.Fatal(err)
	}
	if byName == nil || byName.Field != FieldTraceName || byName.ObservationID != "" {
		t.Errorf("match = %+v, want the trace name with no observation", byName)
	}
}

// TestSnippetWindow: the window is bounded, cut on word boundaries, and centred
// on the first hit wherever in the text it is.
func TestSnippetWindow(t *testing.T) {
	query, err := ParseSearch("needle")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("alpha bravo charlie delta ", 40) + "needle in there " +
		strings.Repeat("echo foxtrot ", 40)

	for _, tc := range []struct {
		name string
		text string
	}{
		{"a hit in the middle", long},
		{"a hit at the start", "needle " + long},
		{"a hit at the very end", strings.Repeat("alpha bravo ", 60) + "needle"},
		{"no hit at all", strings.Repeat("alpha bravo ", 60)},
		{"a short text", "just a needle"},
		{"text that is not valid UTF-8", "before \xff\xfe needle after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snippet := Snippet(tc.text, query)
			if got := utf8.RuneCountInString(snippet); got > SearchSnippetLength {
				t.Errorf("snippet is %d characters, over the %d limit: %q",
					got, SearchSnippetLength, snippet)
			}
			if !utf8.ValidString(snippet) {
				t.Errorf("snippet is not valid UTF-8: %q", snippet)
			}
			if strings.Contains(tc.text, "needle") && !strings.Contains(snippet, "needle") {
				t.Errorf("snippet = %q, want the hit in it", snippet)
			}
			// Word boundaries: whatever is inside the ellipses is whole
			// words of the source text — read as runes, which is how the
			// snippet reads a text that is not valid UTF-8.
			body := strings.Trim(snippet, "…")
			source := collapseSpace(string([]rune(tc.text)))
			if body != "" && !strings.Contains(source, body) {
				t.Errorf("snippet %q is not a window of the text", body)
			}
		})
	}
}

// TestSnippetIsCutFromTheIndexedPrefix: only the first SearchIndexCap bytes are
// searched, so the window is cut from the same prefix — a word past the cap is
// not a hit and must not become one here.
func TestSnippetIsCutFromTheIndexedPrefix(t *testing.T) {
	if got := searchable(strings.Repeat("a", SearchIndexCap+10)); len(got) != SearchIndexCap {
		t.Errorf("searchable kept %d bytes, want the %d-byte cap", len(got), SearchIndexCap)
	}
	// The cut lands on a rune boundary, never inside a character.
	text := strings.Repeat("a", SearchIndexCap-1) + "é"
	if got := searchable(text); !utf8.ValidString(got) {
		t.Errorf("searchable cut a character in half: %q", got[len(got)-4:])
	}
}

// TestFoldSearch is the folding the snippet and the interface share: case and
// diacritics, the way the tokenizer folds them.
func TestFoldSearch(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Refund", "refund"},
		{"RÉFUND", "refund"},
		{"réfund", "refund"},
		{"réfund", "refund"},
		{"Ärger", "arger"},
		{"gpt", "gpt"},
	} {
		if got := foldSearch(tc.in); got != tc.want {
			t.Errorf("foldSearch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
