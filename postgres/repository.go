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

const (
	ListFirstPageQuery = `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,read_at,count FROM notifications.items WHERE owner=$1 ORDER BY created_at DESC,id DESC LIMIT $2`
	ListCursorQuery    = `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,read_at,count FROM notifications.items WHERE owner=$1 AND (created_at,id)<($2,$3::uuid) ORDER BY created_at DESC,id DESC LIMIT $4`
)

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
	e = db.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'notifications','CREATE') OR has_table_privilege(current_user,'notifications.inbox','UPDATE,DELETE,TRUNCATE') OR has_table_privilege(current_user,'notifications.revisions','DELETE,TRUNCATE') OR has_table_privilege(current_user,'notifications.like_subjects','UPDATE,DELETE,TRUNCATE') OR has_table_privilege(current_user,'notifications.closed_owners','UPDATE,DELETE,TRUNCATE') OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='notifications' AND pg_has_role(current_user,nspowner,'USAGE')) OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname='notifications' AND pg_has_role(current_user,c.relowner,'USAGE')) FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil {
		return safe(e)
	}
	if unsafe {
		return n.UnavailableError{Reason: n.ReasonRuntimeUnsafePrivilege}
	}
	for _, table := range []string{"items", "preferences", "inbox", "revisions", "closed_owners", "like_subjects"} {
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
	_ n.AccountCloser             = (*Repository)(nil)
	_ n.Repository                = (*Repository)(nil)
	_ n.ClockedConsumerRepository = (*Repository)(nil)
)

type receiptReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readDeliveryReceipt(ctx context.Context, reader receiptReader, source, id, hash string) (n.Result, bool, error) {
	var old string
	var saved []byte
	var result n.Result
	err := reader.QueryRowContext(ctx, `SELECT fingerprint,result FROM notifications.inbox WHERE source=$1 AND event_id=$2`, source, id).Scan(&old, &saved)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, safe(err)
	}
	if old != hash {
		return result, true, n.ErrConflict
	}
	if len(saved) == 0 {
		return n.Result{Ignored: true}, true, nil
	}
	if err = json.Unmarshal(saved, &result); err != nil {
		return n.Result{}, true, n.UnavailableError{Reason: n.ReasonCorruptStoredJSON}
	}
	return result, true, nil
}

func (r *Repository) ReplayDelivery(ctx context.Context, source, id, hash string) (n.Result, bool, error) {
	db, err := r.db()
	if err != nil {
		return n.Result{}, false, err
	}
	return readDeliveryReceipt(ctx, db, source, id, hash)
}
func (r *Repository) Consume(ctx context.Context, d n.Event, hash string, now time.Time, window time.Duration) (n.Result, error) {
	return r.ConsumeWithClock(ctx, d, hash, func() time.Time { return now }, window)
}

func (r *Repository) ConsumeWithClock(ctx context.Context, d n.Event, hash string, nowFn func() time.Time, window time.Duration) (n.Result, error) {
	if nowFn == nil {
		nowFn = time.Now
	}
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
	result, found, e := readDeliveryReceipt(ctx, tx, d.Source, d.ID, hash)
	if e != nil || found {
		return result, e
	}
	expectedInteractionKey := ""
	if d.Type == "interaction" {
		expectedInteractionKey = d.TitleKey
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.revisions(source,subject,revision,interaction_title_key) VALUES($1,$2,0,$3) ON CONFLICT DO NOTHING`, d.Source, d.Subject, expectedInteractionKey); e != nil {
		return n.Result{}, safe(e)
	}
	var revision int64
	var boundKey string
	if e = tx.QueryRowContext(ctx, `SELECT revision,interaction_title_key FROM notifications.revisions WHERE source=$1 AND subject=$2 FOR UPDATE`, d.Source, d.Subject).Scan(&revision, &boundKey); e != nil {
		return n.Result{}, safe(e)
	}
	if expectedInteractionKey != "" {
		if boundKey == "" {
			if _, e = tx.ExecContext(ctx, `UPDATE notifications.revisions SET interaction_title_key=$3 WHERE source=$1 AND subject=$2`, d.Source, d.Subject, expectedInteractionKey); e != nil {
				return n.Result{}, safe(e)
			}
		} else if boundKey != expectedInteractionKey {
			return n.Result{}, n.ErrInput
		}
	}
	result.Ignored = d.Revision <= revision
	var now time.Time
	if result.Ignored {
		now = nowFn().UTC()
		for _, recipient := range d.Recipients {
			result.Recipients = append(result.Recipients, n.RecipientResult{
				Recipient:         recipient,
				SuppressionReason: n.SuppressionStaleRevision,
			})
		}
	} else {
		// Lock all recipients in sorted order first, then sample the clock so
		// concurrent deliveries never evaluate aggregation with an older pre-lock timestamp.
		closedByRecipient := make([]bool, len(d.Recipients))
		enabledByRecipient := make([]bool, len(d.Recipients))
		for i, recipient := range d.Recipients {
			closed, enabled, err := lockOwner(ctx, tx, recipient)
			if err != nil {
				return n.Result{}, safe(err)
			}
			closedByRecipient[i], enabledByRecipient[i] = closed, enabled
		}
		now = nowFn().UTC()
		for i, recipient := range d.Recipients {
			entry := n.RecipientResult{Recipient: recipient}
			if closedByRecipient[i] {
				entry.SuppressionReason = n.SuppressionAccountClosed
			} else if d.Type == "interaction" && !enabledByRecipient[i] {
				entry.SuppressionReason = n.SuppressionInteractionDisabled
			} else if d.TitleKey == "notifications.interaction.like" {
				ins, err := tx.ExecContext(ctx, `INSERT INTO notifications.like_subjects(owner,source,subject) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, recipient, d.Source, d.Subject)
				if err != nil {
					return n.Result{}, safe(err)
				}
				rows, err := ins.RowsAffected()
				if err != nil {
					return n.Result{}, safe(err)
				}
				if rows == 0 {
					entry.SuppressionReason = n.SuppressionDuplicateInteraction
				}
			}
			entry, e = writeNotification(ctx, tx, d, entry, now, window)
			if e != nil {
				return n.Result{}, e
			}
			result.Recipients = append(result.Recipients, entry)
		}
	}
	if !result.Ignored {
		if _, e = tx.ExecContext(ctx, `UPDATE notifications.revisions SET revision=$3 WHERE source=$1 AND subject=$2`, d.Source, d.Subject, d.Revision); e != nil {
			return n.Result{}, safe(e)
		}
	}
	saved, e := json.Marshal(result)
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
			e = tx.QueryRowContext(ctx, `SELECT id FROM notifications.items WHERE owner=$1 AND type='interaction' AND source_module=$2 AND target_kind=$3 AND target_id=$4 AND title_key=$5 AND title_params=$6 AND created_at>$7 AND read_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`, result.Recipient, d.Source, d.Target.Kind, d.Target.ID, d.TitleKey, params, now.Add(-window)).Scan(&result.ID)
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
	if e := r.ensureOpenOwner(ctx, owner); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 101 || (c.ID == "") != c.CreatedAt.IsZero() {
		return nil, n.ErrInput
	}
	var rows *sql.Rows
	if c.ID == "" {
		rows, e = db.QueryContext(ctx, ListFirstPageQuery, owner, limit)
	} else {
		rows, e = db.QueryContext(ctx, ListCursorQuery, owner, c.CreatedAt, c.ID, limit)
	}
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
func (r *Repository) Unread(ctx context.Context, owner string) (int64, error) {
	db, e := r.db()
	if e != nil {
		return 0, e
	}
	if e := r.ensureOpenOwner(ctx, owner); e != nil {
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
	closed, _, e := lockOwner(ctx, tx, owner)
	if e != nil {
		return safe(e)
	}
	if closed {
		return n.ErrForbidden
	}
	if id == "" {
		_, e = tx.ExecContext(ctx, `UPDATE notifications.items SET read_at=$2 WHERE owner=$1 AND read_at IS NULL`, owner, now)
	} else {
		var exists bool
		e = tx.QueryRowContext(ctx, `WITH updated AS (UPDATE notifications.items SET read_at=$3 WHERE owner=$1 AND id=$2::uuid AND read_at IS NULL)
 SELECT EXISTS(SELECT 1 FROM notifications.items WHERE owner=$1 AND id=$2::uuid)`, owner, id, now).Scan(&exists)
		if e == nil && !exists {
			return n.ErrNotFound
		}
	}
	if e != nil {
		return safe(e)
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
	if value == nil {
		var closed, enabled bool
		e := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1), COALESCE((SELECT interaction_enabled FROM notifications.preferences WHERE owner=$1), true)`, owner).Scan(&closed, &enabled)
		if e != nil {
			return n.Preferences{}, safe(e)
		}
		if closed {
			return n.Preferences{}, n.ErrForbidden
		}
		return n.Preferences{InteractionEnabled: enabled}, nil
	}
	tx, e := db.BeginTx(ctx, nil)
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
	db, e := r.db()
	if e != nil {
		return e
	}
	tx, e := db.BeginTx(ctx, nil)
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
