package notifications_test

import (
	"context"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"reflect"
	"testing"
	"time"
)

func TestConsumeRevisionOrderingAndOriginalResults(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e := delivery("event-3", "business")
	e.Source = "moderation"
	e.EventType = "moderation.result.v1"
	e.Subject = "task/task-1"
	e.Revision = 3
	first := deliver(t, f, e)
	if first.Recipients[0].ID == "" || first.Ignored {
		t.Fatal(first)
	}
	// Complete results can skip revisions. Older and equal revisions have no effect.
	for _, id := range []string{"event-1", "event-2", "another-event-3"} {
		older := e
		older.ID = id
		if id == "event-1" {
			older.Revision = 1
		}
		if id == "event-2" {
			older.Revision = 2
		}
		ignored, err := f.s.Consume(ctx, older)
		if err != nil || !ignored.Ignored || ignored.Recipients[0].ID != "" || ignored.Recipients[0].SuppressionReason != n.SuppressionStaleRevision {
			t.Fatal(ignored, err)
		}
		again, err := f.s.Consume(ctx, older)
		if err != nil || !reflect.DeepEqual(again, ignored) {
			t.Fatal(again, err)
		}
	}
	latest := e
	latest.ID = "event-4"
	latest.Revision = 4
	deliver(t, f, latest)
	replay, err := f.s.Consume(ctx, e)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatal("replay must precede revision filtering", replay, err)
	}
	var revision int64
	if err = f.db.QueryRow("SELECT revision FROM notifications.revisions WHERE source=$1 AND subject=$2", e.Source, e.Subject).Scan(&revision); err != nil || revision != 4 {
		t.Fatal(revision, err)
	}
	if count(t, f, "items") != 2 || count(t, f, "inbox") != 5 {
		t.Fatal("unexpected business effects")
	}
	// Event identity is global within source, not scoped to recipient or subject.
	for _, mutate := range []func(*n.Event){
		func(v *n.Event) { v.Subject = "task/other" },
		func(v *n.Event) { v.Recipients = []string{"bob"} },
		func(v *n.Event) { v.Revision = 100 },
		func(v *n.Event) { v.EventType = "moderation.other.v1" },
	} {
		changed := e
		mutate(&changed)
		if _, err = f.s.Consume(ctx, changed); !errors.Is(err, n.ErrConflict) {
			t.Fatal(err)
		}
	}
	isolated := e
	isolated.Source = "verification"
	isolated.EventType = "verification.result.v1"
	isolated.Revision = 1
	deliver(t, f, isolated)
	if count(t, f, "items") != 3 || count(t, f, "revisions") != 2 {
		t.Fatal("source isolation failed")
	}
	if err = postgres.Migrate(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	replay, err = f.s.Consume(ctx, e)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatal("migration lost replay result", replay, err)
	}
}

func TestConsumeRollbackRestoresAggregateAndRevision(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e := delivery("first", "interaction")
	e.Subject = "review/shared"
	first := deliver(t, f, e)
	_, err := f.db.Exec(`CREATE FUNCTION notifications.fail_inbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic failure'; END $$; CREATE TRIGGER fail_inbox BEFORE INSERT ON notifications.inbox FOR EACH ROW EXECUTE FUNCTION notifications.fail_inbox()`)
	if err != nil {
		t.Fatal(err)
	}
	next := e
	next.ID = "next"
	next.Revision = 2
	if _, err = f.s.Consume(ctx, next); !errors.Is(err, n.ErrUnavailable) {
		t.Fatal(err)
	}
	var revision, aggregate int64
	if err = f.db.QueryRow("SELECT revision FROM notifications.revisions WHERE source=$1 AND subject=$2", e.Source, e.Subject).Scan(&revision); err != nil || revision != 1 {
		t.Fatal(revision, err)
	}
	if err = f.db.QueryRow("SELECT count FROM notifications.items WHERE id=$1", first.Recipients[0].ID).Scan(&aggregate); err != nil || aggregate != 1 {
		t.Fatal(aggregate, err)
	}
	if count(t, f, "inbox") != 1 {
		t.Fatal("failed consumption left inbox")
	}
	if _, err = f.db.Exec("DROP TRIGGER fail_inbox ON notifications.inbox"); err != nil {
		t.Fatal(err)
	}
	got := deliver(t, f, next)
	if got.Recipients[0].ID != first.Recipients[0].ID {
		t.Fatal("aggregate changed")
	}
	if err = f.db.QueryRow("SELECT count FROM notifications.items WHERE id=$1", first.Recipients[0].ID).Scan(&aggregate); err != nil || aggregate != 2 {
		t.Fatal(aggregate, err)
	}
	// A blocked revision lock observes the caller deadline and leaves no new receipt.
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT revision FROM notifications.revisions WHERE source=$1 AND subject=$2 FOR UPDATE", e.Source, e.Subject); err != nil {
		t.Fatal(err)
	}
	blocked := next
	blocked.ID = "blocked"
	blocked.Revision = 3
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err = f.s.Consume(deadline, blocked); err == nil {
		t.Fatal("deadline ignored")
	}
	if count(t, f, "inbox") != 2 {
		t.Fatal("blocked event recorded")
	}
}

func TestConsumeSuppressionAdvancesRevisionAndValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	off := false
	if _, err := f.s.Preferences(ctx, "alice", &off); err != nil {
		t.Fatal(err)
	}
	e := delivery("off", "interaction")
	e.Subject = "review/shared"
	e.Revision = 2
	suppressed := deliver(t, f, e)
	if suppressed.Recipients[0].SuppressionReason != n.SuppressionInteractionDisabled || suppressed.Ignored {
		t.Fatal(suppressed)
	}
	on := true
	if _, err := f.s.Preferences(ctx, "alice", &on); err != nil {
		t.Fatal(err)
	}
	stale := e
	stale.ID = "stale"
	stale.Revision = 1
	got := deliver(t, f, stale)
	if !got.Ignored || count(t, f, "items") != 0 {
		t.Fatal(got)
	}
	replay := deliver(t, f, e)
	if !reflect.DeepEqual(replay, suppressed) {
		t.Fatal(replay)
	}
	for _, mutate := range []func(*n.Event){
		func(v *n.Event) { v.Source = "" },
		func(v *n.Event) { v.ID = "" },
		func(v *n.Event) { v.Subject = "" },
		func(v *n.Event) { v.EventType = "" },
		func(v *n.Event) { v.Revision = 0 },
		func(v *n.Event) { v.Revision = -1 },
	} {
		bad := e
		mutate(&bad)
		if _, err := f.s.Consume(ctx, bad); !errors.Is(err, n.ErrInput) {
			t.Fatal(err)
		}
	}
	if count(t, f, "inbox") != 2 {
		t.Fatal("invalid event persisted")
	}
}

func TestConsumeConcurrentSameRevision(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	results := make(chan n.Result, 12)
	errs := make(chan error, 12)
	for i := range 12 {
		go func(i int) {
			e := delivery(fmt.Sprint(i), "business")
			e.Subject = "shared"
			e.Revision = 3
			result, err := f.s.Consume(ctx, e)
			results <- result
			errs <- err
		}(i)
	}
	applied := 0
	for range 12 {
		result := <-results
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if !result.Ignored {
			applied++
		}
	}
	if applied != 1 || count(t, f, "items") != 1 || count(t, f, "inbox") != 12 || count(t, f, "revisions") != 1 {
		t.Fatal("same revision applied more than once", applied)
	}
}
