package notifications_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
)

func TestTeacherHomepageNotificationOptInScopesPagingUnreadAndBulkRead(t *testing.T) {
	f := setup(t)
	const requestID = "threq-examples"
	decision := n.Event{
		Source: "catalog", ID: "teacher-homepage-result", EventType: "catalog.teacher-homepage-request.resolved.v1",
		Subject: "teacher-homepage-request/" + requestID, Revision: 2, Recipients: []string{"alice"},
		Type: "business", Target: n.Target{Kind: "teacher_homepage_request", ID: requestID},
		TitleKey: "notifications.teacher_homepage_request.approved", TitleParams: map[string]string{"request_id": requestID},
	}
	deliver(t, f, delivery("legacy-old", "business"))
	f.now = f.now.Add(time.Second)
	deliver(t, f, delivery("legacy-new", "business"))
	f.now = f.now.Add(time.Second)
	deliver(t, f, decision)
	deliver(t, f, decision)

	handler := httpapi.Handler{Service: f.s, Authenticate: func(r *http.Request) (string, error) {
		return r.Header.Get("X-Owner"), nil
	}}
	request := func(owner, method, route string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/notifications"+route, nil)
		req.Header.Set("X-Owner", owner)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s returned %d instead of %d: %s", method, route, rec.Code, status, rec.Body.String())
		}
		return rec
	}
	readPage := func(owner, route string) n.Page {
		t.Helper()
		var page n.Page
		rec := request(owner, http.MethodGet, route, 200)
		if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	readUnread := func(route string, count int64, extended bool) {
		t.Helper()
		rec := request("alice", http.MethodGet, route, 200)
		var data struct {
			Count  int64           `json:"count"`
			ByBox  *n.UnreadByBox  `json:"by_box"`
			ByType *n.UnreadByType `json:"by_type"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&data); err != nil || data.Count != count {
			t.Fatalf("unread count differs: %+v, %v", data, err)
		}
		if extended && (data.ByBox == nil || data.ByType == nil || data.ByBox.Review != count || data.ByType.Business != count) {
			t.Fatalf("missing expanded unread counts: %+v", data)
		}
		if !extended && (data.ByBox != nil || data.ByType != nil) {
			t.Fatalf("legacy unread unexpectedly included expanded keys: %+v", data)
		}
	}

	first := readPage("alice", "?limit=1")
	if len(first.Items) != 1 || first.Items[0].Target.Kind == decision.Target.Kind || first.NextCursor == "" {
		t.Fatalf("compatibility filtering must run before paging: %+v", first)
	}
	second := readPage("alice", "?limit=1&cursor="+url.QueryEscape(first.NextCursor))
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.NextCursor != "" {
		t.Fatalf("incorrect filtered second page: %+v", second)
	}
	newView := readPage("alice", "?limit=1&include_extended=true")
	if len(newView.Items) != 1 || newView.Items[0].Target != decision.Target ||
		newView.Items[0].TitleKey != decision.TitleKey || newView.Items[0].TitleParams["request_id"] != requestID {
		t.Fatalf("new client did not receive the decision: %+v", newView)
	}
	request("alice", http.MethodGet, "?include_extended=true&cursor="+url.QueryEscape(first.NextCursor), 400)
	request("alice", http.MethodGet, "?cursor="+url.QueryEscape(newView.NextCursor), 400)
	request("alice", http.MethodGet, "/unread-count?include_extended=true&include_extended=false", 400)
	if page := readPage("bob", "?include_extended=true"); len(page.Items) != 0 {
		t.Fatal("another account read a private notification")
	}

	readUnread("/unread-count", 2, false)
	readUnread("/unread-count?include_extended=true", 3, true)
	request("alice", http.MethodPost, "/read-all", 204)
	readUnread("/unread-count", 0, false)
	readUnread("/unread-count?include_extended=true", 1, true)
	request("alice", http.MethodPost, "/read-all?include_extended=true", 204)
	readUnread("/unread-count?include_extended=true", 0, true)
	if page := readPage("alice", "?include_extended=true"); len(page.Items) != 3 {
		t.Fatal("visibility negotiation removed or duplicated stored notifications")
	}
}
