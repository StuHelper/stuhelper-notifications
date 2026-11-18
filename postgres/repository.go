package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"time"
)

//go:embed schema.sql
var Schema string

type Repository struct{ DB *sql.DB }

func (r *Repository) db() (*sql.DB, error) {
	if r == nil || r.DB == nil {
		return nil, n.UnavailableError{Reason: n.ReasonDatabaseNil}
	}
	return r.DB, nil
}

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
	var classified n.UnavailableError
	if errors.As(e, &classified) {
		return classified
	}
	var pgErr *pgconn.PgError
	if errors.As(e, &pgErr) && len(pgErr.Code) >= 2 {
		switch pgErr.Code[:2] {
		case "08":
			return n.UnavailableError{Reason: n.ReasonDatabaseConnection}
		case "22", "23":
			return n.UnavailableError{Reason: n.ReasonDatabaseConstraint}
		case "28", "42":
			return n.UnavailableError{Reason: n.ReasonDatabasePrivilegeSchema}
		case "40":
			return n.UnavailableError{Reason: n.ReasonDatabaseSerialization}
		case "53":
			return n.UnavailableError{Reason: n.ReasonDatabaseResources}
		case "55":
			return n.UnavailableError{Reason: n.ReasonDatabaseState}
		case "57":
			return n.UnavailableError{Reason: n.ReasonDatabaseIntervention}
		case "P0":
			return n.UnavailableError{Reason: n.ReasonDatabaseException}
		}
	}
	return n.UnavailableError{Reason: n.ReasonDatabaseError}
}
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return n.UnavailableError{Reason: n.ReasonDatabaseNil}
	}
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
	db, e := r.db()
	if e != nil {
		return e
	}
	var hasUsage bool
	if e = db.QueryRowContext(ctx, `SELECT has_schema_privilege(current_user,'notifications','USAGE')`).Scan(&hasUsage); e != nil {
		return safe(e)
	}
	if !hasUsage {
		return n.UnavailableError{Reason: n.ReasonRuntimeMissingSchema}
	}
	var unsafe bool
	e = db.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'notifications','CREATE') OR has_table_privilege(current_user,'notifications.inbox','UPDATE,DELETE,TRUNCATE') OR has_table_privilege(current_user,'notifications.revisions','DELETE,TRUNCATE') OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='notifications' AND pg_has_role(current_user,nspowner,'USAGE')) OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname='notifications' AND pg_has_role(current_user,c.relowner,'USAGE')) FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil {
		return safe(e)
	}
	if unsafe {
		return n.UnavailableError{Reason: n.ReasonRuntimeUnsafePrivilege}
	}
	for _, table := range []string{"items", "preferences", "inbox", "revisions"} {
		var allowed bool
		if e = db.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'SELECT') AND has_table_privilege(current_user,$1,'INSERT')`, "notifications."+table).Scan(&allowed); e != nil {
			return safe(e)
		}
		if !allowed {
			return n.UnavailableError{Reason: n.ReasonRuntimeMissingReadWrite}
		}
	}
	for _, table := range []string{"items", "preferences", "revisions"} {
		var allowed bool
		if e = db.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'UPDATE')`, "notifications."+table).Scan(&allowed); e != nil {
			return safe(e)
		}
		if !allowed {
			return n.UnavailableError{Reason: n.ReasonRuntimeMissingUpdate}
		}
	}
	for _, table := range []string{"items", "preferences"} {
		var allowed bool
		if e = db.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,$1,'DELETE')`, "notifications."+table).Scan(&allowed); e != nil {
			return safe(e)
		}
		if !allowed {
			return n.UnavailableError{Reason: n.ReasonRuntimeMissingDelete}
		}
	}
	return nil
}

// Locking the recipient preference row serializes aggregation, preference changes,
// and read transitions across different source subjects.
func lock(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	if _, e := tx.ExecContext(ctx, `INSERT INTO notifications.preferences(owner) VALUES($1) ON CONFLICT DO NOTHING`, owner); e != nil {
		return false, e
	}
	var enabled bool
	e := tx.QueryRowContext(ctx, `SELECT interaction_enabled FROM notifications.preferences WHERE owner=$1 FOR UPDATE`, owner).Scan(&enabled)
	return enabled, e
}

var _ n.Repository = (*Repository)(nil)

func (r *Repository) Consume(ctx context.Context, d n.Event, hash string, now time.Time, window time.Duration) (n.Result, error) {
	db, e := r.db()
	if e != nil {
		return n.Result{}, e
	}
	tx, e := db.BeginTx(ctx, nil)
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
		if e = json.Unmarshal(saved, &result); e != nil {
			return n.Result{}, n.UnavailableError{Reason: n.ReasonCorruptStoredJSON}
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
			enabled, err := lock(ctx, tx, recipient)
			if err != nil {
				return n.Result{}, safe(err)
			}
			if d.Type == "interaction" && !enabled {
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
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.inbox(source,event_id,subject,revision,fingerprint,result) VALUES($1,$2,$3,$4,$5,$6)`, d.Source, d.ID, d.Subject, d.Revision, hash, saved); e != nil {
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
	db, e := r.db()
	if e != nil {
		return nil, e
	}
	rows, e := db.QueryContext(ctx, `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,read_at,count FROM notifications.items WHERE owner=$1 AND ($2 OR (created_at,id)<($3,$4::uuid)) ORDER BY created_at DESC,id DESC LIMIT $5`, owner, c.ID == "", c.CreatedAt, emptyUUID(c.ID), limit)
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
			return nil, n.UnavailableError{Reason: n.ReasonCorruptStoredJSON}
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
	db, e := r.db()
	if e != nil {
		return 0, e
	}
	var count int64
	e = db.QueryRowContext(ctx, `SELECT count(*) FROM notifications.items WHERE owner=$1 AND read_at IS NULL`, owner).Scan(&count)
	return count, safe(e)
}
func (r *Repository) mark(ctx context.Context, owner, id string, now time.Time) error {
	db, e := r.db()
	if e != nil {
		return e
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	if _, e = lock(ctx, tx, owner); e != nil {
		return safe(e)
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
	db, e := r.db()
	if e != nil {
		return n.Preferences{}, e
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	defer tx.Rollback()
	enabled, e := lock(ctx, tx, owner)
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	if value != nil {
		enabled = *value
		if _, e = tx.ExecContext(ctx, `UPDATE notifications.preferences SET interaction_enabled=$2 WHERE owner=$1`, owner, enabled); e != nil {
			return n.Preferences{}, safe(e)
		}
	}
	return n.Preferences{InteractionEnabled: enabled}, safe(tx.Commit())
}
func (r *Repository) Cleanup(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, n.ErrInput
	}
	db, e := r.db()
	if e != nil {
		return 0, e
	}
	result, e := db.ExecContext(ctx, `WITH expired AS (SELECT id FROM notifications.items WHERE created_at<$1 ORDER BY created_at,id LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM notifications.items WHERE id IN (SELECT id FROM expired)`, before, limit)
	if e != nil {
		return 0, safe(e)
	}
	count, e := result.RowsAffected()
	return count, safe(e)
}
