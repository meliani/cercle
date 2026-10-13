package rooms

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/meliani/cercle/hub/internal/db"
)

func TestLeaveHandsOverToLongestListener(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO circles (id, name, kind, created_at) VALUES ('c1', 'C', 'custom', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := New(d, clock)
	r, _ := m.Create(ctx, "c1", "Room", "alice")
	now = now.Add(time.Second)
	m.Join(ctx, r.ID, "carol")
	now = now.Add(time.Second)
	m.Join(ctx, r.ID, "bob")
	// Rejoining must not reset carol's seniority.
	m.Join(ctx, r.ID, "carol")

	r, closed, err := m.Leave(ctx, r.ID, "alice")
	if err != nil || closed || r.Host != "carol" {
		t.Fatalf("host → carol (joined first): %+v %v %v", r.Host, closed, err)
	}
	if _, err := m.SetState(ctx, r.ID, "bob", State{}); err == nil {
		t.Fatal("non-host cannot set state")
	}
	st, err := m.SetState(ctx, r.ID, "carol", State{Playing: true})
	if err != nil || st.State.Seq != 1 || st.State.UpdatedAt != now.UnixMilli() {
		t.Fatalf("state: %+v %v", st.State, err)
	}
	r, closed, _ = m.Leave(ctx, r.ID, "carol")
	if closed || r.Host != "bob" {
		t.Fatalf("host → bob: %+v", r)
	}
	_, closed, _ = m.Leave(ctx, r.ID, "bob")
	if !closed {
		t.Fatal("empty room closes")
	}
	if _, ok := m.Get(r.ID); ok {
		t.Fatal("closed room gone from memory")
	}
}
