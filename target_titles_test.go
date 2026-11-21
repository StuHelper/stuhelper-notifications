package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type titleBatch struct {
	calls, legacy, refs    int
	state, name, contextID string
	fail                   bool
}

func (b *titleBatch) Resolve(context.Context, string, string, n.Target) (string, error) {
	b.legacy++
	return "", errors.New("single target resolution must not run")
}
func (b *titleBatch) ResolveBatch(ctx context.Context, owner string, refs []n.TargetRef) (map[n.TargetRef]n.TargetResolution, error) {
	b.calls++
	b.refs = len(refs)
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing deadline")
	}
	if b.fail {
		return nil, n.ErrUnavailable
	}
	out := map[n.TargetRef]n.TargetResolution{}
	for _, ref := range refs {
		out[ref] = n.TargetResolution{Status: b.state, Name: b.name, ContextID: b.contextID}
	}
	return out, nil
}
func TestNotificationNamesAreCurrentBoundedAndNeverPersisted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	batch := &titleBatch{state: "accessible", name: "合成课程评价标题"}
	var err error
	f.s, err = n.New(&postgres.Repository{DB: f.db}, batch, n.Config{ReadRetention: 24 * time.Hour, UnreadRetention: 24 * time.Hour, AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		d := delivery(fmt.Sprintf("event-%d", i), "business")
		d.Target.ID = fmt.Sprintf("review-%d", i%50)
		deliver(t, f, d)
	}
	check := func(state, name string) {
		t.Helper()
		page, e := f.s.ListWithContentNames(ctx, "alice", "", 100)
		if e != nil || len(page.Items) != 100 {
			t.Fatal(len(page.Items), e)
		}
		for _, item := range page.Items {
			if item.TitleParams["content_name_state"] != state || item.TitleParams["content_name"] != name || item.TitleParams["content_id"] != "example-1" {
				t.Fatal(item)
			}
			raw, _ := json.Marshal(item)
			var old struct {
				TitleKey    string `json:"title_key"`
				TitleParams struct {
					ContentID string `json:"content_id"`
				} `json:"title_params"`
			}
			if json.Unmarshal(raw, &old) != nil || old.TitleParams.ContentID != "example-1" {
				t.Fatal("legacy client failed")
			}
		}
	}
	check("available", batch.name)
	if batch.calls != 1 || batch.refs != 50 || batch.legacy != 0 {
		t.Fatal("N+1 resolution", batch)
	}
	batch.name = "改名后的标题"
	check("available", batch.name)
	batch.state = "inaccessible"
	check("inaccessible", "")
	batch.state = "accessible"
	batch.fail = true
	check("unavailable", "")
	batch.fail = false
	for _, bad := range []string{"", strings.Repeat("界", 257), "private\ntext", string([]byte{255})} {
		batch.name = bad
		check("unavailable", "")
	}
	batch.name = strings.Repeat("界", 256)
	check("available", batch.name)
	var stored int
	if err = f.db.QueryRow(`SELECT count(*) FROM notifications.items WHERE title_params ? 'content_name' OR title_params ? 'content_name_state'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("read title persisted", stored, err)
	}
	d := delivery("forged-title", "interaction")
	d.TitleParams["content_name"] = "producer supplied text"
	if _, err = f.s.Consume(ctx, d); !errors.Is(err, n.ErrInput) {
		t.Fatal("producer supplied name", err)
	}
	before := batch.calls
	if page, err := f.s.ListWithContentNames(ctx, "bob", "", 100); err != nil || len(page.Items) != 0 || batch.calls != before {
		t.Fatal("other owner lookup", page, err)
	}
	if _, err = f.s.ListWithContentNames(ctx, "", "", 100); !errors.Is(err, n.ErrUnauthenticated) || batch.calls != before {
		t.Fatal("unauthenticated lookup", err)
	}
}

type cancelOnStepResolver struct {
	calls     int
	cancelAt  int
	cancelFn  context.CancelFunc
	waitChild bool
}

func (r *cancelOnStepResolver) Resolve(ctx context.Context, owner, module string, target n.Target) (string, error) {
	r.calls++
	if r.cancelAt > 0 && r.calls == r.cancelAt && r.cancelFn != nil {
		r.cancelFn()
	}
	if r.waitChild {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "accessible", nil
}

type blockingBatchResolver struct {
	cancelParent context.CancelFunc
}

func (b *blockingBatchResolver) Resolve(context.Context, string, string, n.Target) (string, error) {
	return "", errors.New("unexpected legacy call")
}

func (b *blockingBatchResolver) ResolveBatch(ctx context.Context, owner string, refs []n.TargetRef) (map[n.TargetRef]n.TargetResolution, error) {
	if b.cancelParent != nil {
		b.cancelParent()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestParentContextCancellationAbortsTargetResolutionWithoutFake200(t *testing.T) {
	f := setup(t)
	bg := context.Background()
	for i := range 20 {
		d := delivery(fmt.Sprintf("cancel-%d", i), "business")
		d.Target.ID = fmt.Sprintf("review-%d", i)
		deliver(t, f, d)
	}

	// 1. Single-target Resolver: parent cancellation on step 3 must stop the loop immediately and return context.Canceled.
	parentCtx, cancelParent := context.WithCancel(bg)
	defer cancelParent()
	stepResolver := &cancelOnStepResolver{cancelAt: 3, cancelFn: cancelParent}
	svc, err := n.New(&postgres.Repository{DB: f.db}, stepResolver, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.List(parentCtx, "alice", "", 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if stepResolver.calls != 3 {
		t.Fatalf("expected loop to stop at call 3 after parent cancellation, got %d calls", stepResolver.calls)
	}

	// HTTP handler must return 503 (never 200 OK) when parent request context is canceled during target resolution.
	reqCtx, cancelReq := context.WithCancel(bg)
	defer cancelReq()
	httpResolver := &cancelOnStepResolver{cancelAt: 1, cancelFn: cancelReq}
	httpSvc, err := n.New(&postgres.Repository{DB: f.db}, httpResolver, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.Handler{
		Service:      httpSvc,
		Authenticate: func(*http.Request) (string, error) { return "alice", nil },
	}
	req := httptest.NewRequest("GET", "/api/v1/notifications?limit=20", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 on canceled request context, got %d %s", rec.Code, rec.Body.String())
	}

	// 2. BatchResolver: parent cancellation/deadline must propagate error instead of returning 200 unavailable.
	batchParent, cancelBatchParent := context.WithCancel(bg)
	defer cancelBatchParent()
	batchSvc, err := n.New(&postgres.Repository{DB: f.db}, &blockingBatchResolver{cancelParent: cancelBatchParent}, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = batchSvc.ListWithContentNames(batchParent, "alice", "", 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from BatchResolver, got %v", err)
	}

	// 3. Contrast: when parent ctx remains valid and ONLY child ResolveTimeout expires, List degrades to unavailable (200 OK).
	childTimeoutSvc, err := n.New(&postgres.Repository{DB: f.db}, &blockingBatchResolver{}, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    15 * time.Millisecond,
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	page, err := childTimeoutSvc.ListWithContentNames(bg, "alice", "", 20)
	if err != nil || len(page.Items) != 20 {
		t.Fatalf("child ResolveTimeout should degrade gracefully, got err=%v items=%d", err, len(page.Items))
	}
	for _, item := range page.Items {
		if item.TargetStatus != "unavailable" || item.TitleParams["content_name_state"] != "unavailable" {
			t.Fatalf("unexpected degraded item: %+v", item)
		}
	}
}

func TestCommentContextIDAndReasonCodeReadBehavior(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	batch := &titleBatch{state: "accessible", name: "数据结构课程评价", contextID: "review-root-42"}
	var err error
	f.s, err = n.New(&postgres.Repository{DB: f.db}, batch, n.Config{ReadRetention: 24 * time.Hour, UnreadRetention: 24 * time.Hour, AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}

	deliver(t, f, n.Event{
		Recipients:  []string{"alice"},
		ID:          "comment-reply-1",
		Subject:     "comment/101",
		Revision:    1,
		EventType:   "reviews.reply.v1",
		Type:        "interaction",
		Source:      "reviews",
		Target:      n.Target{Kind: "comment", ID: "comment-101"},
		TitleKey:    "notifications.interaction.reply",
		TitleParams: map[string]string{"content_id": "comment-101"},
	})
	deliver(t, f, n.Event{
		Recipients:  []string{"alice"},
		ID:          "resource-rejected-1",
		Subject:     "resource/202",
		Revision:    1,
		EventType:   "moderation.result.v1",
		Type:        "business",
		Source:      "moderation",
		Target:      n.Target{Kind: "resource", ID: "resource-202"},
		TitleKey:    "notifications.content.rejected",
		TitleParams: map[string]string{"content_id": "resource-202", "reason_code": "copyright_claim"},
	})

	// Producers cannot spoof read-only context_id.
	forged := delivery("forged-context", "interaction")
	forged.Target = n.Target{Kind: "comment", ID: "comment-999"}
	forged.TitleParams["context_id"] = "review-spoofed"
	if _, err = f.s.Consume(ctx, forged); !errors.Is(err, n.ErrInput) {
		t.Fatalf("expected ErrInput for producer-supplied context_id, got %v", err)
	}

	// Expanded read includes context_id on accessible comment targets, omits context_id on resource targets, and retains reason_code.
	page, err := f.s.ListWithContentNames(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	byID := map[string]n.Notification{}
	for _, it := range page.Items {
		byID[it.Target.Kind] = it
	}
	if byID["comment"].TitleParams["context_id"] != "review-root-42" || byID["comment"].TitleParams["content_name"] != "数据结构课程评价" {
		t.Fatalf("missing comment context_id: %+v", byID["comment"])
	}
	if _, hasCtx := byID["resource"].TitleParams["context_id"]; hasCtx {
		t.Fatalf("non-comment target must not expose context_id: %+v", byID["resource"])
	}
	if byID["resource"].TitleParams["reason_code"] != "copyright_claim" {
		t.Fatalf("missing reason_code: %+v", byID["resource"])
	}

	// Legacy List omits read-only parameters (content_name, content_name_state, context_id) while keeping stored reason_code.
	legacyPage, err := f.s.List(ctx, "alice", "", 10)
	if err != nil || len(legacyPage.Items) != 2 {
		t.Fatal(legacyPage, err)
	}
	for _, it := range legacyPage.Items {
		if _, hasCtx := it.TitleParams["context_id"]; hasCtx {
			t.Fatalf("legacy list leaked context_id: %+v", it)
		}
	}

	// Inaccessible, unavailable, or unsafe context_id values are never exposed.
	batch.state = "inaccessible"
	inaccPage, err := f.s.ListWithContentNames(ctx, "alice", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range inaccPage.Items {
		if _, hasCtx := it.TitleParams["context_id"]; hasCtx {
			t.Fatalf("inaccessible target must not expose context_id: %+v", it)
		}
	}
	batch.state = "accessible"
	for _, unsafeID := range []string{"", "review/evil", "review?x=1", "review#frag", "..", "bad\nid", strings.Repeat("r", 257)} {
		batch.contextID = unsafeID
		unsafePage, e := f.s.ListWithContentNames(ctx, "alice", "", 10)
		if e != nil {
			t.Fatal(e)
		}
		for _, it := range unsafePage.Items {
			if _, hasCtx := it.TitleParams["context_id"]; hasCtx {
				t.Fatalf("unsafe context_id %q exposed: %+v", unsafeID, it)
			}
		}
	}

	var persisted int
	if err = f.db.QueryRow(`SELECT count(*) FROM notifications.items WHERE title_params ? 'context_id'`).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatal("context_id persisted in database", persisted, err)
	}
}
