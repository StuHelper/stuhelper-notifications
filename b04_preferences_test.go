package notifications_test

import (
	"context"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"github.com/StuHelper/stuhelper-notifications/sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	n "github.com/StuHelper/stuhelper-notifications"
)

func TestGranularReplyAndLikePreferencesAndLegacyCompatibility(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Read-only GET /preferences returns all-true defaults without inserting rows.
	initial, err := f.s.Preferences(ctx, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if initial != (n.Preferences{InteractionEnabled: true, ReplyEnabled: true, LikeEnabled: true}) {
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

	// Disable only likes via generated SDK; replies remain active and interaction_enabled stays true.
	off := false
	setResp, err := client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{LikeEnabled: &off})
	if err != nil || setResp.StatusCode() != 200 || setResp.JSON200 == nil {
		t.Fatal(err, setResp.StatusCode())
	}
	if !setResp.JSON200.InteractionEnabled || !setResp.JSON200.ReplyEnabled || setResp.JSON200.LikeEnabled {
		t.Fatalf("unexpected preferences after disabling likes: %+v", setResp.JSON200)
	}

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
	on := true
	setResp, err = client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{ReplyEnabled: &off, LikeEnabled: &on})
	if err != nil || setResp.StatusCode() != 200 || !setResp.JSON200.InteractionEnabled || setResp.JSON200.ReplyEnabled || !setResp.JSON200.LikeEnabled {
		t.Fatalf("unexpected preferences after flipping: %+v, err=%v", setResp.JSON200, err)
	}
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

	// Disabling the remaining granular flag sets interaction_enabled to false.
	setResp, err = client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{LikeEnabled: &off})
	if err != nil || setResp.StatusCode() != 200 || setResp.JSON200.InteractionEnabled || setResp.JSON200.ReplyEnabled || setResp.JSON200.LikeEnabled {
		t.Fatalf("expected all flags false: %+v, err=%v", setResp.JSON200, err)
	}

	// Legacy PUT {"interaction_enabled": true} re-enables both reply and like.
	setResp, err = client.SetPreferencesWithResponse(ctx, sdk.SetPreferencesJSONRequestBody{InteractionEnabled: &on})
	if err != nil || setResp.StatusCode() != 200 || !setResp.JSON200.InteractionEnabled || !setResp.JSON200.ReplyEnabled || !setResp.JSON200.LikeEnabled {
		t.Fatalf("legacy master enable failed: %+v, err=%v", setResp.JSON200, err)
	}

	// Reject contradictory, null, empty, and unknown payloads without altering state.
	badBodies := []string{
		`{}`,
		`{"reply_enabled":null}`,
		`{"like_enabled":null}`,
		`{"interaction_enabled":true,"like_enabled":null}`,
		`{"interaction_enabled":false,"reply_enabled":true}`,
		`{"interaction_enabled":true,"reply_enabled":false,"like_enabled":false}`,
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

	current, err := f.s.Preferences(ctx, "alice", nil)
	if err != nil || current != (n.Preferences{InteractionEnabled: true, ReplyEnabled: true, LikeEnabled: true}) {
		t.Fatalf("preferences mutated after rejected input: %+v, err=%v", current, err)
	}
}

func TestPreferencesMigrationPreservesDisabledLegacyUsers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.db.Exec(`
		DROP TABLE notifications.preferences;
		CREATE TABLE notifications.preferences (
			owner text PRIMARY KEY,
			interaction_enabled boolean NOT NULL DEFAULT true
		);
		INSERT INTO notifications.preferences(owner, interaction_enabled) VALUES
			('legacy_off', false),
			('legacy_on', true);
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
		if _, err = f.db.Exec("GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.preferences TO " + r); err != nil {
			t.Fatal(err)
		}
	}

	offPrefs, err := f.s.Preferences(ctx, "legacy_off", nil)
	if err != nil || offPrefs != (n.Preferences{InteractionEnabled: false, ReplyEnabled: false, LikeEnabled: false}) {
		t.Fatalf("legacy_off migration mismatch: %+v, err=%v", offPrefs, err)
	}
	onPrefs, err := f.s.Preferences(ctx, "legacy_on", nil)
	if err != nil || onPrefs != (n.Preferences{InteractionEnabled: true, ReplyEnabled: true, LikeEnabled: true}) {
		t.Fatalf("legacy_on migration mismatch: %+v, err=%v", onPrefs, err)
	}
}
