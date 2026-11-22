package notifications_test

import (
	"context"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/sdk"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	n "github.com/StuHelper/stuhelper-notifications"
)

func TestDeleteNotificationOwnershipIdempotencyAndNoResurrection(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	sensitive := n.Event{
		Recipients:  []string{"alice"},
		ID:          "report-receipt-1",
		Subject:     "report/1",
		Revision:    2,
		EventType:   "moderation.result.v1",
		Type:        "business",
		Source:      "moderation",
		Target:      n.Target{Kind: "report", ID: "report-1"},
		TitleKey:    "notifications.report.approved",
		TitleParams: map[string]string{"report_id": "report-1"},
	}
	firstResult := deliver(t, f, sensitive)
	sensitiveID := firstResult.Recipients[0].ID

	aggEvent := delivery("agg-del-1", "interaction")
	aggResult := deliver(t, f, aggEvent)
	aggID := aggResult.Recipients[0].ID

	srv := httptest.NewServer(httpapi.Handler{
		Service: f.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("X-Owner"), nil
		},
	})
	defer srv.Close()

	// Unauthenticated DELETE returns 401.
	unauthReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+"/api/v1/notifications/"+sensitiveID, nil)
	unauthRes, err := http.DefaultClient.Do(unauthReq)
	if err != nil || unauthRes.StatusCode != 401 {
		t.Fatal(err, unauthRes)
	}
	unauthRes.Body.Close()

	// Other user's DELETE returns 404 without removing Alice's notification.
	bobReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+"/api/v1/notifications/"+sensitiveID, nil)
	bobReq.Header.Set("X-Owner", "bob")
	bobRes, err := http.DefaultClient.Do(bobReq)
	if err != nil || bobRes.StatusCode != 404 {
		t.Fatal(err, bobRes)
	}
	bobRes.Body.Close()
	if count(t, f, "items") != 2 {
		t.Fatal("bob deleted alice's item")
	}

	// Non-standard UUID strings return 400 (never 503), and multi-segment or reserved paths return 404.
	for _, badID := range []string{
		"not-a-uuid",
		"urn:uuid:00000000-0000-0000-0000-000000000000",
		"{00000000-0000-0000-0000-000000000000}",
	} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+"/api/v1/notifications/"+badID, nil)
		req.Header.Set("X-Owner", "alice")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("DELETE %q expected 400, got %d", badID, res.StatusCode)
		}
	}
	for _, unknownPath := range []string{
		"/api/v1/notifications/",
		"/api/v1/notifications/a/b",
		"/api/v1/notifications/preferences",
		"/api/v1/notifications/unread-count",
		"/api/v1/notifications/read-all",
	} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+unknownPath, nil)
		req.Header.Set("X-Owner", "alice")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("DELETE %s expected 404, got %d", unknownPath, res.StatusCode)
		}
	}

	// Delete sensitive notification via generated Go SDK.
	client, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("X-Owner", "alice")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	parsedID := uuid.MustParse(sensitiveID)
	delResp, err := client.DeleteNotificationWithResponse(ctx, parsedID)
	if err != nil || delResp.StatusCode() != 204 {
		t.Fatal(err, delResp.StatusCode())
	}

	// Second delete returns 404, unread count drops to 1, and inbox + revisions remain preserved.
	delAgain, err := client.DeleteNotificationWithResponse(ctx, parsedID)
	if err != nil || delAgain.StatusCode() != 404 {
		t.Fatal(err, delAgain.StatusCode())
	}
	unread, err := f.s.Unread(ctx, "alice")
	if err != nil || unread != 1 || count(t, f, "items") != 1 || count(t, f, "inbox") != 2 || count(t, f, "revisions") != 2 {
		t.Fatalf("unexpected state after delete: unread=%d items=%d inbox=%d revisions=%d", unread, count(t, f, "items"), count(t, f, "inbox"), count(t, f, "revisions"))
	}

	// Replaying the original event returns the original result and does NOT resurrect the deleted notification.
	replay := deliver(t, f, sensitive)
	if !reflect.DeepEqual(replay, firstResult) || count(t, f, "items") != 1 {
		t.Fatalf("deleted notification resurrected on replay: %+v", replay)
	}

	// Older revision on the same subject remains ignored via preserved revisions table.
	stale := sensitive
	stale.ID = "report-receipt-stale"
	stale.Revision = 1
	staleRes := deliver(t, f, stale)
	if !staleRes.Ignored || count(t, f, "items") != 1 {
		t.Fatalf("stale revision after delete applied: %+v", staleRes)
	}

	// Deleting an unread interaction notification allows a subsequent new event to create a fresh notification with count=1.
	if err = f.s.Delete(ctx, "alice", aggID); err != nil {
		t.Fatal(err)
	}
	nextAgg := delivery("agg-del-2", "interaction")
	nextRes := deliver(t, f, nextAgg)
	if nextRes.Recipients[0].ID == "" || nextRes.Recipients[0].ID == aggID {
		t.Fatalf("expected fresh notification ID after delete, got %+v", nextRes)
	}
	page, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Count != 1 {
		t.Fatalf("unexpected items after fresh delivery: %+v, err=%v", page, err)
	}
	if err = f.s.CloseAccount(ctx, "alice", "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	closed, err := client.DeleteNotificationWithResponse(ctx, parsedID)
	if err != nil || closed.JSON403 == nil || closed.JSON403.Code != "forbidden" {
		t.Fatalf("closed owner delete: %+v %v", closed, err)
	}
}
