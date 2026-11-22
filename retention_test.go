package notifications_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
)

func TestRetentionBoundariesAfterPublishedSchemaMigration(t *testing.T) {
	const (
		read179     = "00000000-0000-4000-8000-000000000179"
		read180     = "00000000-0000-4000-8000-000000000180"
		unread364   = "00000000-0000-4000-8000-000000000364"
		unread365   = "00000000-0000-4000-8000-000000000365"
		userDeleted = "00000000-0000-4000-8000-000000000001"
	)
	const day = 24 * time.Hour
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	read179at := now.Add(-179 * day)
	read180at := now.Add(-180 * day)
	f := setupDatabase(t, func(db *sql.DB) {
		// Exact notifications v0.1.2 schema, the last tagged production provider.
		schema, err := os.ReadFile("testdata/notifications-v0.1.2.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(schema)); err != nil {
			t.Fatal(err)
		}
		legacyItems := []struct {
			id        string
			kind      string
			title     string
			params    string
			createdAt time.Time
			readAt    *time.Time
		}{
			{read179, "business", "notifications.content.approved", "{}", now.Add(-400 * day), &read179at},
			{read180, "business", "notifications.content.approved", "{}", now.Add(-400 * day), &read180at},
			{unread364, "business", "notifications.content.approved", "{}", now.Add(-364 * day), nil},
			{unread365, "security", "notifications.content.approved", "{}", now.Add(-365 * day), nil},
			{userDeleted, "interaction", "notifications.interaction.like", `{"count":1}`, now.Add(-day), nil},
		}
		for _, item := range legacyItems {
			if _, err = db.Exec(`INSERT INTO notifications.items (id,owner,type,source_module,target_kind,target_id,title_key,title_params,created_at,read_at)
 VALUES ($1,'alice',$2,'reviews','review','review-1',$3,$4::jsonb,$5,$6)`,
				item.id, item.kind, item.title, item.params, item.createdAt, item.readAt); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO notifications.inbox (source,event_id,subject,revision,fingerprint,result)
 VALUES ('reviews',$1::text,$1,1,'fixture',jsonb_build_object('recipients',jsonb_build_array(jsonb_build_object('recipient','alice','id',$1))))`, item.id); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.now = now
	ctx := context.Background()
	service, err := n.New(&postgres.Repository{DB: f.runtime}, f.src, n.Config{
		ReadRetention: n.DefaultReadRetention, UnreadRetention: n.DefaultUnreadRetention,
		AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second,
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}

	// Read time survives repeat migration. A recent interaction timestamp does
	// not extend the unread deadline, and voluntary deletion takes effect now.
	if _, err = f.db.Exec(`UPDATE notifications.items SET updated_at=$1 WHERE id=$2::uuid`, now, unread365); err != nil {
		t.Fatal(err)
	}
	if err = service.Delete(ctx, "alice", userDeleted); err != nil {
		t.Fatal(err)
	}
	if err = service.Delete(ctx, "alice", userDeleted); !errors.Is(err, n.ErrNotFound) {
		t.Fatal(err)
	}
	page, err := service.List(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 2 ||
		page.Items[0].ID != unread364 || page.Items[0].ReadAt != nil ||
		page.Items[1].ID != read179 || page.Items[1].ReadAt == nil || !page.Items[1].ReadAt.Equal(read179at) {
		t.Fatalf("unexpected visible items: %+v, %v", page, err)
	}
	unreadPage, err := service.ListFiltered(ctx, "alice", n.ListQuery{Limit: 10, UnreadOnly: true, Type: "business"})
	if err != nil || len(unreadPage.Items) != 1 || unreadPage.Items[0].ID != unread364 {
		t.Fatalf("unexpected filtered unread items: %+v, %v", unreadPage, err)
	}
	if unread, err := service.UnreadSummary(ctx, "alice"); err != nil || unread.Count != 1 {
		t.Fatalf("unexpected unread summary: %+v, %v", unread, err)
	}
	for _, id := range []string{read180, unread365} {
		if err = service.MarkRead(ctx, "alice", id); !errors.Is(err, n.ErrNotFound) {
			t.Fatalf("marking due item as read: %v", err)
		}
	}
	if err = service.MarkAllReadFiltered(ctx, "alice", n.ReadFilter{Type: "security"}); err != nil {
		t.Fatal(err)
	}
	var stillUnread bool
	if err = f.db.QueryRow(`SELECT read_at IS NULL FROM notifications.items WHERE id=$1::uuid`, unread365).Scan(&stillUnread); err != nil || !stillUnread {
		t.Fatalf("bulk read extended an expired unread item: %t, %v", stillUnread, err)
	}
	if stats, err := service.MaintenanceStats(ctx); err != nil || stats.ExpiredItems != 2 {
		t.Fatalf("unexpected backlog after manual deletion: %+v, %v", stats, err)
	}
	if deleted, err := service.Cleanup(ctx); err != nil || deleted != 2 || count(t, f, "items") != 2 {
		t.Fatalf("cleanup at exact retention boundaries: deleted=%d, err=%v", deleted, err)
	}
	if deleted, err := service.Cleanup(ctx); err != nil || deleted != 0 || count(t, f, "inbox") != 5 || count(t, f, "like_subjects") != 1 {
		t.Fatalf("cleanup retry or deduplication facts: deleted=%d, err=%v", deleted, err)
	}
	f.now = now.Add(day)
	if err = service.MarkRead(ctx, "alice", read179); !errors.Is(err, n.ErrNotFound) {
		t.Fatalf("read timestamp did not reach its deadline: %v", err)
	}
	if deleted, err := service.Cleanup(ctx); err != nil || deleted != 2 || count(t, f, "items") != 0 {
		t.Fatalf("cleanup at next day's boundaries: deleted=%d, err=%v", deleted, err)
	}
}

func TestRetentionCleanupConcurrentAndRetries(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var lockedID string
	for i := range 5 {
		id := deliver(t, f, delivery(fmt.Sprintf("expired-%d", i), "business")).Recipients[0].ID
		if i%2 == 0 {
			if err := f.s.MarkRead(ctx, "alice", id); err != nil {
				t.Fatal(err)
			}
		}
		if i == 0 {
			lockedID = id
		}
	}
	f.now = f.now.Add(25 * time.Hour)
	deliver(t, f, delivery("keep-unread", "business"))

	if _, err := f.db.Exec(`CREATE FUNCTION notifications.reject_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic cleanup failure'; END $$;
 CREATE TRIGGER fail_cleanup AFTER DELETE ON notifications.items FOR EACH STATEMENT EXECUTE FUNCTION notifications.reject_cleanup()`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := f.s.Cleanup(ctx); deleted != 0 || n.UnavailableReason(err) != n.ReasonDatabaseException || count(t, f, "items") != 6 {
		t.Fatalf("failed cleanup was not rolled back or classified: deleted=%d, err=%v", deleted, err)
	}
	if _, err := f.db.Exec("DROP TRIGGER fail_cleanup ON notifications.items"); err != nil {
		t.Fatal(err)
	}

	lock, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.ExecContext(ctx, `SELECT 1 FROM notifications.items WHERE id=$1::uuid FOR UPDATE`, lockedID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		deleted int64
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			deleted, err := f.s.Cleanup(ctx)
			results <- result{deleted, err}
		}()
	}
	close(start)
	var removed int64
	for range 2 {
		got := <-results
		if got.err != nil || got.deleted < 0 || got.deleted > 2 {
			t.Fatalf("concurrent cleanup exceeded its batch or blocked: %+v", got)
		}
		removed += got.deleted
	}
	// The locked row must survive both batches. A full batch is required from
	// at least one worker; the other is allowed to skip rows the first has locked.
	var lockedExists bool
	if err = lock.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.items WHERE id=$1::uuid)`, lockedID).Scan(&lockedExists); err != nil ||
		!lockedExists || removed < 2 || removed > 4 || count(t, f, "items") != 6-int(removed) {
		t.Fatalf("concurrent cleanup deleted locked or duplicate rows: removed=%d, locked=%t, err=%v", removed, lockedExists, err)
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		deleted, err := f.s.Cleanup(ctx)
		if err != nil || deleted < 0 || deleted > 2 {
			t.Fatalf("retry cleanup: deleted=%d, err=%v", deleted, err)
		}
		removed += deleted
	}
	if removed != 5 || count(t, f, "items") != 1 || count(t, f, "inbox") != 6 {
		t.Fatalf("retry left an expired item or removed live data: removed=%d", removed)
	}
}
