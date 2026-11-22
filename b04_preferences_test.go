package notifications_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/sdk"
)

func TestGranularReplyAndLikePreferences(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Read-only GET /preferences returns all-true defaults without inserting rows.
	initial, err := f.s.Preferences(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if initial != (n.Preferences{ReplyEnabled: true, LikeEnabled: true}) {
		t.Fatalf("unexpected default preferences: %+v", initial)
	}
	if count(t, f, "preferences") != 0 {
		t.Fatal("read-only preferences inserted database row")
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

	// Disable likes via the generated SDK and verify that replies still arrive.
	off := false
	on := true
	assertPreferences := func(resp *sdk.SetPreferencesResponse, err error, reply, like bool) {
		t.Helper()
		if err != nil || resp == nil || resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			t.Fatalf("set preferences failed: response=%+v err=%v", resp, err)
		}
		var prefs map[string]bool
		err = json.Unmarshal(resp.Body, &prefs)
		replyValue, hasReply := prefs["reply_enabled"]
		likeValue, hasLike := prefs["like_enabled"]
		if err != nil || len(prefs) != 2 || !hasReply || !hasLike || replyValue != reply || likeValue != like {
			t.Fatalf("unexpected preferences: %s, err=%v", resp.Body, err)
		}
	}
	setResp, err := client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{LikeEnabled: &off})
	assertPreferences(setResp, err, true, false)

	likeEvent := delivery("like-suppressed", "interaction")
	likeEvent.TitleKey = "notifications.interaction.like"
	likeRes := deliver(t, f, likeEvent)
	if likeRes.Recipients[0].ID != "" || likeRes.Recipients[0].SuppressionReason != n.SuppressionInteractionDisabled {
		t.Fatalf("like was not suppressed: %+v", likeRes)
	}

	replyEvent := delivery("reply-delivered", "interaction")
	replyEvent.TitleKey = "notifications.interaction.reply"
	replyRes := deliver(t, f, replyEvent)
	if replyRes.Recipients[0].ID == "" || replyRes.Recipients[0].SuppressionReason != "" {
		t.Fatalf("reply should be delivered when only likes are disabled: %+v", replyRes)
	}

	// Now flip: enable likes, disable replies.
	setResp, err = client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{ReplyEnabled: &off, LikeEnabled: &on})
	assertPreferences(setResp, err, false, true)
	replyBlocked := delivery("reply-suppressed", "interaction")
	replyBlocked.TitleKey = "notifications.interaction.reply"
	if res := deliver(t, f, replyBlocked); res.Recipients[0].SuppressionReason != n.SuppressionInteractionDisabled {
		t.Fatalf("reply was not suppressed: %+v", res)
	}
	likeAllowed := delivery("like-delivered", "interaction")
	likeAllowed.TitleKey = "notifications.interaction.like"
	if res := deliver(t, f, likeAllowed); res.Recipients[0].ID == "" {
		t.Fatalf("like should be delivered: %+v", res)
	}

	prefsResp, err := client.GetPreferencesWithResponse(ctx)
	if err != nil || prefsResp == nil || prefsResp.StatusCode() != http.StatusOK || prefsResp.JSON200 == nil ||
		prefsResp.JSON200.ReplyEnabled || !prefsResp.JSON200.LikeEnabled || strings.Contains(string(prefsResp.Body), "interaction_enabled") {
		t.Fatalf("unexpected current preferences: response=%+v err=%v", prefsResp, err)
	}

	// Rejected patch values must leave both saved preferences as they were.
	badBodies := []string{
		`{}`,
		`{"reply_enabled":null}`,
		`{"like_enabled":null}`,
		`{"interaction_enabled":true}`,
		`{"interaction_enabled":false,"reply_enabled":true}`,
		`{"reply_enabled":"true"}`,
		`{"reply_enabled":true,"extra":false}`,
	}
	for _, body := range badBodies {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+"/api/v1/notifications/preferences", strings.NewReader(body))
		req.Header.Set("X-Owner", "alice")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("body %s expected 400, got %d", body, res.StatusCode)
		}
	}

	current, err := f.s.Preferences(ctx, "alice")
	if err != nil || current != (n.Preferences{ReplyEnabled: false, LikeEnabled: true}) {
		t.Fatalf("preferences mutated after rejected input: %+v, err=%v", current, err)
	}
}

func TestPreferencesMigrationPreservesUserChoices(t *testing.T) {
	ctx := context.Background()
	f := setupDatabase(t, func(db *sql.DB) {
		_, err := db.Exec(`
		CREATE SCHEMA notifications;
		CREATE TABLE notifications.preferences (
			owner text PRIMARY KEY,
			interaction_enabled boolean NOT NULL DEFAULT true
		);
		INSERT INTO notifications.preferences(owner, interaction_enabled) VALUES
			('all_off', false),
			('all_on', true);
		ALTER TABLE notifications.preferences ADD COLUMN reply_enabled boolean;
		ALTER TABLE notifications.preferences ADD COLUMN like_enabled boolean;
		INSERT INTO notifications.preferences(owner, interaction_enabled, reply_enabled, like_enabled)
		VALUES ('likes_only', true, false, true), ('replies_only', true, true, false),
			('global_opt_out', false, true, true);
	`)
		if err != nil {
			t.Fatal(err)
		}
	})
	for owner, want := range map[string]n.Preferences{
		"all_off":        {},
		"all_on":         {ReplyEnabled: true, LikeEnabled: true},
		"likes_only":     {LikeEnabled: true},
		"replies_only":   {ReplyEnabled: true},
		"global_opt_out": {},
	} {
		if got, err := f.s.Preferences(ctx, owner); err != nil || got != want {
			t.Fatalf("%s: preferences=%+v want=%+v: %v", owner, got, want, err)
		}
	}
	var oldColumnExists bool
	if err := f.db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns WHERE table_schema='notifications'
		AND table_name='preferences' AND column_name='interaction_enabled'
	)`).Scan(&oldColumnExists); err != nil || oldColumnExists {
		t.Fatalf("former preference column remains: %t, err=%v", oldColumnExists, err)
	}
}
