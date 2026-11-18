package notifications_test

import (
	"context"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyPreferencesAndUnreadOnlyMarkUpdates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 1. Read-only Preferences(owner, nil) must not insert rows or acquire FOR UPDATE locks.
	prefs, err := f.s.Preferences(ctx, "fresh-reader", nil)
	if err != nil || !prefs.InteractionEnabled {
		t.Fatalf("unexpected default preferences: %+v, err=%v", prefs, err)
	}
	if got := count(t, f, "preferences"); got != 0 {
		t.Fatalf("read-only GET preferences inserted %d row(s) into notifications.preferences", got)
	}

	off := false
	if _, err = f.s.Preferences(ctx, "alice", &off); err != nil {
		t.Fatal(err)
	}
	holdTx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holdTx.Rollback()
	if _, err = holdTx.ExecContext(ctx, `SELECT interaction_enabled FROM notifications.preferences WHERE owner='alice' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	fastCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	gotPrefs, err := f.s.Preferences(fastCtx, "alice", nil)
	if err != nil || gotPrefs.InteractionEnabled {
		t.Fatalf("read-only Preferences blocked on FOR UPDATE or returned wrong state: %+v, err=%v", gotPrefs, err)
	}
	_ = holdTx.Rollback()

	// 2. MarkRead and MarkAllRead must update only unread rows (read_at IS NULL), leaving read tuples untouched.
	ids := make([]string, 5)
	for i := range 5 {
		f.now = f.now.Add(time.Second)
		ids[i] = deliver(t, f, delivery(fmt.Sprintf("mark-%d", i), "business")).Recipients[0].ID
	}
	f.now = f.now.Add(time.Minute)
	if err = f.s.MarkRead(ctx, "alice", ids[0]); err != nil {
		t.Fatal(err)
	}

	tupleMeta := func(id string) string {
		t.Helper()
		var meta string
		if err := f.db.QueryRow(`SELECT ctid::text || ':' || xmin::text FROM notifications.items WHERE id=$1`, id).Scan(&meta); err != nil {
			t.Fatal(err)
		}
		return meta
	}

	firstReadMeta := tupleMeta(ids[0])
	f.now = f.now.Add(time.Minute)
	if err = f.s.MarkRead(ctx, "alice", ids[0]); err != nil {
		t.Fatal(err)
	}
	if afterReplay := tupleMeta(ids[0]); afterReplay != firstReadMeta {
		t.Fatalf("repeat MarkRead rewrote already-read tuple: before=%s after=%s", firstReadMeta, afterReplay)
	}

	f.now = f.now.Add(time.Minute)
	if err = f.s.MarkAllRead(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if afterAllRead := tupleMeta(ids[0]); afterAllRead != firstReadMeta {
		t.Fatalf("MarkAllRead rewrote already-read tuple ids[0]: before=%s after=%s", firstReadMeta, afterAllRead)
	}

	allMetas := make([]string, 5)
	for i, id := range ids {
		allMetas[i] = tupleMeta(id)
	}
	f.now = f.now.Add(time.Minute)
	if err = f.s.MarkAllRead(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if got := tupleMeta(id); got != allMetas[i] {
			t.Fatalf("second MarkAllRead rewrote already-read tuple %d: before=%s after=%s", i, allMetas[i], got)
		}
	}

	// Non-existent or other user's notification still returns ErrNotFound.
	if err = f.s.MarkRead(ctx, "bob", ids[0]); !errors.Is(err, n.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-owner MarkRead, got %v", err)
	}
	if err = f.s.MarkRead(ctx, "alice", "00000000-0000-4000-8000-000000000099"); !errors.Is(err, n.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing ID, got %v", err)
	}
}

func TestCursorListUsesCompositeIndexInGenericPlanAndValidatesBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	repo := &postgres.Repository{DB: f.db}

	for _, badLimit := range []int{-1, 0, 102, 10000} {
		if _, err := repo.List(ctx, "alice", n.Cursor{}, badLimit); !errors.Is(err, n.ErrInput) {
			t.Fatalf("repo.List limit=%d: expected ErrInput, got %v", badLimit, err)
		}
	}
	for _, badCursor := range []n.Cursor{
		{ID: "00000000-0000-4000-8000-000000000001"},
		{CreatedAt: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)},
	} {
		if _, err := repo.List(ctx, "alice", badCursor, 10); !errors.Is(err, n.ErrInput) {
			t.Fatalf("repo.List partial cursor %+v: expected ErrInput, got %v", badCursor, err)
		}
	}

	for i := range 45 {
		if i%3 == 0 {
			f.now = f.now.Add(time.Second)
		}
		deliver(t, f, delivery(fmt.Sprintf("page-%02d", i), "business"))
	}

	// Paginate all 45 items across 5 pages and verify strict ordering without duplicates.
	var collected []n.Notification
	cursor := ""
	for range 10 {
		page, err := f.s.List(ctx, "alice", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		collected = append(collected, page.Items...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(collected) != 45 {
		t.Fatalf("expected 45 items across cursor pages, got %d", len(collected))
	}
	for i := 1; i < len(collected); i++ {
		prev, cur := collected[i-1], collected[i]
		if cur.CreatedAt.After(prev.CreatedAt) || (cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID >= prev.ID) {
			t.Fatalf("cursor ordering violated at %d: prev=%v/%s cur=%v/%s", i, prev.CreatedAt, prev.ID, cur.CreatedAt, cur.ID)
		}
	}

	// Verify under PostgreSQL force_generic_plan that the cursor query pushes (created_at, id) into Index Cond without Filter.
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN (COSTS OFF) `+postgres.ListCursorQuery, "alice", collected[9].CreatedAt, collected[9].ID, 11)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var planLines []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	plan := strings.Join(planLines, "\n")
	if !strings.Contains(plan, "items_owner_cursor") || !strings.Contains(plan, "created_at") || strings.Contains(plan, "Filter:") {
		t.Fatalf("generic plan did not push cursor predicate into items_owner_cursor Index Cond:\n%s", plan)
	}
}
