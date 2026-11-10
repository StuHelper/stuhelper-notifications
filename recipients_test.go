package notifications_test

import (
	"context"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConsumeRecipientsDeduplicationPreferencesAndReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	off := false
	if _, err := f.s.Preferences(ctx, "bob", &off); err != nil {
		t.Fatal(err)
	}
	e := delivery("batch", "interaction")
	e.Recipients = []string{"bob", "alice", "alice"}
	first := deliver(t, f, e)
	if !reflect.DeepEqual(e.Recipients, []string{"bob", "alice", "alice"}) {
		t.Fatal("mutated caller slice")
	}
	if len(first.Recipients) != 2 || first.Recipients[0].Recipient != "alice" || first.Recipients[0].ID == "" || first.Recipients[0].SuppressionReason != "" || first.Recipients[1].Recipient != "bob" || first.Recipients[1].ID != "" || first.Recipients[1].SuppressionReason != n.SuppressionInteractionDisabled {
		t.Fatal(first)
	}
	if count(t, f, "items") != 1 || count(t, f, "inbox") != 1 || count(t, f, "revisions") != 1 {
		t.Fatal("unexpected effects")
	}
	on := true
	if _, err := f.s.Preferences(ctx, "bob", &on); err != nil {
		t.Fatal(err)
	}
	e.Recipients = []string{"alice", "bob"}
	if replay := deliver(t, f, e); !reflect.DeepEqual(replay, first) {
		t.Fatal(replay)
	}
	if count(t, f, "items") != 1 {
		t.Fatal("replay wrote notification")
	}
	changed := e
	changed.Recipients = []string{"alice", "carol"}
	if _, err := f.s.Consume(ctx, changed); !errors.Is(err, n.ErrConflict) {
		t.Fatal(err)
	}
	newer := e
	newer.ID = "newer"
	newer.Revision = 3
	deliver(t, f, newer)
	stale := e
	stale.ID = "stale"
	stale.Revision = 2
	ignored := deliver(t, f, stale)
	if !ignored.Ignored || len(ignored.Recipients) != 2 {
		t.Fatal(ignored)
	}
	for _, entry := range ignored.Recipients {
		if entry.ID != "" || entry.SuppressionReason != n.SuppressionStaleRevision {
			t.Fatal(entry)
		}
	}
	if replay := deliver(t, f, e); !reflect.DeepEqual(replay, first) {
		t.Fatal(replay)
	}
}

func TestConsumeRecipientsBoundsAndAllDelivered(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, recipients := range [][]string{nil, {}, {""}, {"alice", " "}, {strings.Repeat("a", 257)}, make([]string, 21)} {
		e := delivery("invalid", "business")
		e.Recipients = recipients
		if _, err := f.s.Consume(ctx, e); !errors.Is(err, n.ErrInput) {
			t.Fatal(err)
		}
	}
	repeated := delivery("too-many", "business")
	for range 21 {
		repeated.Recipients = append(repeated.Recipients, "alice")
	}
	if _, err := f.s.Consume(ctx, repeated); !errors.Is(err, n.ErrInput) {
		t.Fatal(err)
	}
	if count(t, f, "items") != 0 || count(t, f, "inbox") != 0 || count(t, f, "revisions") != 0 {
		t.Fatal("invalid batch wrote data")
	}
	e := delivery("twenty", "business")
	e.Recipients = nil
	for i := range 20 {
		e.Recipients = append(e.Recipients, fmt.Sprintf("user-%02d", i))
	}
	result := deliver(t, f, e)
	if len(result.Recipients) != 20 {
		t.Fatal(result)
	}
	ids := map[string]bool{}
	for _, entry := range result.Recipients {
		if entry.ID == "" || ids[entry.ID] || entry.SuppressionReason != "" {
			t.Fatal(entry)
		}
		ids[entry.ID] = true
		page, err := f.s.List(ctx, entry.Recipient, "", 100)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != entry.ID {
			t.Fatal(page, err)
		}
	}
	if count(t, f, "items") != 20 || count(t, f, "inbox") != 1 {
		t.Fatal("batch not atomic")
	}
	if replay := deliver(t, f, e); !reflect.DeepEqual(replay, result) || count(t, f, "items") != 20 {
		t.Fatal(replay)
	}
}

func TestConsumeRecipientsRollbackAllWrites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e := delivery("batch", "interaction")
	e.Recipients = []string{"bob", "alice"}
	_, err := f.db.Exec(`CREATE FUNCTION notifications.fail_second_recipient() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.owner='bob' THEN RAISE EXCEPTION 'synthetic second recipient failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_second_recipient BEFORE INSERT ON notifications.items FOR EACH ROW EXECUTE FUNCTION notifications.fail_second_recipient()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Consume(ctx, e); !errors.Is(err, n.ErrUnavailable) {
		t.Fatal(err)
	}
	for _, table := range []string{"items", "inbox", "revisions", "preferences"} {
		if count(t, f, table) != 0 {
			t.Fatal("partial write", table)
		}
	}
	if _, err = f.db.Exec("DROP TRIGGER fail_second_recipient ON notifications.items"); err != nil {
		t.Fatal(err)
	}
	first := deliver(t, f, e)
	_, err = f.db.Exec(`CREATE FUNCTION notifications.reject_batch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic inbox failure'; END $$; CREATE TRIGGER reject_batch BEFORE INSERT ON notifications.inbox FOR EACH ROW EXECUTE FUNCTION notifications.reject_batch()`)
	if err != nil {
		t.Fatal(err)
	}
	next := e
	next.ID = "next"
	next.Revision = 2
	if _, err = f.s.Consume(ctx, next); !errors.Is(err, n.ErrUnavailable) {
		t.Fatal(err)
	}
	if count(t, f, "items") != 2 || count(t, f, "inbox") != 1 {
		t.Fatal("partial aggregate")
	}
	for _, entry := range first.Recipients {
		var aggregate int
		if err = f.db.QueryRow("SELECT count FROM notifications.items WHERE id=$1", entry.ID).Scan(&aggregate); err != nil || aggregate != 1 {
			t.Fatal(aggregate, err)
		}
	}
	var revision int
	if err = f.db.QueryRow("SELECT revision FROM notifications.revisions WHERE source=$1 AND subject=$2", e.Source, e.Subject).Scan(&revision); err != nil || revision != 1 {
		t.Fatal(revision, err)
	}
	if _, err = f.db.Exec("DROP TRIGGER reject_batch ON notifications.inbox"); err != nil {
		t.Fatal(err)
	}
	deliver(t, f, next)
}

func TestConsumeRecipientsConcurrentReplayAndOverlap(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result n.Result
		err    error
	}
	results := make(chan outcome, 12)
	for i := range 12 {
		go func(i int) {
			e := delivery("same", "business")
			e.Recipients = []string{"alice", "bob"}
			if i%2 == 0 {
				e.Recipients = []string{"bob", "alice", "bob"}
			}
			result, err := f.s.Consume(ctx, e)
			results <- outcome{result, err}
		}(i)
	}
	var first n.Result
	for i := range 12 {
		out := <-results
		if out.err != nil {
			t.Fatal(out.err)
		}
		if i == 0 {
			first = out.result
		} else if !reflect.DeepEqual(first, out.result) {
			t.Fatal("different replay", out.result)
		}
	}
	if count(t, f, "items") != 2 || count(t, f, "inbox") != 1 {
		t.Fatal("duplicate batch")
	}
	// Different source subjects overlap recipients in opposite input order.
	for i := range 12 {
		go func(i int) {
			e := delivery(fmt.Sprint(i), "business")
			e.Recipients = []string{"alice", "bob"}
			if i%2 == 0 {
				e.Recipients = []string{"bob", "alice"}
			}
			result, err := f.s.Consume(ctx, e)
			results <- outcome{result, err}
		}(i)
	}
	for range 12 {
		if out := <-results; out.err != nil {
			t.Fatal(out.err)
		}
	}
	if count(t, f, "items") != 26 || count(t, f, "inbox") != 13 {
		t.Fatal("overlapping batch lost")
	}
}

// Migration refuses to invent recipients for populated prior candidate receipts.
func TestBatchInboxMigrationPreservesExistingData(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			deliver(t, f, delivery("retained", "business"))
			_, err := f.db.Exec(`DROP TABLE notifications.inbox; CREATE TABLE notifications.inbox(source text NOT NULL,event_id text NOT NULL,subject text NOT NULL,revision bigint NOT NULL,fingerprint text NOT NULL,item_id uuid,suppressed boolean NOT NULL,ignored boolean NOT NULL,PRIMARY KEY(source,event_id),CHECK(NOT(suppressed AND ignored)),CHECK((item_id IS NOT NULL)=(NOT suppressed AND NOT ignored)))`)
			if err != nil {
				t.Fatal(err)
			}
			if populated {
				if _, err = f.db.Exec("INSERT INTO notifications.inbox VALUES('source','id','subject',1,'hash',NULL,true,false)"); err != nil {
					t.Fatal(err)
				}
			}
			err = postgres.Migrate(ctx, f.db)
			if populated {
				if err == nil || !strings.Contains(err.Error(), "recipient reconstruction") {
					t.Fatal(err)
				}
				if count(t, f, "inbox") != 1 {
					t.Fatal("receipt lost")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = postgres.Migrate(ctx, f.db); err != nil {
					t.Fatal(err)
				}
				var exists bool
				if err = f.db.QueryRow("SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='notifications' AND table_name='inbox' AND column_name='result')").Scan(&exists); err != nil || !exists {
					t.Fatal(exists, err)
				}
			}
			if count(t, f, "items") != 1 || count(t, f, "revisions") != 1 {
				t.Fatal("migration changed data")
			}
		})
	}
}
