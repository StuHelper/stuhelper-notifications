package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"github.com/StuHelper/stuhelper-notifications/sdk"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBoxTypeUnreadFilteringSummaryAndScopedMarkAllRead(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Deliver 35 distinct interaction notifications so the default 30-item unfiltered page is full.
	for i := range 35 {
		d := delivery(fmt.Sprintf("interaction-%d", i), "interaction")
		d.Target.ID = fmt.Sprintf("review-item-%d", i)
		deliver(t, f, d)
		f.now = f.now.Add(time.Second)
	}

	specials := []n.Event{
		{Recipients: []string{"alice"}, ID: "rev-reject", Subject: "mod/1", Revision: 1, EventType: "moderation.result.v1", Type: "business", Source: "moderation", Target: n.Target{Kind: "review", ID: "rev-1"}, TitleKey: "notifications.content.rejected", TitleParams: map[string]string{"content_id": "rev-1"}},
		{Recipients: []string{"alice"}, ID: "rep-approve", Subject: "mod/2", Revision: 1, EventType: "moderation.result.v1", Type: "business", Source: "moderation", Target: n.Target{Kind: "report", ID: "rep-1"}, TitleKey: "notifications.report.approved", TitleParams: map[string]string{"report_id": "rep-1"}},
		{Recipients: []string{"alice"}, ID: "stu-reject", Subject: "ver/1", Revision: 1, EventType: "verification.manual.rejected.v1", Type: "business", Source: "verification", Target: n.Target{Kind: "verification_application", ID: "app-1"}, TitleKey: "notifications.student_verification.rejected", TitleParams: map[string]string{"application_id": "app-1"}},
		{Recipients: []string{"alice"}, ID: "id-reject", Subject: "ver/2", Revision: 1, EventType: "verification.identity.rejected.v1", Type: "account", Source: "verification", Target: n.Target{Kind: "verification_operation", ID: "op-1"}, TitleKey: "notifications.identity_verification.rejected", TitleParams: map[string]string{"operation_id": "op-1"}},
		{Recipients: []string{"alice"}, ID: "sys-sec", Subject: "sys/1", Revision: 1, EventType: "system.security.v1", Type: "security", Source: "verification", Target: n.Target{Kind: "verification_operation", ID: "op-2"}, TitleKey: "notifications.system.security", TitleParams: map[string]string{"reference_id": "op-2"}},
		{Recipients: []string{"alice"}, ID: "sys-acc", Subject: "sys/2", Revision: 1, EventType: "system.account.v1", Type: "account", Source: "verification", Target: n.Target{Kind: "verification_credential", ID: "cred-1"}, TitleKey: "notifications.system.account", TitleParams: map[string]string{"reference_id": "cred-1"}},
	}
	// Put key notices earlier in time than the 35 interaction items to simulate interaction flooding.
	savedNow := f.now
	f.now = time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC)
	for _, ev := range specials {
		deliver(t, f, ev)
		f.now = f.now.Add(time.Second)
	}
	f.now = savedNow

	summary, err := f.s.UnreadSummary(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := n.UnreadSummary{
		Count:  41,
		ByBox:  n.UnreadByBox{Interaction: 35, Review: 2, Verification: 2, System: 2},
		ByType: n.UnreadByType{Interaction: 35, Business: 3, Security: 1, Account: 2},
	}
	if summary != wantSummary {
		t.Fatalf("unexpected unread summary: got %+v, want %+v", summary, wantSummary)
	}

	// Bob is completely isolated.
	bobSummary, err := f.s.UnreadSummary(ctx, "bob")
	if err != nil || bobSummary != (n.UnreadSummary{}) {
		t.Fatalf("unexpected bob summary: %+v, err=%v", bobSummary, err)
	}

	srv := httptest.NewServer(httpapi.Handler{
		Service: f.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("X-Owner"), nil
		},
	})
	defer srv.Close()

	client, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("X-Owner", "alice")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Unread endpoint through generated SDK.
	unreadResp, err := client.GetUnreadCountWithResponse(ctx)
	if err != nil || unreadResp.StatusCode() != 200 || unreadResp.JSON200 == nil {
		t.Fatal(err, unreadResp.StatusCode())
	}
	if unreadResp.JSON200.Count != 41 || unreadResp.JSON200.ByBox.Verification != 2 || unreadResp.JSON200.ByBox.Review != 2 || unreadResp.JSON200.ByType.Security != 1 {
		t.Fatalf("unexpected SDK unread response: %+v", unreadResp.JSON200)
	}

	// Verification box fetches both items on the first page despite 35 newer interactions.
	verBox := sdk.Box("verification")
	verResp, err := client.ListNotificationsWithResponse(ctx, &sdk.ListNotificationsParams{Box: &verBox})
	if err != nil || verResp.StatusCode() != 200 || verResp.JSON200 == nil || len(verResp.JSON200.Items) != 2 {
		t.Fatal(err, verResp.StatusCode())
	}

	// Paginate all 35 interaction notifications with limit=12 and verify cursor filter binding.
	intBox := sdk.Box("interaction")
	limit := 12
	cursor := ""
	seen := map[string]bool{}
	var firstInteractionCursor string
	for {
		params := &sdk.ListNotificationsParams{Box: &intBox, Limit: &limit}
		if cursor != "" {
			params.Cursor = &cursor
		}
		resp, e := client.ListNotificationsWithResponse(ctx, params)
		if e != nil || resp.StatusCode() != 200 || resp.JSON200 == nil {
			t.Fatal(e, resp.StatusCode())
		}
		for _, item := range resp.JSON200.Items {
			if seen[item.Id.String()] || item.Type != sdk.NotificationType("interaction") {
				t.Fatalf("unexpected item in interaction page: %+v", item)
			}
			seen[item.Id.String()] = true
		}
		if firstInteractionCursor == "" {
			firstInteractionCursor = resp.JSON200.NextCursor
		}
		cursor = resp.JSON200.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 35 {
		t.Fatalf("expected 35 interaction items, got %d", len(seen))
	}

	// Reusing an interaction cursor on another box or unfiltered list is rejected.
	revBox := sdk.Box("review")
	badCursorResp, err := client.ListNotificationsWithResponse(ctx, &sdk.ListNotificationsParams{Box: &revBox, Cursor: &firstInteractionCursor})
	if err != nil || badCursorResp.StatusCode() != 400 {
		t.Fatalf("expected 400 for cross-box cursor, got %v %d", err, badCursorResp.StatusCode())
	}

	// Scoped mark-all-read on interaction box leaves review, verification, and system unread.
	for _, query := range []string{"box=%ZZ", "box=review;ignored=1"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/notifications/read-all?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Owner", "alice")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid filter %q returned %d", query, res.StatusCode)
		}
	}
	afterInvalid, err := f.s.UnreadSummary(ctx, "alice")
	if err != nil || afterInvalid != summary {
		t.Fatalf("malformed filters changed unread notifications: %+v %v", afterInvalid, err)
	}
	markResp, err := client.MarkAllReadWithResponse(ctx, &sdk.MarkAllReadParams{Box: &intBox})
	if err != nil || markResp.StatusCode() != 204 {
		t.Fatal(err, markResp.StatusCode())
	}

	// Bob marking review read does not touch Alice's notifications.
	bobReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/notifications/read-all?box=review", nil)
	bobReq.Header.Set("X-Owner", "bob")
	bobRes, err := http.DefaultClient.Do(bobReq)
	if err != nil || bobRes.StatusCode != 204 {
		t.Fatal(err, bobRes)
	}
	bobRes.Body.Close()

	afterInteractionRead, err := f.s.UnreadSummary(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := n.UnreadSummary{
		Count:  6,
		ByBox:  n.UnreadByBox{Interaction: 0, Review: 2, Verification: 2, System: 2},
		ByType: n.UnreadByType{Interaction: 0, Business: 3, Security: 1, Account: 2},
	}
	if afterInteractionRead != wantAfter {
		t.Fatalf("unexpected unread summary after interaction read-all: got %+v, want %+v", afterInteractionRead, wantAfter)
	}

	// unread_only=true returns 0 items for interaction and 6 items across all boxes.
	unreadOnly := true
	intUnreadResp, err := client.ListNotificationsWithResponse(ctx, &sdk.ListNotificationsParams{Box: &intBox, UnreadOnly: &unreadOnly})
	if err != nil || intUnreadResp.StatusCode() != 200 || len(intUnreadResp.JSON200.Items) != 0 {
		t.Fatal(err, intUnreadResp.StatusCode())
	}
	allUnreadResp, err := client.ListNotificationsWithResponse(ctx, &sdk.ListNotificationsParams{UnreadOnly: &unreadOnly})
	if err != nil || allUnreadResp.StatusCode() != 200 || len(allUnreadResp.JSON200.Items) != 6 {
		t.Fatal(err, allUnreadResp.StatusCode())
	}

	// Scoped mark-all-read by type=security marks only the security notice read, preserving earlier read_at timestamps.
	var prevReadAt time.Time
	if err = f.db.QueryRow(`SELECT read_at FROM notifications.items WHERE owner='alice' AND box='interaction' LIMIT 1`).Scan(&prevReadAt); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Minute)
	secType := sdk.MarkAllReadParamsType("security")
	secMarkResp, err := client.MarkAllReadWithResponse(ctx, &sdk.MarkAllReadParams{Type: &secType})
	if err != nil || secMarkResp.StatusCode() != 204 {
		t.Fatal(err, secMarkResp.StatusCode())
	}
	var afterReadAt time.Time
	if err = f.db.QueryRow(`SELECT read_at FROM notifications.items WHERE owner='alice' AND box='interaction' AND read_at=$1 LIMIT 1`, prevReadAt).Scan(&afterReadAt); err != nil {
		t.Fatal("existing read_at timestamp was overwritten", err)
	}
	afterSecRead, err := f.s.UnreadSummary(ctx, "alice")
	if err != nil || afterSecRead.Count != 5 || afterSecRead.ByType.Security != 0 || afterSecRead.ByBox.System != 1 {
		t.Fatalf("unexpected summary after security read-all: %+v, err=%v", afterSecRead, err)
	}

	// Invalid and duplicate query parameters return 400.
	badRequests := []struct {
		method, target string
	}{
		{http.MethodGet, "/api/v1/notifications?box=unknown"},
		{http.MethodGet, "/api/v1/notifications?box=interaction&box=review"},
		{http.MethodGet, "/api/v1/notifications?type=unknown"},
		{http.MethodGet, "/api/v1/notifications?type=business&type=account"},
		{http.MethodGet, "/api/v1/notifications?unread_only=1"},
		{http.MethodGet, "/api/v1/notifications?unread_only=true&unread_only=false"},
		{http.MethodGet, "/api/v1/notifications?limit=10&limit=20"},
		{http.MethodGet, "/api/v1/notifications?unexpected=1"},
		{http.MethodPost, "/api/v1/notifications/read-all?box=unknown"},
		{http.MethodPost, "/api/v1/notifications/read-all?box=review&box=system"},
		{http.MethodPost, "/api/v1/notifications/read-all?type=unknown"},
		{http.MethodPost, "/api/v1/notifications/read-all?unexpected=1"},
	}
	for _, tc := range badRequests {
		req, _ := http.NewRequestWithContext(ctx, tc.method, srv.URL+tc.target, nil)
		req.Header.Set("X-Owner", "alice")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("%s %s expected 400, got %d", tc.method, tc.target, res.StatusCode)
		}
	}

	// Legacy Unread JSON compatibility: top-level count remains a non-negative integer.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/notifications/unread-count", nil)
	req.Header.Set("X-Owner", "alice")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var legacyUnread struct {
		Count int64 `json:"count"`
	}
	if json.NewDecoder(res.Body).Decode(&legacyUnread) != nil || legacyUnread.Count != 5 {
		t.Fatalf("legacy unread decode failed: %+v", legacyUnread)
	}

	// Service-level validation checks.
	if _, err = f.s.ListFiltered(ctx, "alice", n.ListQuery{Limit: 10, Box: "invalid"}); !errors.Is(err, n.ErrInput) {
		t.Fatal(err)
	}
	if err = f.s.MarkAllReadFiltered(ctx, "alice", n.ReadFilter{Type: "invalid"}); !errors.Is(err, n.ErrInput) {
		t.Fatal(err)
	}
}

func TestBoxColumnMigrationBackfillsExistingItems(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.db.Exec(`
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
			('00000000-0000-4000-8000-000000000001','alice','interaction','reviews','review','r1','notifications.interaction.like','{"content_id":"r1"}','2026-11-08T00:00:00Z'),
			('00000000-0000-4000-8000-000000000002','alice','business','moderation','report','rp1','notifications.report.approved','{"report_id":"rp1"}','2026-11-08T00:01:00Z'),
			('00000000-0000-4000-8000-000000000003','alice','account','verification','verification_removal_request','vr1','notifications.verification_revocation.approved','{"request_id":"vr1"}','2026-11-08T00:02:00Z'),
			('00000000-0000-4000-8000-000000000004','alice','security','verification','verification_operation','op1','notifications.system.security','{"reference_id":"op1"}','2026-11-08T00:03:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = postgres.Migrate(ctx, f.db); err != nil {
			t.Fatal(err)
		}
	}
	// Re-grant runtime permissions on the recreated items table.
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

	summary, err := f.s.UnreadSummary(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := n.UnreadSummary{
		Count:  4,
		ByBox:  n.UnreadByBox{Interaction: 1, Review: 1, Verification: 1, System: 1},
		ByType: n.UnreadByType{Interaction: 1, Business: 1, Security: 1, Account: 1},
	}
	if summary != want {
		t.Fatalf("backfilled summary mismatch: got %+v, want %+v", summary, want)
	}
}
