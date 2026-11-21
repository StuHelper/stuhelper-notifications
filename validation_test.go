package notifications_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStrictUUIDValidationRejectsNonCanonicalFormatsWith400(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	realID := deliver(t, f, delivery("uuid-item", "business")).Recipients[0].ID

	h := httpapi.Handler{
		Service:      f.s,
		Authenticate: func(*http.Request) (string, error) { return "alice", nil },
	}

	nonCanonical := []string{
		"urn:uuid:00000000-0000-0000-0000-000000000000",
		"{00000000-0000-0000-0000-000000000000}",
		"!00000000-0000-0000-0000-000000000000?",
		"00000000000000000000000000000000",
		" 00000000-0000-0000-0000-000000000000",
		"00000000-0000-0000-0000-000000000000 ",
	}

	for _, badID := range nonCanonical {
		if err := f.s.MarkRead(ctx, "alice", badID); !errors.Is(err, n.ErrInput) {
			t.Fatalf("MarkRead(%q): expected ErrInput, got %v", badID, err)
		}

		rawCursor, _ := json.Marshal(n.Cursor{
			UpdatedAt: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
			ID:        badID,
		})
		encoded := base64.RawURLEncoding.EncodeToString(rawCursor)
		if _, err := f.s.List(ctx, "alice", encoded, 10); !errors.Is(err, n.ErrInput) {
			t.Fatalf("List cursor(%q): expected ErrInput, got %v", badID, err)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/notifications?cursor="+encoded, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_input"`) {
			t.Fatalf("HTTP GET cursor(%q): expected 400, got %d %s", badID, rec.Code, rec.Body.String())
		}
	}

	for _, pathID := range []string{
		"urn:uuid:00000000-0000-0000-0000-000000000000",
		"%7B00000000-0000-0000-0000-000000000000%7D",
		"00000000000000000000000000000000",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/notifications/"+pathID+"/read", nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_input"`) {
			t.Fatalf("HTTP POST read(%q): expected 400, got %d %s", pathID, rec.Code, rec.Body.String())
		}
	}

	// Canonical 36-char UUID (including uppercase hex) is normalized and marks the notification read.
	unreadBefore, err := f.s.Unread(ctx, "alice")
	if err != nil || unreadBefore != 1 {
		t.Fatal(unreadBefore, err)
	}
	if err := f.s.MarkRead(ctx, "alice", strings.ToUpper(realID)); err != nil {
		t.Fatalf("canonical uppercase UUID failed: %v", err)
	}
	unreadAfter, err := f.s.Unread(ctx, "alice")
	if err != nil || unreadAfter != 0 {
		t.Fatal(unreadAfter, err)
	}
}

func TestStrictUTF8ControlAndWhitespaceValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	badValues := []struct {
		name string
		val  string
	}{
		{"invalid_utf8_ff", string([]byte{'b', 'a', 'd', 0xff})},
		{"invalid_utf8_overlong", string([]byte{0xc0, 0xaf})},
		{"newline", "line1\nline2"},
		{"carriage_return", "line1\rline2"},
		{"tab", "col1\tcol2"},
		{"del_control", "abc\x7fdef"},
		{"nul", "abc\x00def"},
		{"leading_space", " alice"},
		{"trailing_space", "alice "},
	}

	fieldMutators := []struct {
		field  string
		mutate func(*n.Event, string)
	}{
		{"Source", func(e *n.Event, v string) { e.Source = v }},
		{"ID", func(e *n.Event, v string) { e.ID = v }},
		{"EventType", func(e *n.Event, v string) { e.EventType = v }},
		{"Subject", func(e *n.Event, v string) { e.Subject = v }},
		{"Target.Kind", func(e *n.Event, v string) { e.Target.Kind = v }},
		{"Target.ID", func(e *n.Event, v string) { e.Target.ID = v }},
		{"TitleKey", func(e *n.Event, v string) { e.TitleKey = v }},
		{"Recipients", func(e *n.Event, v string) { e.Recipients = []string{v} }},
	}

	for _, bv := range badValues {
		for _, fm := range fieldMutators {
			ev := delivery("valid-id", "business")
			fm.mutate(&ev, bv.val)
			if _, err := f.s.Consume(ctx, ev); !errors.Is(err, n.ErrInput) {
				t.Fatalf("Consume %s=%s: expected ErrInput, got %v", fm.field, bv.name, err)
			}
		}

		on := true
		const validUUID = "00000000-0000-0000-0000-000000000001"
		if _, err := f.s.List(ctx, bv.val, "", 10); !errors.Is(err, n.ErrInput) {
			t.Fatalf("List owner=%s: expected ErrInput, got %v", bv.name, err)
		}
		if _, err := f.s.Unread(ctx, bv.val); !errors.Is(err, n.ErrInput) {
			t.Fatalf("Unread owner=%s: expected ErrInput, got %v", bv.name, err)
		}
		if err := f.s.MarkRead(ctx, bv.val, validUUID); !errors.Is(err, n.ErrInput) {
			t.Fatalf("MarkRead owner=%s: expected ErrInput, got %v", bv.name, err)
		}
		if err := f.s.MarkAllRead(ctx, bv.val); !errors.Is(err, n.ErrInput) {
			t.Fatalf("MarkAllRead owner=%s: expected ErrInput, got %v", bv.name, err)
		}
		if _, err := f.s.Preferences(ctx, bv.val); !errors.Is(err, n.ErrInput) {
			t.Fatalf("Preferences owner=%s: expected ErrInput, got %v", bv.name, err)
		}
		if _, err := f.s.UpdatePreferences(ctx, bv.val, n.PreferencesUpdate{ReplyEnabled: &on, LikeEnabled: &on}); !errors.Is(err, n.ErrInput) {
			t.Fatalf("UpdatePreferences owner=%s: expected ErrInput, got %v", bv.name, err)
		}
	}

	for _, table := range []string{"items", "inbox", "revisions", "preferences"} {
		if got := count(t, f, table); got != 0 {
			t.Fatalf("malformed inputs wrote %d rows to %s", got, table)
		}
	}
}
