package store

import (
	"database/sql"
	"testing"
)

// TestQueueItemsFromBeforeKeepTheirNames: a database from 0031 keeps the names
// its items were claimed and completed under, and an item holds a name or an
// account, never both (spec 048 #15).
func TestQueueItemsFromBeforeKeepTheirNames(t *testing.T) {
	path := openAtSchema(t, "0031_score_author.sql")
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, query := range []string{
			`INSERT INTO projects (id, name) VALUES ('p1', 'app')`,
			`INSERT INTO annotation_queues (project_id, name, description, score_configs, created_at, updated_at)
			 VALUES ('p1', 'review', '', '[]', 0, 0)`,
			`INSERT INTO annotation_items (project_id, queue, id, trace_id, status, seq, added_at, completed_by, completed_at)
			 VALUES ('p1', 'review', 'i1', 't1', 'completed', 1, 0, 'ada', 1)`,
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

	item, err := s.QueueItem(t.Context(), "p1", "review", "i1")
	if err != nil || item == nil {
		t.Fatalf("read the item: %v %v", item, err)
	}
	if item.CompletedBy != "ada" || item.CompletedByAccount != "" {
		t.Errorf("completed by %q/%q, want the name it had", item.CompletedBy, item.CompletedByAccount)
	}
	if _, err := s.db.Exec(`UPDATE annotation_items SET completed_by_account = 'acc' WHERE id = 'i1'`); err == nil {
		t.Error("an item finished by a name and an account at once was stored")
	}
}
