package notifications_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"sync"
	"testing"
	"time"
)

func TestClockSkewAndPreLockTimestampSamplingDoNotSplitInteractionAggregate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 1. Multi-instance slight NTP skew: replica B writes at t0+5ms, then replica A writes at t0+1ms.
	base := f.now
	f.now = base.Add(5 * time.Millisecond)
	evB := delivery("skew-b", "interaction")
	evB.Subject = "comment/c-b"
	resB := deliver(t, f, evB)

	f.now = base.Add(1 * time.Millisecond)
	evA := delivery("skew-a", "interaction")
	evA.Subject = "comment/c-a"
	resA := deliver(t, f, evA)

	if resA.Recipients[0].ID != resB.Recipients[0].ID || count(t, f, "items") != 1 {
		t.Fatalf("slight clock skew split unread interaction notification: idA=%s idB=%s items=%d",
			resA.Recipients[0].ID, resB.Recipients[0].ID, count(t, f, "items"))
	}

	// 2. Same-instance goroutine pre-lock race: Goroutine 1 enters Consume while recipient lock is held,
	// clock advances while waiting, and Goroutine 1 must sample time after acquiring recipient lock.
	f2 := setup(t)
	var clockMu sync.Mutex
	curNow := f2.now
	syncedSvc, err := n.New(&postgres.Repository{DB: f2.db}, f2.src, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
	}, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return curNow
	})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err = syncedSvc.Preferences(ctx, "alice", &enabled); err != nil {
		t.Fatal(err)
	}
	holdTx, err := f2.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holdTx.Rollback()
	if _, err = holdTx.ExecContext(ctx, `SELECT interaction_enabled FROM notifications.preferences WHERE owner='alice' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var g1Result n.Result
	var g1Err error
	wg.Add(1)
	go func() {
		defer wg.Done()
		ev := delivery("race-waiter", "interaction")
		ev.Subject = "comment/waiter"
		g1Result, g1Err = syncedSvc.Consume(ctx, ev)
	}()

	time.Sleep(25 * time.Millisecond)
	clockMu.Lock()
	later := curNow.Add(2 * time.Hour)
	curNow = later
	clockMu.Unlock()
	if err = holdTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if g1Err != nil {
		t.Fatal(g1Err)
	}
	page, err := syncedSvc.List(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	if !page.Items[0].CreatedAt.Equal(later) {
		t.Fatalf("Consume sampled clock before acquiring lock: got CreatedAt=%v, want %v", page.Items[0].CreatedAt, later)
	}
	_ = g1Result
}

func TestInteractionSubjectIsolationAndLikeToggleDeduplication(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 1. Out-of-order independent interaction entities (like rev=2 arriving before reply rev=1) both deliver.
	likeEv := delivery("like-bob-1", "interaction")
	likeEv.TitleKey = "notifications.interaction.like"
	likeEv.Subject = "like/review-1/bob"
	likeEv.Revision = 2
	likeRes := deliver(t, f, likeEv)

	replyEv := delivery("reply-carol-1", "interaction")
	replyEv.TitleKey = "notifications.interaction.reply"
	replyEv.Subject = "comment/c-1"
	replyEv.Revision = 1
	replyRes := deliver(t, f, replyEv)

	if likeRes.Ignored || replyRes.Ignored || likeRes.Recipients[0].ID == "" || replyRes.Recipients[0].ID == "" {
		t.Fatalf("independent interaction lost: like=%+v reply=%+v", likeRes, replyRes)
	}

	// 2. Reusing the same Subject across different interaction TitleKeys (like vs reply) must fail with ErrInput, never silently drop.
	crossReply := delivery("cross-reply", "interaction")
	crossReply.TitleKey = "notifications.interaction.reply"
	crossReply.Subject = "like/review-1/bob"
	crossReply.Revision = 1
	if _, err := f.s.Consume(ctx, crossReply); !errors.Is(err, n.ErrInput) {
		t.Fatalf("cross-kind interaction Subject reuse: expected ErrInput, got %v", err)
	}

	// Bare target ID as Subject on like events must be rejected to prevent multi-liker sequence collisions.
	for _, badSubj := range []string{"review-1", "review/review-1"} {
		badLike := delivery("bad-like-"+badSubj, "interaction")
		badLike.TitleKey = "notifications.interaction.like"
		badLike.Subject = badSubj
		if _, err := f.s.Consume(ctx, badLike); !errors.Is(err, n.ErrInput) {
			t.Fatalf("bare target Subject %q on like: expected ErrInput, got %v", badSubj, err)
		}
	}

	// 3. Like -> unlike -> re-like on the same (owner, source, subject) relation must NOT double-increment count.
	relikeEv := likeEv
	relikeEv.ID = "like-bob-3"
	relikeEv.Revision = 3
	relikeRes := deliver(t, f, relikeEv)
	if relikeRes.Recipients[0].ID != "" || relikeRes.Recipients[0].SuppressionReason != n.SuppressionDuplicateInteraction {
		t.Fatalf("expected duplicate_interaction suppression on re-like, got %+v", relikeRes)
	}

	var likeCount int64
	if err := f.db.QueryRow(`SELECT count FROM notifications.items WHERE id=$1`, likeRes.Recipients[0].ID).Scan(&likeCount); err != nil || likeCount != 1 {
		t.Fatalf("like toggle inflated aggregate count: got %d, err=%v", likeCount, err)
	}

	// Different liker on the same target DOES increment aggregate count to 2.
	carolLike := likeEv
	carolLike.ID = "like-carol-1"
	carolLike.Subject = "like/review-1/carol"
	carolLike.Revision = 1
	carolRes := deliver(t, f, carolLike)
	if carolRes.Recipients[0].ID != likeRes.Recipients[0].ID {
		t.Fatalf("distinct liker failed to aggregate: %+v", carolRes)
	}
	if err := f.db.QueryRow(`SELECT count FROM notifications.items WHERE id=$1`, likeRes.Recipients[0].ID).Scan(&likeCount); err != nil || likeCount != 2 {
		t.Fatalf("distinct liker count mismatch: got %d, err=%v", likeCount, err)
	}

	// Even after marking the like notification read, Bob toggling re-like again (rev=5) must not create a new unread item.
	if err := f.s.MarkRead(ctx, "alice", likeRes.Recipients[0].ID); err != nil {
		t.Fatal(err)
	}
	relikeAfterRead := likeEv
	relikeAfterRead.ID = "like-bob-5"
	relikeAfterRead.Revision = 5
	afterReadRes := deliver(t, f, relikeAfterRead)
	if afterReadRes.Recipients[0].ID != "" || afterReadRes.Recipients[0].SuppressionReason != n.SuppressionDuplicateInteraction {
		t.Fatalf("re-like after read created spam notification: %+v", afterReadRes)
	}
	unread, err := f.s.Unread(ctx, "alice")
	if err != nil || unread != 1 { // only replyRes remains unread
		t.Fatalf("unexpected unread count %d, err=%v", unread, err)
	}
}

func TestLegacyLikeReplayAndMigration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := delivery("legacy-root", "interaction")
	old.TitleKey = "notifications.interaction.like"
	old.Subject = old.Target.ID
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	// The previous writer accepted root target subjects and stored the canonical event digest.
	repo := &postgres.Repository{DB: f.db}
	before, err := repo.Consume(ctx, old, fmt.Sprintf("%x", sha256.Sum256(raw)), f.now, time.Hour)
	if err != nil || before.Recipients[0].ID == "" {
		t.Fatal(before, err)
	}
	independent := old
	independent.ID = "legacy-independent"
	independent.Subject = "like/first"
	saved := deliver(t, f, independent)
	if _, err = f.db.Exec(`DROP TABLE notifications.like_subjects; ALTER TABLE notifications.revisions DROP COLUMN interaction_title_key`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = postgres.Migrate(ctx, f.db); err != nil {
			t.Fatal(err)
		}
	}
	// The deployment grants the newly created table to the runtime role after migration.
	if _, err = f.db.Exec(`DO $$ BEGIN EXECUTE format('GRANT SELECT,INSERT ON notifications.like_subjects TO %I',current_database() || '_runtime'); END $$`); err != nil {
		t.Fatal(err)
	}
	replay, err := f.s.Consume(ctx, old)
	if err != nil || len(replay.Recipients) != 1 || replay.Recipients[0].ID != before.Recipients[0].ID {
		t.Fatalf("legacy replay: %+v %v", replay, err)
	}
	old.Revision++
	if _, err = f.s.Consume(ctx, old); !errors.Is(err, n.ErrConflict) {
		t.Fatalf("altered replay: %v", err)
	}
	old.ID = "new-invalid"
	if _, err = f.s.Consume(ctx, old); !errors.Is(err, n.ErrInput) {
		t.Fatalf("new invalid root: %v", err)
	}
	if err = f.s.MarkRead(ctx, "alice", saved.Recipients[0].ID); err != nil {
		t.Fatal(err)
	}
	independent.ID = "later"
	independent.Revision++
	again := deliver(t, f, independent)
	if again.Recipients[0].SuppressionReason != n.SuppressionDuplicateInteraction || count(t, f, "items") != 1 {
		t.Fatalf("pre-upgrade like delivered twice: %+v", again)
	}
	if err = f.s.CloseAccount(ctx, "alice", "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	var personal int
	if err = f.db.QueryRow("SELECT count(*) FROM notifications.like_subjects WHERE owner='alice'").Scan(&personal); err != nil || personal != 0 {
		t.Fatalf("closure kept personal deduplication rows: %d %v", personal, err)
	}
}

func TestLegacyLikeMigrationRollsBackWithoutEvidence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := delivery("expired-like", "interaction")
	d.TitleKey = "notifications.interaction.like"
	d.Subject = "like/expired"
	deliver(t, f, d)
	if _, err := f.db.Exec(`DROP TABLE notifications.like_subjects; ALTER TABLE notifications.revisions DROP COLUMN interaction_title_key; UPDATE notifications.inbox SET result=NULL`); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, f.db); err == nil {
		t.Fatal("missing delivery evidence accepted")
	}
	var clean bool
	if err := f.db.QueryRow(`SELECT to_regclass('notifications.like_subjects') IS NULL AND NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='notifications.revisions'::regclass AND attname='interaction_title_key' AND NOT attisdropped) AND NOT EXISTS(SELECT 1 FROM notifications.inbox WHERE result IS NOT NULL)`).Scan(&clean); err != nil || !clean {
		t.Fatal(clean, err)
	}

	// Rehearse reconstruction from the fixture source's delivery facts, leaving expired inbox details empty.
	if _, err := f.db.Exec(`ALTER TABLE notifications.revisions ADD COLUMN interaction_title_key text NOT NULL DEFAULT '';
 CREATE TABLE notifications.like_subjects(owner text NOT NULL,source text NOT NULL,subject text NOT NULL,PRIMARY KEY(owner,source,subject));
 INSERT INTO notifications.like_subjects VALUES('alice','reviews','like/expired');
 UPDATE notifications.revisions SET interaction_title_key='notifications.interaction.like' WHERE source='reviews' AND subject='like/expired';
 DO $$ BEGIN EXECUTE format('GRANT SELECT,INSERT ON notifications.like_subjects TO %I',current_database() || '_runtime'); END $$`); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if got := deliver(t, f, d); !got.Ignored || len(got.Recipients) != 0 {
		t.Fatal(got)
	}
	d.ID = "after-reconstruction"
	d.Revision++
	if got := deliver(t, f, d); got.Recipients[0].SuppressionReason != n.SuppressionDuplicateInteraction {
		t.Fatal(got)
	}
}
