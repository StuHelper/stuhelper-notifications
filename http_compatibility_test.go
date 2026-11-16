package notifications_test

import (
	"context"
	"encoding/json"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyHTTPParametersAndExplicitNameExpansion(t *testing.T) {
	f := setup(t)
	deliver(t, f, delivery("compat", "interaction"))
	h := httpapi.Handler{Service: f.s, Authenticate: func(*http.Request) (string, error) { return "alice", nil }}
	for _, query := range []string{"", "?include_content_names=false", "?include_content_names=true", "?include_content_names=yes", "?include_content_names=true&include_content_names=false"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/notifications"+query, nil))
		if query == "?include_content_names=yes" || query == "?include_content_names=true&include_content_names=false" {
			if rec.Code != 400 {
				t.Fatal(query, rec.Code)
			}
			continue
		}
		var page n.Page
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		want := 1
		if query == "?include_content_names=true" {
			want = 3
		}
		if len(page.Items[0].TitleParams) != want {
			t.Fatal("closed legacy parameters changed", query, page)
		}
		if query == "" || query == "?include_content_names=true" {
			if dir := os.Getenv("STUHELPER_B2_RESPONSE_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, map[bool]string{true: "notifications-expanded.json", false: "notifications-legacy.json"}[query != ""]), rec.Body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Existing Go callers retain exactly the original parameter object too.
	page, err := f.s.List(context.Background(), "alice", "", 10)
	if err != nil || len(page.Items[0].TitleParams) != 1 {
		t.Fatal(page, err)
	}
}
