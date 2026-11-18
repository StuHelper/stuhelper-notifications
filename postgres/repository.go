package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"time"
)

//go:embed schema.sql
var Schema string

type Repository struct{ DB *sql.DB }

func safe(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, sql.ErrNoRows) {
		return n.ErrNotFound
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	return n.ErrUnavailable
}
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(726391840120)"); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, Schema); e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Repository) CheckRuntime(ctx context.Context) error {
	var unsafe bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'notifications','CREATE') OR has_table_privilege(current_user,'notifications.inbox','UPDATE,DELETE') OR has_table_privilege(current_user,'notifications.revisions','DELETE') FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil || unsafe {
		return n.ErrUnavailable
	}
	for _, table := range []string{"items", "preferences", "inbox", "revisions", "closed_owners"} {
		var allowed bool
		e = r.DB.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'SELECT') AND has_table_privilege(current_user,$1,'INSERT')`, "notifications."+table).Scan(&allowed)
		if e != nil || !allowed {
			return n.ErrUnavailable
		}
	}
	for _, table := range []string{"items", "preferences", "revisions"} {
		var allowed bool
		e = r.DB.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'UPDATE')`, "notifications."+table).Scan(&allowed)
		if e != nil || !allowed {
			return n.ErrUnavailable
		}
	}
	return nil
}

// lockOwner serializes per-recipient mutations (delivery, preference changes,
// read transitions, and account closure) without recreating rows for closed owners.
func lockOwner(ctx context.Context, tx *sql.Tx, owner string) (bool, bool, error) {
	if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "notifications:owner:"+owner); e != nil {
		return false, false, e
	}
	var closed bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1)`, owner).Scan(&closed); e != nil {
		return false, false, e
	}
	if closed {
		return true, false, nil
	}
	var enabled bool
	e := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT interaction_enabled FROM notifications.preferences WHERE owner=$1 FOR UPDATE), true)`, owner).Scan(&enabled)
	return false, enabled, e
}

func (r *Repository) ensureOpenOwner(ctx context.Context, owner string) error {
	var closed bool
	if e := r.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1)`, owner).Scan(&closed); e != nil {
		return safe(e)
	}
	if closed {
		return n.ErrForbidden
	}
	return nil
}

var (
	_ n.Repository    = (*Repository)(nil)
	_ n.AccountCloser = (*Repository)(nil)
)

func (r *Repository) Consume(ctx context.Context, d n.Event, hash string, now time.Time, window time.Duration) (n.Result, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return n.Result{}, safe(e)
	}
	defer tx.Rollback()
	// Serialize the globally unique event identity even if a replay changes subject
	// or recipient. Hash collisions only serialize unrelated events, never dedupe them.
	identity, _ := json.Marshal([]string{d.Source, d.ID})
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", string(identity)); e != nil {
		return n.Result{}, safe(e)
	}
	var old string
	var saved []byte
	var result n.Result
	e = tx.QueryRowContext(ctx, `SELECT fingerprint,result FROM notifications.inbox WHERE source=$1 AND event_id=$2`, d.Source, d.ID).Scan(&old, &saved)
	if e == nil {
		if old != hash {
			return n.Result{}, n.ErrConflict
		}
		if len(saved) == 0 {
			return n.Result{Ignored: true}, nil
		}
		if e = json.Unmarshal(saved, &result); e != nil {
			return n.Result{}, n.ErrUnavailable
		}
		return result, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return n.Result{}, safe(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.revisions(source,subject,revision) VALUES($1,$2,0) ON CONFLICT DO NOTHING`, d.Source, d.Subject); e != nil {
		return n.Result{}, safe(e)
	}
	var revision int64
	if e = tx.QueryRowContext(ctx, `SELECT revision FROM notifications.revisions WHERE source=$1 AND subject=$2 FOR UPDATE`, d.Source, d.Subject).Scan(&revision); e != nil {
		return n.Result{}, safe(e)
	}
	result.Ignored = d.Revision <= revision
	// Service supplies unique recipients in sorted order. Consistent lock ordering
	// prevents deadlocks between different events with overlapping recipient sets.
	for _, recipient := range d.Recipients {
		entry := n.RecipientResult{Recipient: recipient}
		if result.Ignored {
			entry.SuppressionReason = n.SuppressionStaleRevision
		} else {
			closed, enabled, err := lockOwner(ctx, tx, recipient)
			if err != nil {
				return n.Result{}, safe(err)
			}
			if closed {
				entry.SuppressionReason = n.SuppressionAccountClosed
			} else if d.Type == "interaction" && !enabled {
				entry.SuppressionReason = n.SuppressionInteractionDisabled
			}
			entry, e = writeNotification(ctx, tx, d, entry, now, window)
			if e != nil {
				return n.Result{}, e
			}
		}
		result.Recipients = append(result.Recipients, entry)
	}
	if !result.Ignored {
		if _, e = tx.ExecContext(ctx, `UPDATE notifications.revisions SET revision=$3 WHERE source=$1 AND subject=$2`, d.Source, d.Subject, d.Revision); e != nil {
			return n.Result{}, safe(e)
		}
	}
	saved, e = json.Marshal(result)
	if e != nil {
		return n.Result{}, n.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.inbox(source,event_id,subject,revision,fingerprint,result,consumed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, d.Source, d.ID, d.Subject, d.Revision, hash, saved, now); e != nil {
		return n.Result{}, safe(e)
	}
	if e = tx.Commit(); e != nil {
		return n.Result{}, safe(e)
	}
	return result, nil
}

func writeNotification(ctx context.Context, tx *sql.Tx, d n.Event, result n.RecipientResult, now time.Time, window time.Duration) (n.RecipientResult, error) {
	params, e := json.Marshal(d.TitleParams)
	if e != nil {
		return n.RecipientResult{}, n.ErrInput
	}
	if result.SuppressionReason == "" {
		if d.Type == "interaction" {
			e = tx.QueryRowContext(ctx, `SELECT id FROM notifications.items WHERE owner=$1 AND type='interaction' AND source_module=$2 AND target_kind=$3 AND target_id=$4 AND title_key=$5 AND title_params=$6 AND created_at>$7 AND created_at<=$8 AND read_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`, result.Recipient, d.Source, d.Target.Kind, d.Target.ID, d.TitleKey, params, now.Add(-window), now).Scan(&result.ID)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return n.RecipientResult{}, safe(e)
			}
		}
		if result.ID != "" {
			_, e = tx.ExecContext(ctx, `UPDATE notifications.items SET count=count+1 WHERE id=$1`, result.ID)
		} else {
			result.ID = uuid.NewString()
			_, e = tx.ExecContext(ctx, `INSERT INTO notifications.items(id,owner,type,source_module,target_kind,target_id,title_key,title_params,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, result.ID, result.Recipient, d.Type, d.Source, d.Target.Kind, d.Target.ID, d.TitleKey, params, now)
		}
		if e != nil {
			return n.RecipientResult{}, safe(e)
		}
	}
	return result, nil
}
func (r *Repository) List(ctx context.Context, owner string, c n.Cursor, limit int) ([]n.Notification, error) {
	if e := r.ensureOpenOwner(ctx, owner); e != nil {
		return nil, e
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,read_at,count FROM notifications.items WHERE owner=$1 AND ($2 OR (created_at,id)<($3,$4::uuid)) ORDER BY created_at DESC,id DESC LIMIT $5`, owner, c.ID == "", c.CreatedAt, emptyUUID(c.ID), limit)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []n.Notification{}
	for rows.Next() {
		var v n.Notification
		var params []byte
		if e = rows.Scan(&v.ID, &v.Type, &v.SourceModule, &v.Target.Kind, &v.Target.ID, &v.TitleKey, &params, &v.CreatedAt, &v.ReadAt, &v.Count); e != nil {
			return nil, safe(e)
		}
		if e = json.Unmarshal(params, &v.TitleParams); e != nil {
			return nil, n.ErrUnavailable
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func emptyUUID(s string) string {
	if s == "" {
		return uuid.Nil.String()
	}
	return s
}
func (r *Repository) Unread(ctx context.Context, owner string) (int64, error) {
	if e := r.ensureOpenOwner(ctx, owner); e != nil {
		return 0, e
	}
	var count int64
	e := r.DB.QueryRowContext(ctx, `SELECT count(*) FROM notifications.items WHERE owner=$1 AND read_at IS NULL`, owner).Scan(&count)
	return count, safe(e)
}
func (r *Repository) mark(ctx context.Context, owner, id string, now time.Time) error {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	closed, _, e := lockOwner(ctx, tx, owner)
	if e != nil {
		return safe(e)
	}
	if closed {
		return n.ErrForbidden
	}
	result, e := tx.ExecContext(ctx, `UPDATE notifications.items SET read_at=COALESCE(read_at,$3) WHERE owner=$1 AND ($2='' OR id=$4::uuid)`, owner, id, now, emptyUUID(id))
	if e != nil {
		return safe(e)
	}
	affected, e := result.RowsAffected()
	if e != nil {
		return safe(e)
	}
	if id != "" && affected == 0 {
		return n.ErrNotFound
	}
	return safe(tx.Commit())
}
func (r *Repository) MarkRead(ctx context.Context, owner, id string, now time.Time) error {
	return r.mark(ctx, owner, id, now)
}
func (r *Repository) MarkAllRead(ctx context.Context, owner string, now time.Time) error {
	return r.mark(ctx, owner, "", now)
}
func (r *Repository) Preferences(ctx context.Context, owner string, value *bool) (n.Preferences, error) {
	if value == nil {
		var closed, enabled bool
		e := r.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1), COALESCE((SELECT interaction_enabled FROM notifications.preferences WHERE owner=$1), true)`, owner).Scan(&closed, &enabled)
		if e != nil {
			return n.Preferences{}, safe(e)
		}
		if closed {
			return n.Preferences{}, n.ErrForbidden
		}
		return n.Preferences{InteractionEnabled: enabled}, nil
	}
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	defer tx.Rollback()
	closed, _, e := lockOwner(ctx, tx, owner)
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	if closed {
		return n.Preferences{}, n.ErrForbidden
	}
	enabled := *value
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.preferences(owner,interaction_enabled) VALUES($1,$2) ON CONFLICT(owner) DO UPDATE SET interaction_enabled=EXCLUDED.interaction_enabled`, owner, enabled); e != nil {
		return n.Preferences{}, safe(e)
	}
	return n.Preferences{InteractionEnabled: enabled}, safe(tx.Commit())
}
func (r *Repository) CloseAccount(ctx context.Context, owner, closureID string, now time.Time) error {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "notifications:owner:"+owner); e != nil {
		return safe(e)
	}
	var saved string
	e = tx.QueryRowContext(ctx, `SELECT closure_id FROM notifications.closed_owners WHERE owner=$1`, owner).Scan(&saved)
	if e == nil {
		if saved != closureID {
			return n.ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return safe(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.closed_owners(owner,closure_id,closed_at) VALUES($1,$2,$3)`, owner, closureID, now); e != nil {
		return safe(e)
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM notifications.items WHERE owner=$1`, owner); e != nil {
		return safe(e)
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM notifications.preferences WHERE owner=$1`, owner); e != nil {
		return safe(e)
	}
	return safe(tx.Commit())
}
func (r *Repository) Cleanup(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, n.ErrInput
	}
	result, e := r.DB.ExecContext(ctx, `WITH expired AS (SELECT id FROM notifications.items WHERE created_at<$1 ORDER BY created_at,id LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM notifications.items WHERE id IN (SELECT id FROM expired)`, before, limit)
	if e != nil {
		return 0, safe(e)
	}
	count, e := result.RowsAffected()
	return count, safe(e)
}

// CleanupInboxDetails prunes recipient delivery details from completed inbox receipts
// older than before using a dedicated maintenance database connection, while retaining
// (source, event_id, fingerprint) deduplication records.
func CleanupInboxDetails(ctx context.Context, db *sql.DB, before time.Time, limit int) (int64, error) {
	if db == nil {
		return 0, n.ErrUnavailable
	}
	if limit < 1 || limit > 10000 {
		return 0, n.ErrInput
	}
	result, e := db.ExecContext(ctx, `WITH expired AS (SELECT source,event_id FROM notifications.inbox WHERE result IS NOT NULL AND consumed_at<$1 ORDER BY consumed_at,source,event_id LIMIT $2 FOR UPDATE SKIP LOCKED) UPDATE notifications.inbox i SET result=NULL FROM expired e WHERE i.source=e.source AND i.event_id=e.event_id`, before, limit)
	if e != nil {
		return 0, safe(e)
	}
	count, e := result.RowsAffected()
	return count, safe(e)
}
