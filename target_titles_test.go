package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"strings"
	"testing"
	"time"
)

type titleBatch struct {
	calls, legacy, refs int
	state, name         string
	fail                bool
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
		out[ref] = n.TargetResolution{Status: b.state, Name: b.name}
	}
	return out, nil
}
func TestNotificationNamesAreCurrentBoundedAndNeverPersisted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	batch := &titleBatch{state: "accessible", name: "合成课程评价标题"}
	var err error
	f.s, err = n.New(&postgres.Repository{DB: f.db}, batch, n.Config{Retention: 24 * time.Hour, AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second}, func() time.Time { return f.now })
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
