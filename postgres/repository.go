package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"strings"
	"time"
)

//go:embed schema.sql
var Schema string

const (
	ListFirstPageQuery = `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,updated_at,read_at,count FROM notifications.items WHERE owner=$1 AND (read_at > $2 OR (read_at IS NULL AND created_at > $3)) ORDER BY updated_at DESC,id DESC LIMIT $4`
	ListCursorQuery    = `SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,updated_at,read_at,count FROM notifications.items WHERE owner=$1 AND (updated_at,id)<($2,$3::uuid) AND (read_at > $4 OR (read_at IS NULL AND created_at > $5)) ORDER BY updated_at DESC,id DESC LIMIT $6`
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
	var canDelete bool
	if e = r.DB.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,'notifications.items','DELETE')`).Scan(&canDelete); e != nil || !canDelete {
		return n.ErrUnavailable
	}
	return nil
}

// lockOwner serializes per-recipient mutations (delivery, preference changes,
// read transitions, and account closure) without recreating rows for closed owners.
func lockOwner(ctx context.Context, tx *sql.Tx, owner string) (bool, n.Preferences, error) {
	if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "notifications:owner:"+owner); e != nil {
		return false, n.Preferences{}, e
	}
	var closed bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1)`, owner).Scan(&closed); e != nil {
		return false, n.Preferences{}, e
	}
	if closed {
		return true, n.Preferences{}, nil
	}
	var prefs n.Preferences
	err := tx.QueryRowContext(ctx, `SELECT reply_enabled,like_enabled FROM notifications.preferences WHERE owner=$1 FOR UPDATE`, owner).Scan(&prefs.ReplyEnabled, &prefs.LikeEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, n.Preferences{ReplyEnabled: true, LikeEnabled: true}, nil
	}
	return false, prefs, err
}

func interactionAllowed(titleKey string, prefs n.Preferences) bool {
	switch titleKey {
	case "notifications.interaction.reply":
		return prefs.ReplyEnabled
	case "notifications.interaction.like":
		return prefs.LikeEnabled
	default:
		return false
	}
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
		prefsByRecipient := make([]n.Preferences, len(d.Recipients))
		for i, recipient := range d.Recipients {
			closed, prefs, err := lockOwner(ctx, tx, recipient)
			if err != nil {
				return n.Result{}, safe(err)
			}
			closedByRecipient[i], prefsByRecipient[i] = closed, prefs
		}
		now = nowFn().UTC()
		for i, recipient := range d.Recipients {
			entry := n.RecipientResult{Recipient: recipient}
			if closedByRecipient[i] {
				entry.SuppressionReason = n.SuppressionAccountClosed
			} else if d.Type == "interaction" && !interactionAllowed(d.TitleKey, prefsByRecipient[i]) {
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
			_, e = tx.ExecContext(ctx, `UPDATE notifications.items SET count=count+1,updated_at=GREATEST(updated_at,$2) WHERE id=$1`, result.ID, now)
		} else {
			result.ID = uuid.NewString()
			_, e = tx.ExecContext(ctx, `INSERT INTO notifications.items(id,owner,box,type,source_module,target_kind,target_id,title_key,title_params,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`, result.ID, result.Recipient, n.BoxForTitleKey(d.TitleKey), d.Type, d.Source, d.Target.Kind, d.Target.ID, d.TitleKey, params, now)
		}
		if e != nil {
			return n.RecipientResult{}, safe(e)
		}
	}
	return result, nil
}
func (r *Repository) List(ctx context.Context, owner string, c n.Cursor, limit int, cutoffs n.RetentionCutoffs) ([]n.Notification, error) {
	return r.ListFiltered(ctx, owner, n.ListQuery{}, c, limit, cutoffs)
}
func (r *Repository) ListFiltered(ctx context.Context, owner string, q n.ListQuery, c n.Cursor, limit int, cutoffs n.RetentionCutoffs) ([]n.Notification, error) {
	db, e := r.db()
	if e != nil {
		return nil, e
	}
	if e = r.ensureOpenOwner(ctx, owner); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 101 || (c.ID == "") != c.UpdatedAt.IsZero() {
		return nil, n.ErrInput
	}
	clauses := []string{"owner=$1"}
	args := []any{owner}
	if q.Box != "" {
		args = append(args, q.Box)
		clauses = append(clauses, fmt.Sprintf("box=$%d", len(args)))
	}
	if q.Type != "" {
		args = append(args, q.Type)
		clauses = append(clauses, fmt.Sprintf("type=$%d", len(args)))
	}
	if q.UnreadOnly {
		clauses = append(clauses, "read_at IS NULL")
	}
	if c.ID != "" {
		args = append(args, c.UpdatedAt, c.ID)
		clauses = append(clauses, fmt.Sprintf("(updated_at,id)<($%d,$%d::uuid)", len(args)-1, len(args)))
	}
	if q.UnreadOnly {
		args = append(args, cutoffs.Unread)
		clauses = append(clauses, fmt.Sprintf("created_at > $%d", len(args)))
	} else {
		args = append(args, cutoffs.Read, cutoffs.Unread)
		clauses = append(clauses, fmt.Sprintf("(read_at > $%d OR (read_at IS NULL AND created_at > $%d))", len(args)-1, len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf("SELECT id,type,source_module,target_kind,target_id,title_key,title_params,created_at,updated_at,read_at,count FROM notifications.items WHERE %s ORDER BY updated_at DESC,id DESC LIMIT $%d", strings.Join(clauses, " AND "), len(args))
	if q.Box == "" && q.Type == "" && !q.UnreadOnly {
		if c.ID == "" {
			query = ListFirstPageQuery
		} else {
			query = ListCursorQuery
		}
	}
	rows, e := db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []n.Notification{}
	for rows.Next() {
		var v n.Notification
		var params []byte
		if e = rows.Scan(&v.ID, &v.Type, &v.SourceModule, &v.Target.Kind, &v.Target.ID, &v.TitleKey, &params, &v.CreatedAt, &v.UpdatedAt, &v.ReadAt, &v.Count); e != nil {
			return nil, safe(e)
		}
		if e = json.Unmarshal(params, &v.TitleParams); e != nil {
			return nil, n.UnavailableError{Reason: n.ReasonCorruptStoredJSON}
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func (r *Repository) Unread(ctx context.Context, owner string, unreadCutoff time.Time) (int64, error) {
	summary, e := r.UnreadSummary(ctx, owner, unreadCutoff)
	return summary.Count, e
}

func (r *Repository) UnreadSummary(ctx context.Context, owner string, unreadCutoff time.Time) (n.UnreadSummary, error) {
	db, e := r.db()
	if e != nil {
		return n.UnreadSummary{}, e
	}
	if e = r.ensureOpenOwner(ctx, owner); e != nil {
		return n.UnreadSummary{}, e
	}
	rows, e := db.QueryContext(ctx, `SELECT box,type,count(*) FROM notifications.items WHERE owner=$1 AND read_at IS NULL AND created_at > $2 GROUP BY box,type`, owner, unreadCutoff)
	if e != nil {
		return n.UnreadSummary{}, safe(e)
	}
	defer rows.Close()
	var out n.UnreadSummary
	for rows.Next() {
		var box, kind string
		var cnt int64
		if e = rows.Scan(&box, &kind, &cnt); e != nil {
			return n.UnreadSummary{}, safe(e)
		}
		out.Count += cnt
		switch box {
		case n.BoxInteraction:
			out.ByBox.Interaction += cnt
		case n.BoxReview:
			out.ByBox.Review += cnt
		case n.BoxVerification:
			out.ByBox.Verification += cnt
		default:
			out.ByBox.System += cnt
		}
		switch kind {
		case "interaction":
			out.ByType.Interaction += cnt
		case "business":
			out.ByType.Business += cnt
		case "security":
			out.ByType.Security += cnt
		case "account":
			out.ByType.Account += cnt
		}
	}
	return out, safe(rows.Err())
}
func (r *Repository) MarkRead(ctx context.Context, owner, id string, now time.Time, cutoffs n.RetentionCutoffs) error {
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
	var exists bool
	e = tx.QueryRowContext(ctx, `WITH updated AS (
  UPDATE notifications.items SET read_at=$3
  WHERE owner=$1 AND id=$2::uuid AND read_at IS NULL AND created_at > $4
  RETURNING id
 )
 SELECT EXISTS(SELECT 1 FROM updated)
     OR EXISTS(SELECT 1 FROM notifications.items WHERE owner=$1 AND id=$2::uuid AND read_at > $5)`,
		owner, id, now, cutoffs.Unread, cutoffs.Read).Scan(&exists)
	if e == nil && !exists {
		return n.ErrNotFound
	}

	if e != nil {
		return safe(e)
	}
	return safe(tx.Commit())
}
func (r *Repository) MarkAllRead(ctx context.Context, owner string, now, unreadCutoff time.Time) error {
	return r.MarkAllReadFiltered(ctx, owner, n.ReadFilter{}, now, unreadCutoff)
}
func (r *Repository) MarkAllReadFiltered(ctx context.Context, owner string, filter n.ReadFilter, now, unreadCutoff time.Time) error {
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
	if closed {
		return n.ErrForbidden
	}
	if e != nil {
		return safe(e)
	}
	clauses := []string{"owner=$1", "read_at IS NULL", "created_at > $3"}
	args := []any{owner, now, unreadCutoff}
	if filter.Box != "" {
		args = append(args, filter.Box)
		clauses = append(clauses, fmt.Sprintf("box=$%d", len(args)))
	}
	if filter.Type != "" {
		args = append(args, filter.Type)
		clauses = append(clauses, fmt.Sprintf("type=$%d", len(args)))
	}
	query := fmt.Sprintf("UPDATE notifications.items SET read_at=$2 WHERE %s", strings.Join(clauses, " AND "))
	if _, e = tx.ExecContext(ctx, query, args...); e != nil {
		return safe(e)
	}
	return safe(tx.Commit())
}
func (r *Repository) Delete(ctx context.Context, owner, id string) error {
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
	if closed {
		return n.ErrForbidden
	}
	if e != nil {
		return safe(e)
	}
	result, e := tx.ExecContext(ctx, `DELETE FROM notifications.items WHERE owner=$1 AND id=$2::uuid`, owner, id)
	if e != nil {
		return safe(e)
	}
	affected, e := result.RowsAffected()
	if e != nil {
		return safe(e)
	}
	if affected == 0 {
		return n.ErrNotFound
	}
	return safe(tx.Commit())
}
func (r *Repository) Preferences(ctx context.Context, owner string) (n.Preferences, error) {
	db, err := r.db()
	if err != nil {
		return n.Preferences{}, err
	}
	var closed bool
	var prefs n.Preferences
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications.closed_owners WHERE owner=$1),
 COALESCE(p.reply_enabled,true),COALESCE(p.like_enabled,true)
 FROM (VALUES(1)) AS v(n) LEFT JOIN notifications.preferences p ON p.owner=$1`, owner).Scan(&closed, &prefs.ReplyEnabled, &prefs.LikeEnabled)
	if err != nil {
		return n.Preferences{}, safe(err)
	}
	if closed {
		return n.Preferences{}, n.ErrForbidden
	}
	return prefs, nil
}
func (r *Repository) UpdatePreferences(ctx context.Context, owner string, patch n.PreferencesUpdate) (n.Preferences, error) {
	if patch.ReplyEnabled == nil && patch.LikeEnabled == nil {
		return n.Preferences{}, n.ErrInput
	}
	db, e := r.db()
	if e != nil {
		return n.Preferences{}, e
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	defer tx.Rollback()
	closed, prefs, e := lockOwner(ctx, tx, owner)
	if closed {
		return n.Preferences{}, n.ErrForbidden
	}
	if e != nil {
		return n.Preferences{}, safe(e)
	}
	if patch.ReplyEnabled != nil {
		prefs.ReplyEnabled = *patch.ReplyEnabled
	}
	if patch.LikeEnabled != nil {
		prefs.LikeEnabled = *patch.LikeEnabled
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO notifications.preferences(owner,reply_enabled,like_enabled) VALUES($1,$2,$3) ON CONFLICT(owner) DO UPDATE SET reply_enabled=EXCLUDED.reply_enabled,like_enabled=EXCLUDED.like_enabled`, owner, prefs.ReplyEnabled, prefs.LikeEnabled); e != nil {
		return n.Preferences{}, safe(e)
	}
	return prefs, safe(tx.Commit())
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
func (r *Repository) Cleanup(ctx context.Context, cutoffs n.RetentionCutoffs, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, n.ErrInput
	}
	db, e := r.db()
	if e != nil {
		return 0, e
	}
	// Take bounded candidates from each expiry index, then delete no more than
	// limit in deadline order. Time minus cutoff compares how overdue each item is.
	result, e := db.ExecContext(ctx, `WITH expired_read AS (
  SELECT id, read_at - $1::timestamptz AS overdue_order FROM notifications.items
  WHERE read_at <= $1 ORDER BY read_at,id LIMIT $3 FOR UPDATE SKIP LOCKED
 ), expired_unread AS (
  SELECT id, created_at - $2::timestamptz AS overdue_order FROM notifications.items
  WHERE read_at IS NULL AND created_at <= $2 ORDER BY created_at,id LIMIT $3 FOR UPDATE SKIP LOCKED
 ), expired AS (
  SELECT id FROM (SELECT * FROM expired_read UNION ALL SELECT * FROM expired_unread) candidates
  ORDER BY overdue_order,id LIMIT $3
 )
 DELETE FROM notifications.items USING expired WHERE items.id=expired.id`, cutoffs.Read, cutoffs.Unread, limit)
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

func (r *Repository) MaintenanceStats(ctx context.Context, cutoffs n.RetentionCutoffs) (n.MaintenanceStats, error) {
	var stats n.MaintenanceStats
	var oldest sql.NullTime
	db, e := r.db()
	if e != nil {
		return n.MaintenanceStats{}, e
	}
	e = db.QueryRowContext(ctx, `SELECT count(*),min(created_at) FROM notifications.items
 WHERE read_at <= $1 OR (read_at IS NULL AND created_at <= $2)`, cutoffs.Read, cutoffs.Unread).Scan(&stats.ExpiredItems, &oldest)
	if e != nil {
		return n.MaintenanceStats{}, safe(e)
	}
	if oldest.Valid {
		t := oldest.Time.UTC()
		stats.OldestExpiredAt = &t
	}
	inboxEst, e := r.estimateTableRows(ctx, "inbox")
	if e != nil {
		return n.MaintenanceStats{}, e
	}
	revEst, e := r.estimateTableRows(ctx, "revisions")
	if e != nil {
		return n.MaintenanceStats{}, e
	}
	stats.EstimatedInboxRows = inboxEst
	stats.EstimatedRevisionRows = revEst
	return stats, nil
}

func (r *Repository) estimateTableRows(ctx context.Context, table string) (int64, error) {
	var estimate float64
	e := r.DB.QueryRowContext(ctx, `SELECT c.reltuples FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='notifications' AND c.relname=$1`, table).Scan(&estimate)
	if e != nil {
		return 0, safe(e)
	}
	if estimate >= 0 {
		return int64(estimate), nil
	}
	var bounded int64
	switch table {
	case "inbox":
		e = r.DB.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM notifications.inbox LIMIT 100000) s`).Scan(&bounded)
	case "revisions":
		e = r.DB.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM notifications.revisions LIMIT 100000) s`).Scan(&bounded)
	default:
		return 0, n.ErrInput
	}
	return bounded, safe(e)
}
