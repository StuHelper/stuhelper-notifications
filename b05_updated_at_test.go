package notifications_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"github.com/StuHelper/stuhelper-notifications/sdk"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAggregationUpdatedAtFloatingOrderAndMigration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t0 := f.now
	first := deliver(t, f, delivery("reply-1", "interaction"))
	interactionID := first.Recipients[0].ID

	// 10 minutes later, deliver an unrelated business notification.
	f.now = t0.Add(10 * time.Minute)
	second := deliver(t, f, delivery("business-1", "business"))
	businessID := second.Recipients[0].ID

	// Before new aggregation, business-1 (t0+10m) sits above reply-1 (t0).
	page, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != businessID || page.Items[1].ID != interactionID {
		t.Fatalf("unexpected initial order: %+v, err=%v", page.Items, err)
	}
	if !page.Items[1].CreatedAt.Equal(t0) || !page.Items[1].UpdatedAt.Equal(t0) {
		t.Fatalf("initial timestamps mismatch: %+v", page.Items[1])
	}

	// 20 minutes after t0 (still inside the 1h AggregationWindow), deliver a second reply on the same target.
	f.now = t0.Add(20 * time.Minute)
	agg := deliver(t, f, delivery("reply-2", "interaction"))
	if agg.Recipients[0].ID != interactionID {
		t.Fatalf("expected aggregation into %s, got %s", interactionID, agg.Recipients[0].ID)
	}

	// Now the aggregated interaction item floats to the top with created_at=t0 and updated_at=t0+20m.
	page, err = f.s.List(ctx, "alice", "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != interactionID || page.Items[0].Count != 2 {
		t.Fatalf("aggregated item did not float to top: %+v, err=%v", page.Items, err)
	}
	if !page.Items[0].CreatedAt.Equal(t0) || !page.Items[0].UpdatedAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("unexpected timestamps after aggregation: created=%v updated=%v", page.Items[0].CreatedAt, page.Items[0].UpdatedAt)
	}

	// Second page via cursor yields business-1 without duplication.
	page2, err := f.s.List(ctx, "alice", page.NextCursor, 10)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID != businessID || page2.NextCursor != "" {
		t.Fatalf("unexpected second page: %+v, err=%v", page2, err)
	}

	// A cursor with a creation time instead of an update time is invalid.
	legacyRaw, _ := json.Marshal(struct {
		CreatedAt time.Time `json:"CreatedAt"`
		ID        string    `json:"ID"`
	}{CreatedAt: t0.Add(20 * time.Minute), ID: interactionID})
	legacyCursor := base64.RawURLEncoding.EncodeToString(legacyRaw)
	if _, err = f.s.List(ctx, "alice", legacyCursor, 10); !errors.Is(err, n.ErrInput) {
		t.Fatalf("creation-time cursor: expected invalid input, got %v", err)
	}

	// Rollback on failed inbox write restores both count and updated_at.
	_, err = f.db.Exec(`CREATE FUNCTION notifications.fail_b05_inbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic b05 failure'; END $$; CREATE TRIGGER fail_b05_inbox BEFORE INSERT ON notifications.inbox FOR EACH ROW EXECUTE FUNCTION notifications.fail_b05_inbox()`)
	if err != nil {
		t.Fatal(err)
	}
	f.now = t0.Add(30 * time.Minute)
	if _, err = f.s.Consume(ctx, delivery("reply-rollback", "interaction")); !errors.Is(err, n.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER fail_b05_inbox ON notifications.inbox`); err != nil {
		t.Fatal(err)
	}
	afterRollback, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || afterRollback.Items[0].Count != 2 || !afterRollback.Items[0].UpdatedAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("updated_at or count leaked after rollback: %+v", afterRollback.Items[0])
	}

	// MarkRead does not alter updated_at.
	f.now = t0.Add(40 * time.Minute)
	if err = f.s.MarkRead(ctx, "alice", interactionID); err != nil {
		t.Fatal(err)
	}
	afterRead, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || !afterRead.Items[0].UpdatedAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("MarkRead mutated updated_at: %+v", afterRead.Items[0])
	}

	// Generated Go SDK exposes UpdatedAt accurately.
	srv := httptest.NewServer(httpapi.Handler{
		Service:      f.s,
		Authenticate: func(*http.Request) (string, error) { return "alice", nil },
	})
	defer srv.Close()
	client, err := sdk.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sdkResp, err := client.ListNotificationsWithResponse(ctx, nil)
	if err != nil || sdkResp.StatusCode() != 200 || len(sdkResp.JSON200.Items) != 2 {
		t.Fatal(err, sdkResp.StatusCode())
	}
	if !sdkResp.JSON200.Items[0].UpdatedAt.Equal(t0.Add(20*time.Minute)) || !sdkResp.JSON200.Items[0].CreatedAt.Equal(t0) {
		t.Fatalf("SDK timestamps mismatch: %+v", sdkResp.JSON200.Items[0])
	}

	// Migrate an existing items table without updated_at column and verify backfill.
	_, err = f.db.Exec(`
		DROP TABLE notifications.items;
		CREATE TABLE notifications.items (
			id uuid PRIMARY KEY,
			owner text NOT NULL,
			type text NOT NULL CHECK (type IN ('interaction','business','security','account')),
			source_module text NOT NULL,
			target_kind text NOT NULL,
			target_id text NOT NULL,
			title_key text NOT NULL,
			title_params jsonb NOT NULL,
			created_at timestamptz NOT NULL,
			read_at timestamptz,
			count bigint NOT NULL DEFAULT 1 CHECK (count>0)
		);
		INSERT INTO notifications.items(id,owner,type,source_module,target_kind,target_id,title_key,title_params,created_at) VALUES
			('00000000-0000-4000-8000-000000000010','alice','business','reviews','review','r1','notifications.content.approved','{"content_id":"r1"}','2026-11-08T01:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = postgres.Migrate(ctx, f.db); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.db.Query(`SELECT rolname FROM pg_roles WHERE rolname LIKE 'notifications_%_runtime'`)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for rows.Next() {
		var r string
		if err = rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, r)
	}
	rows.Close()
	for _, r := range roles {
		if _, err = f.db.Exec("GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.items TO " + r); err != nil {
			t.Fatal(err)
		}
	}
	migrated, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || len(migrated.Items) != 1 || !migrated.Items[0].UpdatedAt.Equal(migrated.Items[0].CreatedAt) {
		t.Fatalf("migration backfill failed: %+v, err=%v", migrated, err)
	}
}

func TestUpdatedAtMigrationAcceptsPreviousWriter(t *testing.T) {
	f := setup(t)
	at := time.Date(2024, 5, 1, 10, 11, 12, 345000000, time.UTC)
	var updated time.Time
	err := f.db.QueryRow(`INSERT INTO notifications.items(id,owner,type,source_module,target_kind,target_id,title_key,title_params,created_at)
 VALUES(gen_random_uuid(),'compat-owner','interaction','reviews','review','compat-review','notifications.interaction.like','{}',$1)
 RETURNING updated_at`, at).Scan(&updated)
	if err != nil || !updated.Equal(at) {
		t.Fatalf("previous writer: updated %v want %v: %v", updated, at, err)
	}
}
