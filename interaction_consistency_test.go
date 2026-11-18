package notifications_test

import (
	"context"
	"errors"
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
		Retention:         24 * time.Hour,
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
