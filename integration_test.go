package notifications_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type resolver struct {
	state string
	err   error
}

func (r *resolver) Resolve(ctx context.Context, owner, module string, target n.Target) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("missing deadline")
	}
	return r.state, r.err
}

type fixture struct {
	s   *n.Service
	db  *sql.DB
	src *resolver
	now time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("STUHELPER_NOTIFICATIONS_TEST_DSN")
	if dsn == "" {
		t.Fatal("isolated PostgreSQL required")
	}
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	name := fmt.Sprintf("notifications_%d", time.Now().UnixNano())
	role := name + "_runtime"
	if _, e = admin.Exec("CREATE DATABASE " + name); e != nil {
		t.Fatal(e)
	}
	owner, e := sql.Open("pgx", dsn+" dbname="+name)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		owner.Close()
		admin.Exec("DROP DATABASE " + name + " WITH(FORCE)")
		admin.Exec("DROP ROLE " + role)
		admin.Close()
	})
	for range 2 {
		if e = postgres.Migrate(context.Background(), owner); e != nil {
			t.Fatal(e)
		}
	}
	_, e = owner.Exec("CREATE ROLE " + role + " LOGIN; REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA notifications TO " + role + "; GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.items,notifications.preferences TO " + role + "; GRANT SELECT,INSERT,UPDATE ON notifications.revisions TO " + role + "; GRANT SELECT,INSERT ON notifications.inbox TO " + role + "; CREATE SCHEMA foreign_data; CREATE TABLE foreign_data.secret(value text)")
	if e != nil {
		t.Fatal(e)
	}
	runtime, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { runtime.Close() })
	repo := &postgres.Repository{DB: runtime}
	if e = repo.CheckRuntime(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = (&postgres.Repository{DB: owner}).CheckRuntime(context.Background()); e == nil {
		t.Fatal("migration identity accepted")
	}
	for _, query := range []string{"SELECT * FROM foreign_data.secret", "CREATE TABLE notifications.bad(id int)", "DELETE FROM notifications.inbox", "DELETE FROM notifications.revisions", "UPDATE notifications.inbox SET fingerprint='bad'"} {
		if _, e = runtime.Exec(query); e == nil {
			t.Fatalf("runtime allowed %s", query)
		}
	}
	f := &fixture{db: owner, src: &resolver{state: "accessible"}, now: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	f.s, e = n.New(repo, f.src, n.Config{Retention: 24 * time.Hour, AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second}, func() time.Time { return f.now })
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func delivery(key, kind string) n.Event {
	return n.Event{Recipients: []string{"alice"}, ID: key, Subject: "object/" + key, Revision: 1, EventType: "reviews.result.v1", Type: kind, Source: "reviews", Target: n.Target{Kind: "review", ID: "review-1"}, TitleKey: titleForType(kind), TitleParams: paramsForType(kind)}
}
func deliver(t *testing.T, f *fixture, d n.Event) n.Result {
	t.Helper()
	v, e := f.s.Consume(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func count(t *testing.T, f *fixture, table string) int {
	t.Helper()
	var v int
	if e := f.db.QueryRow("SELECT count(*) FROM notifications." + table).Scan(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestOwnershipPaginationReadAndTargetState(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ids := []string{}
	for i := range 5 {
		ids = append(ids, deliver(t, f, delivery(fmt.Sprint(i), "business")).Recipients[0].ID)
	}
	if count(t, f, "items") != 5 {
		t.Fatal("delivery count")
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		p, e := f.s.List(ctx, "alice", cursor, 2)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range p.Items {
			if seen[v.ID] || v.TargetStatus != "accessible" {
				t.Fatal("pagination or state")
			}
			seen[v.ID] = true
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatal(seen)
	}
	for _, limit := range []int{0, 101} {
		if _, e := f.s.List(ctx, "alice", "", limit); !errors.Is(e, n.ErrInput) {
			t.Fatal(e)
		}
	}
	if _, e := f.s.List(ctx, "alice", "garbage", 2); !errors.Is(e, n.ErrInput) {
		t.Fatal(e)
	}
	if _, e := f.s.List(ctx, "", "", 2); !errors.Is(e, n.ErrUnauthenticated) {
		t.Fatal(e)
	}
	p, e := f.s.List(ctx, "bob", "", 100)
	if e != nil || len(p.Items) != 0 {
		t.Fatal(p, e)
	}
	if e = f.s.MarkRead(ctx, "bob", ids[0]); !errors.Is(e, n.ErrNotFound) {
		t.Fatal(e)
	}
	unread, e := f.s.Unread(ctx, "alice")
	if e != nil || unread != 5 {
		t.Fatal(unread, e)
	}
	for range 2 {
		if e = f.s.MarkRead(ctx, "alice", ids[0]); e != nil {
			t.Fatal(e)
		}
		f.now = f.now.Add(time.Minute)
	}
	var read time.Time
	if e = f.db.QueryRow("SELECT read_at FROM notifications.items WHERE id=$1", ids[0]).Scan(&read); e != nil {
		t.Fatal(e)
	}
	if !read.Equal(f.now.Add(-2 * time.Minute)) {
		t.Fatal("read timestamp changed on replay")
	}
	unread, e = f.s.Unread(ctx, "alice")
	if e != nil || unread != 4 {
		t.Fatal(unread, e)
	}
	for range 2 {
		if e = f.s.MarkAllRead(ctx, "alice"); e != nil {
			t.Fatal(e)
		}
	}
	unread, e = f.s.Unread(ctx, "alice")
	if e != nil || unread != 0 {
		t.Fatal(unread, e)
	}
	f.src.state = "inaccessible"
	p, e = f.s.List(ctx, "alice", "", 100)
	if e != nil || p.Items[0].TargetStatus != "inaccessible" {
		t.Fatal(p, e)
	}
	f.src.err = context.DeadlineExceeded
	p, e = f.s.List(ctx, "alice", "", 100)
	if e != nil || p.Items[0].TargetStatus != "unavailable" {
		t.Fatal(p, e)
	}
}
func TestConcurrentDeliveryAggregationPreferencesAndCleanup(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := delivery("one", "interaction")
	var wg sync.WaitGroup
	results := make(chan n.Result, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); v, e := f.s.Consume(ctx, d); results <- v; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for v := range results {
		if id != "" && id != v.Recipients[0].ID {
			t.Fatal("different replay result")
		}
		id = v.Recipients[0].ID
	}
	if count(t, f, "items") != 1 || count(t, f, "inbox") != 1 {
		t.Fatal("duplicate write")
	}
	d.TitleKey = "notifications.interaction.like"
	if _, e := f.s.Consume(ctx, d); !errors.Is(e, n.ErrConflict) {
		t.Fatal(e)
	}
	v := deliver(t, f, delivery("two", "interaction"))
	if v.Recipients[0].ID != id {
		t.Fatal("not aggregated")
	}
	var aggregate int
	if e := f.db.QueryRow("SELECT count FROM notifications.items WHERE id=$1", id).Scan(&aggregate); e != nil || aggregate != 2 {
		t.Fatal(aggregate, e)
	}
	disabled := false
	if _, e := f.s.Preferences(ctx, "alice", &disabled); e != nil {
		t.Fatal(e)
	}
	blocked := delivery("off", "interaction")
	v = deliver(t, f, blocked)
	if v.Recipients[0].SuppressionReason == "" || v.Recipients[0].ID != "" || count(t, f, "items") != 1 {
		t.Fatal(v)
	}
	for _, kind := range []string{"business", "security", "account"} {
		v = deliver(t, f, delivery(kind, kind))
		if v.Recipients[0].SuppressionReason != "" {
			t.Fatal(kind)
		}
	}
	enabled := true
	if _, e := f.s.Preferences(ctx, "alice", &enabled); e != nil {
		t.Fatal(e)
	}
	if v = deliver(t, f, blocked); v.Recipients[0].SuppressionReason == "" {
		t.Fatal("suppressed replay entered inbox")
	}
	f.now = f.now.Add(time.Hour)
	v = deliver(t, f, delivery("boundary", "interaction"))
	if v.Recipients[0].ID == id {
		t.Fatal("window boundary aggregated")
	}
	if e := f.s.MarkRead(ctx, "alice", v.Recipients[0].ID); e != nil {
		t.Fatal(e)
	}
	next := deliver(t, f, delivery("after-read", "interaction"))
	if next.Recipients[0].ID == v.Recipients[0].ID {
		t.Fatal("read item aggregated")
	}
	f.now = f.now.Add(25 * time.Hour)
	before := count(t, f, "items")
	removed, e := f.s.Cleanup(ctx)
	if e != nil || removed != 2 || count(t, f, "items") != before-2 {
		t.Fatal(removed, e)
	}
	for count(t, f, "items") > 0 {
		if _, e = f.s.Cleanup(ctx); e != nil {
			t.Fatal(e)
		}
	}
	v = deliver(t, f, delivery("one", "interaction"))
	if v.Recipients[0].ID != id || count(t, f, "items") != 0 {
		t.Fatal("expired replay resurrected")
	}
}
func TestHTTPAuthenticationAndStrictPreferences(t *testing.T) {
	f := setup(t)
	id := deliver(t, f, delivery("one", "business")).Recipients[0].ID
	paths := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/notifications", "", 401},
		{"GET", "/api/v1/notifications/unread-count", "", 401},
		{"GET", "/api/v1/notifications/preferences", "", 401},
		{"PUT", "/api/v1/notifications/preferences", `{"interaction_enabled":false}`, 401},
		{"POST", "/api/v1/notifications/" + id + "/read", "", 401},
		{"POST", "/api/v1/notifications/read-all", "", 401},
	}
	h := httpapi.Handler{Service: f.s, Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("Test-Owner"), nil }}
	request := func(method, path, body, owner string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Test-Owner", owner)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, p := range paths {
		w := request(p.method, p.path, p.body, "")
		if w.Code != p.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("POST", "/api/v1/notifications/"+id+"/read", "", "bob"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request("GET", "/api/v1/notifications", "", "bob"); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"interaction_enabled":null}`, `{"interaction_enabled":false,"security_enabled":false}`, `{"interaction_enabled":false} {}`} {
		if w := request("PUT", "/api/v1/notifications/preferences", body, "alice"); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w := request("PUT", "/api/v1/notifications/preferences", `{"interaction_enabled":false}`, "alice"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if v := deliver(t, f, delivery("disabled", "interaction")); v.Recipients[0].SuppressionReason == "" {
		t.Fatal(v)
	}
	if w := request("GET", "/api/v1/notifications?limit=101", "", "alice"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request("GET", "/wrong", "", "alice"); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestDeliveryRollbackAndRetentionBoundary(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, e := f.db.Exec(`CREATE FUNCTION notifications.reject_inbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test inbox failure'; END $$; CREATE TRIGGER fail_inbox BEFORE INSERT ON notifications.inbox FOR EACH ROW EXECUTE FUNCTION notifications.reject_inbox()`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Consume(ctx, delivery("rollback", "business")); !errors.Is(e, n.ErrUnavailable) {
		t.Fatal(e)
	}
	if count(t, f, "items") != 0 || count(t, f, "inbox") != 0 || count(t, f, "revisions") != 0 {
		t.Fatal("partial transaction")
	}
	if _, e = f.db.Exec("DROP TRIGGER fail_inbox ON notifications.inbox"); e != nil {
		t.Fatal(e)
	}
	deliver(t, f, delivery("rollback", "business"))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = f.s.Consume(canceled, delivery("canceled", "business")); e == nil {
		t.Fatal("canceled delivery succeeded")
	}
	if count(t, f, "items") != 1 || count(t, f, "inbox") != 1 {
		t.Fatal("canceled delivery wrote data")
	}
	f.now = f.now.Add(24 * time.Hour)
	removed, e := f.s.Cleanup(ctx)
	if e != nil || removed != 0 {
		t.Fatal("retention boundary", removed, e)
	}
	deliver(t, f, delivery("new", "business"))
	f.now = f.now.Add(time.Second)
	removed, e = f.s.Cleanup(ctx)
	if e != nil || removed != 1 || count(t, f, "items") != 1 {
		t.Fatal("retention", removed, e)
	}
	p, e := f.s.List(ctx, "alice", "", 100)
	if e != nil || len(p.Items) != 1 {
		t.Fatal(p, e)
	}
}

func TestAggregationSeparatesTargetsParametersAndRecipients(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := deliver(t, f, delivery("one", "interaction"))
	for i := range 4 {
		d := delivery(fmt.Sprintf("different-%d", i), "interaction")
		switch i {
		case 0:
			d.TitleParams["content_id"] = "different"
		case 1:
			d.Target.ID = "review-2"
		case 2:
			d.Source = "catalog"
		case 3:
			d.Recipients = []string{"bob"}
		}
		if result := deliver(t, f, d); result.Recipients[0].ID == first.Recipients[0].ID {
			t.Fatal("unrelated event aggregated")
		}
	}
	if count(t, f, "items") != 5 {
		t.Fatal("wrong aggregation")
	}
	p, e := f.s.List(ctx, "bob", "", 100)
	if e != nil || len(p.Items) != 1 {
		t.Fatal(p, e)
	}
	d := delivery("invalid", "interaction")
	d.Target.ID = ""
	if _, e = f.s.Consume(ctx, d); !errors.Is(e, n.ErrInput) {
		t.Fatal(e)
	}
	if count(t, f, "inbox") != 5 {
		t.Fatal("invalid delivery wrote inbox")
	}
}

func TestAuthenticationDependencyFailure(t *testing.T) {
	f := setup(t)
	h := httpapi.Handler{Service: f.s, Authenticate: func(*http.Request) (string, error) { return "", n.ErrUnavailable }}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/notifications", nil))
	if w.Code != 503 {
		t.Fatal("authentication outage must be unavailable", w.Code)
	}
	if count(t, f, "items") != 0 {
		t.Fatal("auth failure wrote data")
	}
}

func titleForType(kind string) string {
	switch kind {
	case "interaction":
		return "notifications.interaction.reply"
	case "business":
		return "notifications.content.approved"
	default:
		return "notifications.system." + kind
	}
}
func paramsForType(kind string) map[string]string {
	name := "content_id"
	if kind == "security" || kind == "account" {
		name = "reference_id"
	}
	return map[string]string{name: "example-1"}
}
