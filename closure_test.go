package notifications_test

import (
	"context"
	"database/sql"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"github.com/StuHelper/stuhelper-notifications/sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccountClosureDeletesPersonalDataBlocksResurrectionAndEnforcesHTTP(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	off := false
	if _, e := f.s.Preferences(ctx, "alice", &off); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Preferences(ctx, "bob", &off); e != nil {
		t.Fatal(e)
	}
	preCloseEvent := delivery("pre-close", "business")
	preCloseEvent.Recipients = []string{"alice", "bob"}
	preCloseResult := deliver(t, f, preCloseEvent)
	aliceItemID := preCloseResult.Recipients[0].ID
	if aliceItemID == "" || count(t, f, "items") != 2 || count(t, f, "preferences") != 2 {
		t.Fatal("initial state missing", preCloseResult)
	}

	closureID := "11111111-1111-4111-8111-111111111111"
	for _, bad := range [][2]string{{"", closureID}, {" ", closureID}, {"alice", ""}, {"alice", " "}} {
		if e := f.s.CloseAccount(ctx, bad[0], bad[1]); !errors.Is(e, n.ErrInput) {
			t.Fatalf("expected ErrInput for %q/%q, got %v", bad[0], bad[1], e)
		}
	}

	// Failure mid-transaction must roll back both deletion and closure marker.
	_, e := f.db.Exec(`CREATE FUNCTION notifications.fail_close_pref_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic delete failure'; END $$; CREATE TRIGGER fail_close_pref_delete BEFORE DELETE ON notifications.preferences FOR EACH ROW EXECUTE FUNCTION notifications.fail_close_pref_delete()`)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.CloseAccount(ctx, "alice", closureID); !errors.Is(e, n.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on rollback, got %v", e)
	}
	if count(t, f, "items") != 2 || count(t, f, "preferences") != 2 || count(t, f, "closed_owners") != 0 {
		t.Fatal("failed closure mutated database")
	}
	if _, e = f.db.Exec(`DROP TRIGGER fail_close_pref_delete ON notifications.preferences; DROP FUNCTION notifications.fail_close_pref_delete()`); e != nil {
		t.Fatal(e)
	}

	// Concurrent idempotent closure with the same closureID must succeed once and delete only alice's rows.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.s.CloseAccount(ctx, "alice", closureID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var aliceItems, alicePrefs, closedCount int
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.items WHERE owner='alice'`).Scan(&aliceItems); e != nil || aliceItems != 0 {
		t.Fatal("alice items retained", aliceItems, e)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.preferences WHERE owner='alice'`).Scan(&alicePrefs); e != nil || alicePrefs != 0 {
		t.Fatal("alice preferences retained", alicePrefs, e)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.closed_owners WHERE owner='alice' AND closure_id=$1`, closureID).Scan(&closedCount); e != nil || closedCount != 1 {
		t.Fatal("closure fence missing", closedCount, e)
	}
	if count(t, f, "items") != 1 || count(t, f, "preferences") != 1 {
		t.Fatal("bob data affected by alice closure")
	}

	// CloseOwner alias behaves identically; mismatched closureID returns ErrConflict.
	if e = f.s.CloseOwner(ctx, "alice", closureID); e != nil {
		t.Fatal(e)
	}
	if e = f.s.CloseAccount(ctx, "alice", "22222222-2222-4222-8222-222222222222"); !errors.Is(e, n.ErrConflict) {
		t.Fatalf("expected ErrConflict on mismatched closureID, got %v", e)
	}

	// Delayed upstream outbox delivery after closure must suppress alice without re-inserting preferences or items, while delivering to bob.
	delayed := delivery("post-close", "business")
	delayed.Recipients = []string{"alice", "bob"}
	delayedResult := deliver(t, f, delayed)
	if len(delayedResult.Recipients) != 2 ||
		delayedResult.Recipients[0].Recipient != "alice" ||
		delayedResult.Recipients[0].ID != "" ||
		delayedResult.Recipients[0].SuppressionReason != n.SuppressionAccountClosed ||
		delayedResult.Recipients[1].Recipient != "bob" ||
		delayedResult.Recipients[1].ID == "" ||
		delayedResult.Recipients[1].SuppressionReason != "" {
		t.Fatal("unexpected post-close fanout result", delayedResult)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.items WHERE owner='alice'`).Scan(&aliceItems); e != nil || aliceItems != 0 {
		t.Fatal("delayed event resurrected alice items", aliceItems, e)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.preferences WHERE owner='alice'`).Scan(&alicePrefs); e != nil || alicePrefs != 0 {
		t.Fatal("delayed event resurrected alice preferences", alicePrefs, e)
	}

	// Replaying the pre-closure event returns the saved receipt without resurrecting alice's deleted item.
	if replay := deliver(t, f, preCloseEvent); replay.Recipients[0].ID != aliceItemID {
		t.Fatal("pre-closure replay changed", replay)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.items WHERE owner='alice'`).Scan(&aliceItems); e != nil || aliceItems != 0 {
		t.Fatal("replay resurrected alice item", aliceItems, e)
	}

	// All user-facing operations and HTTP routes reject closed owners with 403 Forbidden and write nothing.
	on := true
	if _, e = f.s.List(ctx, "alice", "", 10); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if _, e = f.s.ListWithContentNames(ctx, "alice", "", 10); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if _, e = f.s.Unread(ctx, "alice"); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if e = f.s.MarkRead(ctx, "alice", aliceItemID); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if e = f.s.MarkAllRead(ctx, "alice"); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if _, e = f.s.Preferences(ctx, "alice", nil); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	if _, e = f.s.Preferences(ctx, "alice", &on); !errors.Is(e, n.ErrForbidden) {
		t.Fatal(e)
	}
	h := httpapi.Handler{
		Service:      f.s,
		Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("Test-Owner"), nil },
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/notifications", ""},
		{"GET", "/api/v1/notifications/unread-count", ""},
		{"GET", "/api/v1/notifications/preferences", ""},
		{"PUT", "/api/v1/notifications/preferences", `{"interaction_enabled":true}`},
		{"POST", "/api/v1/notifications/" + aliceItemID + "/read", ""},
		{"POST", "/api/v1/notifications/read-all", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Test-Owner", "alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"forbidden"`) {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	server := httptest.NewServer(h)
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL, sdk.WithRequestEditorFn(func(_ context.Context, r *http.Request) error { r.Header.Set("Test-Owner", "alice"); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := client.GetPreferencesWithResponse(ctx, nil)
	if err != nil || prefs.JSON403 == nil || prefs.JSON403.Code != "forbidden" {
		t.Fatalf("SDK closed account response: %+v %v", prefs, err)
	}
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.preferences WHERE owner='alice'`).Scan(&alicePrefs); e != nil || alicePrefs != 0 {
		t.Fatal("forbidden access recreated alice preferences", alicePrefs, e)
	}
}

func TestInboxDetailRetentionCleanupAndLegacyUpgrade(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	ev1 := delivery("old-1", "business")
	ev2 := delivery("old-2", "business")
	f.now = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	deliver(t, f, ev1)
	deliver(t, f, ev2)

	f.now = f.now.Add(90 * 24 * time.Hour)
	evBoundary := delivery("boundary", "business")
	deliver(t, f, evBoundary)

	if _, e := postgres.CleanupInboxDetails(ctx, nil, f.now, 10); !errors.Is(e, n.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable for nil db", e)
	}
	for _, badLimit := range []int{0, 10001} {
		if _, e := postgres.CleanupInboxDetails(ctx, f.db, f.now, badLimit); !errors.Is(e, n.ErrInput) {
			t.Fatalf("expected ErrInput for limit=%d, got %v", badLimit, e)
		}
	}

	// Boundary timestamp (consumed_at == before) must NOT be pruned; batch limit=1 prunes one at a time.
	pruned, e := postgres.CleanupInboxDetails(ctx, f.db, f.now, 1)
	if e != nil || pruned != 1 {
		t.Fatal("first bounded prune failed", pruned, e)
	}
	pruned, e = postgres.CleanupInboxDetails(ctx, f.db, f.now, 10)
	if e != nil || pruned != 1 {
		t.Fatal("second bounded prune failed", pruned, e)
	}
	pruned, e = postgres.CleanupInboxDetails(ctx, f.db, f.now, 10)
	if e != nil || pruned != 0 {
		t.Fatal("idempotent prune should touch 0 rows", pruned, e)
	}

	var leaked int
	if e = f.db.QueryRow(`SELECT count(*) FROM notifications.inbox WHERE event_id IN ('old-1','old-2') AND result IS NOT NULL`).Scan(&leaked); e != nil || leaked != 0 {
		t.Fatal("expired inbox recipient details retained", leaked, e)
	}
	var boundaryResult sql.NullString
	if e = f.db.QueryRow(`SELECT result::text FROM notifications.inbox WHERE event_id='boundary'`).Scan(&boundaryResult); e != nil || !boundaryResult.Valid {
		t.Fatal("boundary inbox detail pruned prematurely", boundaryResult, e)
	}

	// Clear expired items first, then verify replaying a pruned >90d event neither resurrects items nor loses conflict detection.
	for count(t, f, "items") > 1 {
		if _, e = f.s.Cleanup(ctx); e != nil {
			t.Fatal(e)
		}
	}
	itemsBefore := count(t, f, "items")
	replayed := deliver(t, f, ev1)
	if !replayed.Ignored || len(replayed.Recipients) != 0 || count(t, f, "items") != itemsBefore {
		t.Fatal("pruned replay resurrected notification", replayed, count(t, f, "items"))
	}
	mutated := ev1
	mutated.Subject = "object/mutated"
	if _, e = f.s.Consume(ctx, mutated); !errors.Is(e, n.ErrConflict) {
		t.Fatal("pruned inbox lost fingerprint conflict check", e)
	}

	// Upgrade path: pre-existing inbox table without consumed_at and with NOT NULL result upgrades cleanly in place.
	_, e = f.db.Exec(`DROP TABLE notifications.inbox; CREATE TABLE notifications.inbox(source text NOT NULL,event_id text NOT NULL,subject text NOT NULL,revision bigint NOT NULL CHECK (revision>0),fingerprint text NOT NULL,result jsonb NOT NULL CHECK (jsonb_typeof(result->'recipients')='array' AND jsonb_array_length(result->'recipients') BETWEEN 1 AND 20),PRIMARY KEY(source,event_id)); INSERT INTO notifications.inbox VALUES('reviews','legacy-1','object/legacy',1,'hash','{"ignored":false,"recipients":[{"recipient":"alice","id":"00000000-0000-0000-0000-000000000001","suppression_reason":""}]}')`)
	if e != nil {
		t.Fatal(e)
	}
	if e = postgres.Migrate(ctx, f.db); e != nil {
		t.Fatal("in-place upgrade failed", e)
	}
	if _, e = f.db.Exec(`INSERT INTO notifications.inbox(source,event_id,subject,revision,fingerprint,result) VALUES('reviews','bad','object/bad',1,'hash','{}')`); e == nil {
		t.Fatal("malformed non-null result bypassed CHECK constraint")
	}
	pruned, e = postgres.CleanupInboxDetails(ctx, f.db, time.Now().UTC().Add(time.Hour), 10)
	if e != nil || pruned != 1 {
		t.Fatal("cleanup after legacy upgrade failed", pruned, e)
	}
}
