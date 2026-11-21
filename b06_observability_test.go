package notifications_test

import (
	"context"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu       sync.Mutex
	consumes []n.ConsumeObservation
	resolves []n.ResolveObservation
	cleanups []n.CleanupObservation
}

func (o *recordingObserver) ObserveConsume(obs n.ConsumeObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.consumes = append(o.consumes, obs)
}

func (o *recordingObserver) ObserveResolve(obs n.ResolveObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resolves = append(o.resolves, obs)
}

func (o *recordingObserver) ObserveCleanup(obs n.CleanupObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cleanups = append(o.cleanups, obs)
}

func TestMaintenanceStatsAndOperationalObserver(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	obs := &recordingObserver{}
	emptyStats, err := f.s.MaintenanceStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if emptyStats.ExpiredItems != 0 || emptyStats.OldestExpiredAt != nil || emptyStats.EstimatedInboxRows != 0 || emptyStats.EstimatedRevisionRows != 0 {
		t.Fatalf("unexpected empty stats: %+v", emptyStats)
	}

	// Now create an observed service using the isolated runtime role.
	svc, err := n.New(
		&postgres.Repository{DB: f.runtime},
		f.src,
		n.Config{
			ReadRetention:     24 * time.Hour,
			UnreadRetention:   24 * time.Hour,
			AggregationWindow: time.Hour,
			CleanupBatch:      2,
			ResolveTimeout:    time.Second,
			Observer:          obs,
		},
		func() time.Time { return f.now },
	)
	if err != nil {
		t.Fatal(err)
	}

	t0 := f.now
	for i := range 4 {
		f.now = t0.Add(time.Duration(i) * time.Minute)
		ev := delivery(fmt.Sprintf("exp-%d", i), "business")
		ev.Target.ID = fmt.Sprintf("review-%d", i)
		if _, err = svc.Consume(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	// Preference suppression and stale revision observations (adds 1 item for alice at t0+3m, total 5 expired items).
	off := false
	if _, err = svc.UpdatePreferences(ctx, "bob", n.PreferencesUpdate{ReplyEnabled: &off, LikeEnabled: &off}); err != nil {
		t.Fatal(err)
	}
	batchEv := delivery("mixed-recipients", "interaction")
	batchEv.Recipients = []string{"alice", "bob"}
	batchEv.Subject = "review/obs"
	batchEv.Revision = 2
	if _, err = svc.Consume(ctx, batchEv); err != nil {
		t.Fatal(err)
	}
	staleEv := batchEv
	staleEv.ID = "stale-obs"
	staleEv.Revision = 1
	if _, err = svc.Consume(ctx, staleEv); err != nil {
		t.Fatal(err)
	}
	badEv := batchEv
	badEv.ID = ""
	if _, err = svc.Consume(ctx, badEv); !errors.Is(err, n.ErrInput) {
		t.Fatal(err)
	}

	// Add one fresh notification after 25 hours so 5 items are expired and 1 item remains active.
	f.now = t0.Add(25 * time.Hour)
	if _, err = svc.Consume(ctx, delivery("fresh", "business")); err != nil {
		t.Fatal(err)
	}

	stats, err := svc.MaintenanceStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ExpiredItems != 5 || stats.OldestExpiredAt == nil || !stats.OldestExpiredAt.Equal(t0) {
		t.Fatalf("unexpected backlog stats before ANALYZE: %+v", stats)
	}
	if stats.EstimatedInboxRows != 7 || stats.EstimatedRevisionRows != 6 {
		t.Fatalf("unexpected row estimates before ANALYZE: %+v", stats)
	}

	// Run ANALYZE as migration owner so pg_class.reltuples is populated, then re-query as runtime role.
	if _, err = f.db.Exec(`ANALYZE notifications.inbox; ANALYZE notifications.revisions`); err != nil {
		t.Fatal(err)
	}
	analyzedStats, err := svc.MaintenanceStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if analyzedStats.EstimatedInboxRows != 7 || analyzedStats.EstimatedRevisionRows != 6 {
		t.Fatalf("unexpected row estimates after ANALYZE: %+v", analyzedStats)
	}

	// Observe target resolution across accessible, inaccessible, and unavailable states.
	f.src.state = "accessible"
	if _, err = svc.List(ctx, "alice", "", 10); err != nil {
		t.Fatal(err)
	}
	f.src.state = "inaccessible"
	if _, err = svc.List(ctx, "alice", "", 10); err != nil {
		t.Fatal(err)
	}
	f.src.err = context.DeadlineExceeded
	if _, err = svc.List(ctx, "alice", "", 10); err != nil {
		t.Fatal(err)
	}
	f.src.err = nil

	// Cleanup batches: 2 (full), 2 (full), 1 (partial).
	deleted1, err := svc.Cleanup(ctx)
	if err != nil || deleted1 != 2 {
		t.Fatal(deleted1, err)
	}
	midStats, err := svc.MaintenanceStats(ctx)
	if err != nil || midStats.ExpiredItems != 3 || midStats.OldestExpiredAt == nil || !midStats.OldestExpiredAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("unexpected mid-cleanup stats: %+v, err=%v", midStats, err)
	}
	deleted2, err := svc.Cleanup(ctx)
	if err != nil || deleted2 != 2 {
		t.Fatal(deleted2, err)
	}
	deleted3, err := svc.Cleanup(ctx)
	if err != nil || deleted3 != 1 {
		t.Fatal(deleted3, err)
	}
	doneStats, err := svc.MaintenanceStats(ctx)
	if err != nil || doneStats.ExpiredItems != 0 || doneStats.OldestExpiredAt != nil {
		t.Fatalf("unexpected post-cleanup stats: %+v, err=%v", doneStats, err)
	}

	// Verify recorded observations.
	if len(obs.consumes) != 8 {
		t.Fatalf("expected 8 consume observations, got %d", len(obs.consumes))
	}
	if obs.consumes[4].Delivered != 1 || obs.consumes[4].Suppressed[n.SuppressionInteractionDisabled] != 1 {
		t.Fatalf("unexpected mixed-recipient observation: %+v", obs.consumes[4])
	}
	if !obs.consumes[5].Ignored || obs.consumes[5].Suppressed[n.SuppressionStaleRevision] != 2 {
		t.Fatalf("unexpected stale revision observation: %+v", obs.consumes[5])
	}
	if !errors.Is(obs.consumes[6].Err, n.ErrInput) {
		t.Fatalf("expected ErrInput observation, got %+v", obs.consumes[6])
	}
	if len(obs.resolves) != 3 || obs.resolves[0].Accessible == 0 || obs.resolves[1].Inaccessible == 0 || obs.resolves[2].Unavailable == 0 {
		t.Fatalf("unexpected resolve observations: %+v", obs.resolves)
	}
	if len(obs.cleanups) != 3 || !obs.cleanups[0].FullBatch || obs.cleanups[0].Deleted != 2 || !obs.cleanups[1].FullBatch || obs.cleanups[1].Deleted != 2 || obs.cleanups[2].FullBatch || obs.cleanups[2].Deleted != 1 {
		t.Fatalf("unexpected cleanup observations: %+v", obs.cleanups)
	}
}

func TestResolveObserverCountsEachDistinctTargetOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.db.Exec(`INSERT INTO notifications.items(id,owner,type,source_module,target_kind,target_id,title_key,title_params,created_at)
 SELECT gen_random_uuid(),'duplicate-owner','business','reviews','review','same-id','notifications.content.approved','{}',now() FROM generate_series(1,2)`); err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	svc, err := n.New(&postgres.Repository{DB: f.runtime}, f.src, n.Config{ReadRetention: 24 * time.Hour, UnreadRetention: 24 * time.Hour, AggregationWindow: time.Hour, CleanupBatch: 2, ResolveTimeout: time.Second, Observer: obs}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	page, err := svc.List(ctx, "duplicate-owner", "", 10)
	if err != nil || len(page.Items) != 2 || len(obs.resolves) != 1 {
		t.Fatalf("list: %+v %v, observations %+v", page, err, obs.resolves)
	}
	counts := obs.resolves[0]
	if counts.Targets != 1 || counts.Accessible != 1 || counts.Inaccessible+counts.Unavailable != 0 {
		t.Fatalf("duplicate resolution outcomes: %+v", counts)
	}
}

func TestCatalogPayloadSchemaSynthesisFromXTitleCatalog(t *testing.T) {
	cmd := exec.Command(
		"node",
		"--input-type=module",
		"-e",
		`
import fs from 'node:fs';
import assert from 'node:assert/strict';
import { synthesizeContract } from './scripts/generate.mjs';

const raw = fs.readFileSync('contracts/notifications.openapi.json', 'utf8');
assert.equal(synthesizeContract(raw), raw, 'existing contract must round-trip identically');

const doc = JSON.parse(raw);
for (const k of Object.keys(doc.components.schemas)) {
  if (k.endsWith('TitlePayload')) delete doc.components.schemas[k];
}
doc.components.schemas.TitleKey.enum = [];
doc.components.schemas.Notification.allOf[0].oneOf = [];
doc['x-title-catalog']['notifications.synthetic_demo.completed'] = {
  description: '合成测试通知',
  type: 'business',
  target_kinds: ['synthetic_target'],
  parameters: ['demo_id'],
  read_parameters: ['content_name', 'content_name_state']
};
const out = JSON.parse(synthesizeContract(JSON.stringify(doc)));
assert(out.components.schemas.SyntheticDemoCompletedTitlePayload, 'missing generated TitlePayload');
assert(out.components.schemas.TitleKey.enum.includes('notifications.synthetic_demo.completed'));
assert(out.components.schemas.Target.properties.kind.enum.includes('synthetic_target'));
assert.deepEqual(
  Object.keys(out.components.schemas.SyntheticDemoCompletedTitlePayload.properties.title_params.properties),
  ['demo_id', 'content_name', 'content_name_state']
);
assert.deepEqual(
  out.components.schemas.InteractionReplyTitlePayload.properties.title_params.required,
  ['content_id', 'content_name', 'content_name_state']
);
assert.deepEqual(
  out.components.schemas.SystemNoticeTitlePayload.properties.title_params.required,
  ['reference_id']
);
`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("synthesizeContract test failed: %v\n%s", err, string(out))
	}
}
