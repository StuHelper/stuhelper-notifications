package notifications_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/sdk"
)

func TestHTTPRouteMatchingStrictQueryParamsAndSDKTypes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	itemID := deliver(t, f, delivery("a10-1", "interaction")).Recipients[0].ID
	deliver(t, f, delivery("a10-2", "interaction"))

	h := httpapi.Handler{
		Service: f.s,
		Authenticate: func(r *http.Request) (string, error) {
			if r.Header.Get("Test-Forbidden") == "1" {
				return "", n.ErrForbidden
			}
			return r.Header.Get("Test-Owner"), nil
		},
	}
	req := func(method, target, body, owner string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Test-Owner", owner)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. Subpaths that do not match single-segment /{id}/read must return 404 not_found (never 400).
	for _, unknownPath := range []string{
		"/api/v1/notifications/read",
		"/api/v1/notifications//read",
		"/api/v1/notifications/a/b/read",
		"/api/v1/notifications/" + itemID + "/extra/read",
	} {
		w := req("POST", unknownPath, "", "alice")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"not_found"`) {
			t.Fatalf("POST %s: expected 404 not_found, got %d %s", unknownPath, w.Code, w.Body.String())
		}
	}

	// 2. Duplicate, unknown, or malformed query parameters must return 400 invalid_input.
	for _, badQuery := range []string{
		"/api/v1/notifications?limit=10&limit=bad",
		"/api/v1/notifications?limit=10&limit=20",
		"/api/v1/notifications?cursor=&cursor=",
		"/api/v1/notifications?after=opaque",
		"/api/v1/notifications?%zz=1",
		"/api/v1/notifications?include_content_names=true",
		"/api/v1/notifications?include_extended=true",
		"/api/v1/notifications/unread-count?extra=1",
		"/api/v1/notifications/unread-count?include_extended=true",
		"/api/v1/notifications/preferences?extra=1",
		"/api/v1/notifications/preferences?include_extended=true",
		"/api/v1/notifications/read-all?extra=1",
		"/api/v1/notifications/read-all?include_extended=true",
		"/api/v1/notifications/" + itemID + "/read?extra=1",
	} {
		method := "GET"
		if strings.HasSuffix(strings.Split(badQuery, "?")[0], "/read") || strings.Contains(badQuery, "/read-all") {
			method = "POST"
		}
		w := req(method, badQuery, "", "alice")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_input"`) {
			t.Fatalf("%s %s: expected 400 invalid_input, got %d %s", method, badQuery, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"extra=1", "include_extended=true"} {
		if w := req("PUT", "/api/v1/notifications/preferences?"+query, `{"reply_enabled":false}`, "alice"); w.Code != http.StatusBadRequest {
			t.Fatalf("PUT preferences with query %s: expected 400, got %d", query, w.Code)
		}
	}

	// 3. Generated Go SDK contract: Notification.Id (UUID) passes directly to MarkReadWithResponse,
	// Notification.Count is int64, 204 omits Content-Type, and 403 maps to JSON403.
	srv := httptest.NewServer(h)
	defer srv.Close()

	authedClient, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Test-Owner", "alice")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	listResp, err := authedClient.ListNotificationsWithResponse(ctx, nil)
	if err != nil || listResp.StatusCode() != http.StatusOK || listResp.JSON200 == nil || len(listResp.JSON200.Items) != 1 {
		t.Fatalf("SDK ListNotifications failed: status=%d err=%v", listResp.StatusCode(), err)
	}
	gotItem := listResp.JSON200.Items[0]
	var countAsInt64 int64 = gotItem.Count
	if countAsInt64 != 2 || gotItem.Id.String() != itemID {
		t.Fatalf("SDK Notification field mismatch: count=%d id=%s", countAsInt64, gotItem.Id.String())
	}

	// Pass gotItem.Id directly to MarkReadWithResponse without type conversion.
	markResp, err := authedClient.MarkReadWithResponse(ctx, gotItem.Id)
	if err != nil || markResp.StatusCode() != http.StatusNoContent {
		t.Fatalf("SDK MarkRead failed: status=%d err=%v", markResp.StatusCode(), err)
	}
	if ct := markResp.HTTPResponse.Header.Get("Content-Type"); ct != "" {
		t.Fatalf("204 MarkRead must omit Content-Type, got %q", ct)
	}
	if cc := markResp.HTTPResponse.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", cc)
	}

	unreadResp, err := authedClient.GetUnreadCountWithResponse(ctx)
	if err != nil || unreadResp.StatusCode() != http.StatusOK || unreadResp.JSON200 == nil || unreadResp.JSON200.Count != 0 {
		t.Fatalf("SDK GetUnreadCount failed: %+v err=%v", unreadResp.JSON200, err)
	}

	prefsResp, err := authedClient.GetPreferencesWithResponse(ctx)
	if err != nil || prefsResp.StatusCode() != http.StatusOK || prefsResp.JSON200 == nil || !prefsResp.JSON200.ReplyEnabled || !prefsResp.JSON200.LikeEnabled {
		t.Fatalf("SDK GetPreferences failed: %+v err=%v", prefsResp.JSON200, err)
	}

	disabled := false
	setResp, err := authedClient.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{ReplyEnabled: &disabled})
	if err != nil || setResp.StatusCode() != http.StatusOK || setResp.JSON200 == nil || setResp.JSON200.ReplyEnabled || !setResp.JSON200.LikeEnabled {
		t.Fatalf("SDK SetPreferences failed: %+v err=%v", setResp.JSON200, err)
	}

	allReadResp, err := authedClient.MarkAllReadWithResponse(ctx, nil)
	if err != nil || allReadResp.StatusCode() != http.StatusNoContent || allReadResp.HTTPResponse.Header.Get("Content-Type") != "" {
		t.Fatalf("SDK MarkAllRead failed: status=%d ct=%q err=%v", allReadResp.StatusCode(), allReadResp.HTTPResponse.Header.Get("Content-Type"), err)
	}

	forbiddenClient, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Test-Forbidden", "1")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	forbidResp, err := forbiddenClient.ListNotificationsWithResponse(ctx, nil)
	if err != nil || forbidResp.StatusCode() != http.StatusForbidden || forbidResp.JSON403 == nil || forbidResp.JSON403.Code != "forbidden" {
		t.Fatalf("SDK 403 handling failed: status=%d body=%s err=%v", forbidResp.StatusCode(), string(forbidResp.Body), err)
	}

	// 4. Verify module.yaml declares identity optional dependency and notifications.inbox capability requirement.
	manifestBytes, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifestText := string(manifestBytes)
	if !strings.Contains(manifestText, "- id: identity") || !strings.Contains(manifestText, "capability_requires:") || !strings.Contains(manifestText, "notifications.inbox:") {
		t.Fatalf("module.yaml missing identity capability_requires:\n%s", manifestText)
	}
}
