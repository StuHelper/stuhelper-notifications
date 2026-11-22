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

func TestTeacherHomepageNotificationPagingUnreadAndBulkRead(t *testing.T) {
	f := setup(t)
	const requestID = "threq-examples"
	decision := n.Event{
		Source: "catalog", ID: "teacher-homepage-result", EventType: "catalog.teacher-homepage-request.resolved.v1",
		Subject: "teacher-homepage-request/" + requestID, Revision: 2, Recipients: []string{"alice"},
		Type: "business", Target: n.Target{Kind: "teacher_homepage_request", ID: requestID},
		TitleKey: "notifications.teacher_homepage_request.approved", TitleParams: map[string]string{"request_id": requestID},
	}
	interactionID := deliver(t, f, delivery("reply", "interaction")).Recipients[0].ID
	f.now = f.now.Add(time.Second)
	businessID := deliver(t, f, delivery("review-result", "business")).Recipients[0].ID
	f.now = f.now.Add(time.Second)
	decisionID := deliver(t, f, decision).Recipients[0].ID
	if replay := deliver(t, f, decision); replay.Recipients[0].ID != decisionID {
		t.Fatalf("decision replay changed the delivered item: %+v", replay)
	}

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
	readUnread := func(owner string, want n.UnreadSummary) {
		t.Helper()
		rec := request(owner, http.MethodGet, "/unread-count", 200)
		var got n.UnreadSummary
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil || got != want {
			t.Fatalf("unread summary=%+v want=%+v: %v", got, want, err)
		}
	}

	first := readPage("alice", "?limit=1")
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("missing first page: %+v", first)
	}
	item := first.Items[0]
	if item.ID != decisionID || item.Target != decision.Target || item.TargetStatus != "accessible" ||
		item.TitleKey != decision.TitleKey || len(item.TitleParams) != 1 || item.TitleParams["request_id"] != requestID {
		t.Fatalf("incorrect decision notification: %+v", item)
	}
	second := readPage("alice", "?cursor="+url.QueryEscape(first.NextCursor))
	if len(second.Items) != 2 || second.Items[0].ID != businessID || second.Items[1].ID != interactionID || second.NextCursor != "" {
		t.Fatalf("incorrect second page: %+v", second)
	}
	if page := readPage("bob", ""); len(page.Items) != 0 {
		t.Fatal("another account read a private notification")
	}
	readUnread("bob", n.UnreadSummary{})

	readUnread("alice", n.UnreadSummary{
		Count:  3,
		ByBox:  n.UnreadByBox{Interaction: 1, Review: 2},
		ByType: n.UnreadByType{Interaction: 1, Business: 2},
	})
	request("alice", http.MethodPost, "/read-all?box=review&type=business", 204)
	readUnread("alice", n.UnreadSummary{
		Count:  1,
		ByBox:  n.UnreadByBox{Interaction: 1},
		ByType: n.UnreadByType{Interaction: 1},
	})
	unread := readPage("alice", "?unread_only=true")
	if len(unread.Items) != 1 || unread.Items[0].ID != interactionID {
		t.Fatalf("bulk read updated the wrong notifications: %+v", unread)
	}
	f.now = f.now.Add(time.Second)
	rejection := decision
	rejection.ID = "second-teacher-homepage-result"
	rejection.Target.ID = "threq-second"
	rejection.Subject = "teacher-homepage-request/" + rejection.Target.ID
	rejection.TitleKey = "notifications.teacher_homepage_request.rejected"
	rejection.TitleParams = map[string]string{"request_id": rejection.Target.ID}
	rejectionID := deliver(t, f, rejection).Recipients[0].ID

	request("alice", http.MethodPost, "/read-all", 204)
	readUnread("alice", n.UnreadSummary{})
	if page := readPage("alice", ""); len(page.Items) != 4 || page.Items[0].ID != rejectionID || page.Items[0].ReadAt == nil {
		t.Fatalf("unexpected stored notifications after bulk read: %+v", page)
	}
}
