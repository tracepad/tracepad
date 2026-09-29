package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The author of a score in the store (spec 048, Testing): what is written, what
// a read says about it as the account and the key change, and a database that
// held scores before schema 0031.

// authoredScore is one numeric score on a trace, written by author.
func authoredScore(id string, author *ScoreAuthor) *Score {
	one := 1.0
	return &Score{ID: id, TraceID: "trace-1", Name: "quality", DataType: ScoreNumeric,
		Value: &one, Timestamp: time.Now().UnixNano(), Author: author}
}

// authorOf reads one score back and returns its author.
func authorOf(t *testing.T, s *Store, projectID, id string) *ScoreAuthor {
	t.Helper()
	score, err := s.Score(t.Context(), projectID, id)
	if err != nil || score == nil {
		t.Fatalf("read score %s: %v %v", id, score, err)
	}
	return score.Author
}

func assertAuthor(t *testing.T, got *ScoreAuthor, want ScoreAuthor) {
	t.Helper()
	if got == nil {
		t.Fatalf("author = nil, want %+v", want)
	}
	if *got != want {
		t.Errorf("author = %+v, want %+v", *got, want)
	}
}

// TestScoreAuthorStandsAsTheAccountAndTheKeyChange: the copy survives every
// change to the credential, and the standing follows it (#2, #4, #7).
func TestScoreAuthorStandsAsTheAccountAndTheKeyChange(t *testing.T) {
	f := newAccountFixture(t)
	ada := f.invite(t, "ada@example.com", false, Membership{ProjectID: f.project.ID, Role: RoleEditor})
	named := "Ada"
	f.submit(t, &AccountUpdate{AccountID: ada.ID, Name: &named, Now: time.Now().UnixNano()})
	ada.Name = named

	f.submit(t, &KeyCreate{ProjectID: f.project.ID, Keys: KeyPair{PublicKey: "tp-pk-judge", Secret: "tp-sk-judge"},
		Name: "judge", Scopes: "ingest", Origin: OriginAccount(ada)})
	keys, err := f.ProjectKeys(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var judge *KeyInfo
	for i := range keys {
		if keys[i].PublicKey == "tp-pk-judge" {
			judge = &keys[i]
		}
	}
	if judge == nil {
		t.Fatal("the minted key is not listed")
	}

	f.submit(t, &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{
		authoredScore("s-account", AccountAuthor(ada)),
		authoredScore("s-key", KeyAuthor(judge)),
	}})
	byAda := ScoreAuthor{Kind: AuthorAccount, ID: ada.ID, Name: "Ada", Email: "ada@example.com"}
	byJudge := ScoreAuthor{Kind: AuthorKey, ID: "tp-pk-judge", Name: "judge"}

	step := func(what string, account, key string) {
		t.Helper()
		t.Run(what, func(t *testing.T) {
			want := byAda
			want.Standing = account
			assertAuthor(t, authorOf(t, f.Store, f.project.ID, "s-account"), want)
			want = byJudge
			want.Standing = key
			assertAuthor(t, authorOf(t, f.Store, f.project.ID, "s-key"), want)
		})
	}
	step("as written", RoleEditor, StandingActive)

	renamed := "Ada L."
	f.submit(t, &AccountUpdate{AccountID: ada.ID, Name: &renamed, Now: time.Now().UnixNano()})
	byAda.Name = renamed
	step("renamed: the name now", RoleEditor, StandingActive)

	empty := ""
	f.submit(t, &AccountUpdate{AccountID: ada.ID, Name: &empty, Now: time.Now().UnixNano()})
	byAda.Name = "Ada"
	step("renamed to nothing: the copy", RoleEditor, StandingActive)

	f.submit(t, &MembershipDelete{AccountID: ada.ID, ProjectID: f.project.ID})
	step("removed from the project", StandingRemoved, StandingActive)

	yes := true
	f.submit(t, &AccountUpdate{AccountID: ada.ID, Disabled: &yes, Now: time.Now().UnixNano()})
	step("disabled", StandingDisabled, StandingActive)

	authored, err := f.ScoresAuthoredBy(t.Context(), ada.ID)
	if err != nil || authored != 1 {
		t.Fatalf("ScoresAuthoredBy = %d, %v; want 1", authored, err)
	}
	f.submit(t, &AccountDelete{AccountID: ada.ID, Confirm: ada.Email})
	step("deleted: the copy stays", StandingDeleted, StandingActive)

	f.submit(t, &KeyRevoke{ProjectID: f.project.ID, PublicKey: "tp-pk-judge", Confirm: "test"})
	step("the key revoked: its name stays", StandingDeleted, StandingRevoked)

	// Nothing took the scores with it (#7).
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM scores`).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d scores after the account and the key went, want 2 (%v)", n, err)
	}
}

// TestTheLastWriterIsTheAuthor: a rewrite of the same id stamps the new caller
// and keeps nothing of the old one (#3).
func TestTheLastWriterIsTheAuthor(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.invite(t, "owner@example.com", true)
	judge := &KeyInfo{PublicKey: "tp-pk-test", Name: "default"}

	first := authoredScore("s-1", KeyAuthor(judge))
	first.Metadata = []byte(`{"source":"judge"}`)
	f.submit(t, &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{first}})
	second := authoredScore("s-1", AccountAuthor(owner))
	second.Metadata = first.Metadata
	f.submit(t, &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{second}})

	score, err := f.Score(t.Context(), f.project.ID, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	assertAuthor(t, score.Author, ScoreAuthor{Kind: AuthorAccount, ID: owner.ID,
		Email: "owner@example.com", Standing: RoleOwner})
	if string(score.Metadata) != `{"source":"judge"}` {
		t.Errorf("metadata = %s, want the source kept", score.Metadata)
	}
}

// TestScoresFilterByAuthor: the filter keeps one author's scores and pages
// in the listing's order (#9).
func TestScoresFilterByAuthor(t *testing.T) {
	f := newAccountFixture(t)
	ada := f.invite(t, "ada@example.com", true)
	key := &KeyInfo{PublicKey: "tp-pk-test", Name: "default"}
	var scores []*Score
	for i, author := range []*ScoreAuthor{AccountAuthor(ada), KeyAuthor(key), AccountAuthor(ada), nil, AccountAuthor(ada)} {
		score := authoredScore(scoreID(i+1), author)
		score.Timestamp = int64(i + 1)
		scores = append(scores, score)
	}
	f.submit(t, &ScoreWrite{ProjectID: f.project.ID, Scores: scores})

	page, err := f.Scores(t.Context(), f.project.ID, ScoreFilter{AuthorID: ada.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != scoreID(5) || page[1].ID != scoreID(3) {
		t.Fatalf("first page = %v, want scores 5 and 3", ids(page))
	}
	rest, err := f.Scores(t.Context(), f.project.ID, ScoreFilter{AuthorID: ada.ID, Limit: 2,
		After: &ScoreCursor{Timestamp: page[1].Timestamp, ID: page[1].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].ID != scoreID(1) {
		t.Fatalf("second page = %v, want score 1", ids(rest))
	}
	byKey, err := f.Scores(t.Context(), f.project.ID, ScoreFilter{AuthorID: key.PublicKey, Limit: 10})
	if err != nil || len(byKey) != 1 || byKey[0].ID != scoreID(2) {
		t.Fatalf("by key = %v, %v; want score 2", ids(byKey), err)
	}

	var plan, detail string
	var id, parent, unused int
	rows, err := f.db.Query(`EXPLAIN QUERY PLAN SELECT `+authoredScoreColumns+authoredScoreFrom+`
		 WHERE s.project_id = ? AND s.author_id = ? ORDER BY s.timestamp DESC, s.id DESC LIMIT 3`,
		f.project.ID, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "idx_scores_author") {
		t.Errorf("the author filter does not use idx_scores_author:\n%s", plan)
	}
}

// TestAScoreAuthorIsAnAccountOrAKey: the store refuses anything else, and the
// CHECKs refuse a half-written author (#2).
func TestAScoreAuthorIsAnAccountOrAKey(t *testing.T) {
	f := newAccountFixture(t)
	for _, author := range []*ScoreAuthor{{Kind: "person", ID: "x"}, {Kind: AuthorKey}} {
		err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID,
			Scores: []*Score{authoredScore("s-bad", author)}})
		if err == nil {
			t.Errorf("author %+v was written", *author)
		}
	}
	_, err := f.db.Exec(`INSERT INTO scores (project_id, id, name, data_type, value, timestamp, created_at,
		author_kind, author_id) VALUES (?, 'half', 'q', 'numeric', 1, 0, 0, 'key', 'tp-pk-test')`, f.project.ID)
	if err == nil {
		t.Error("an author without its names was stored")
	}
}

// TestScoresFromBeforeHaveNoAuthor: a database migrated from 0030 reads no
// author on the scores it held, and stamps every new one (#6).
func TestScoresFromBeforeHaveNoAuthor(t *testing.T) {
	path := openAtSchema(t, "0030_erasures.sql")
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, query := range []string{
			`INSERT INTO projects (id, name) VALUES ('p1', 'app')`,
			`INSERT INTO scores (project_id, id, trace_id, name, data_type, value, metadata, timestamp, created_at)
			 VALUES ('p1', 'old', 't1', 'quality', 'numeric', 1.0, '{"annotator":"ada"}', 1, 1)`,
		} {
			if _, err := db.Exec(query); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
	}()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if err := writer.Submit(t.Context(), &ScoreWrite{ProjectID: "p1", Scores: []*Score{
		authoredScore("new", KeyAuthor(&KeyInfo{PublicKey: "tp-pk-gone", Name: "ci"}))}}); err != nil {
		t.Fatal(err)
	}
	if author := authorOf(t, s, "p1", "old"); author != nil {
		t.Errorf("a score from before reads author %+v, want none", *author)
	}
	assertAuthor(t, authorOf(t, s, "p1", "new"),
		ScoreAuthor{Kind: AuthorKey, ID: "tp-pk-gone", Name: "ci", Standing: StandingRevoked})
}

func ids(scores []*Score) []string {
	out := make([]string, 0, len(scores))
	for _, score := range scores {
		out = append(out, score.ID)
	}
	return out
}
