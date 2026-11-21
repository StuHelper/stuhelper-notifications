package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var _ Consumer = (*Service)(nil)

type Service struct {
	repo     Repository
	resolver Resolver
	config   Config
	now      func() time.Time
}

func New(repo Repository, resolver Resolver, config Config, now func() time.Time) (*Service, error) {
	if repo == nil || resolver == nil || config.ReadRetention <= 0 || config.ReadRetention > DefaultReadRetention ||
		config.UnreadRetention <= 0 || config.UnreadRetention > DefaultUnreadRetention ||
		config.AggregationWindow <= 0 || config.AggregationWindow > config.UnreadRetention || config.CleanupBatch < 1 || config.CleanupBatch > 10000 || config.ResolveTimeout <= 0 {
		return nil, ErrInput
	}
	if now == nil {
		now = time.Now
	}
	return &Service{repo, resolver, config, now}, nil
}
func valid(s string, max int) bool {
	return s != "" && len(s) <= max && strings.TrimSpace(s) == s && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
func parseUUID(s string) (string, error) {
	if len(s) != 36 {
		return "", ErrInput
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return "", ErrInput
	}
	return u.String(), nil
}
func ownerOK(owner string) error {
	if owner == "" {
		return ErrUnauthenticated
	}
	if !valid(owner, 256) {
		return ErrInput
	}
	return nil
}
func (s *Service) Consume(ctx context.Context, d Event) (res Result, retErr error) {
	started := time.Now()
	defer func() {
		if s.config.Observer == nil {
			return
		}
		suppressed := map[string]int{}
		delivered := 0
		for _, entry := range res.Recipients {
			if entry.ID != "" {
				delivered++
			}
			if entry.SuppressionReason != "" {
				suppressed[entry.SuppressionReason]++
			}
		}
		s.config.Observer.ObserveConsume(ConsumeObservation{
			Source:     d.Source,
			Type:       d.Type,
			TitleKey:   d.TitleKey,
			Delivered:  delivered,
			Suppressed: suppressed,
			Ignored:    res.Ignored,
			Err:        retErr,
			Duration:   time.Since(started),
		})
	}()
	if d.Revision < 1 || !valid(d.Subject, 512) || !valid(d.EventType, 128) || !valid(d.ID, 256) || !valid(d.Source, 80) || !valid(d.Target.Kind, 80) || !valid(d.Target.ID, 256) || !valid(d.TitleKey, 256) {
		return Result{}, ErrInput
	}
	// Bound the submitted list before sorting and deduplicating.
	if len(d.Recipients) < 1 || len(d.Recipients) > 20 {
		return Result{}, ErrInput
	}
	recipients := slices.Clone(d.Recipients)
	for _, recipient := range recipients {
		if !valid(recipient, 256) {
			return Result{}, ErrInput
		}
	}
	slices.Sort(recipients)
	d.Recipients = slices.Compact(recipients)
	if err := validateTitle(d); err != nil {
		return Result{}, err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Result{}, ErrInput
	}
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	if d.TitleKey == "notifications.interaction.like" && (d.Subject == d.Target.ID || d.Subject == d.Target.Kind+"/"+d.Target.ID) {
		// A pre-upgrade writer may already have committed this identity.
		if reader, ok := s.repo.(interface {
			ReplayDelivery(context.Context, string, string, string) (Result, bool, error)
		}); ok {
			if saved, found, err := reader.ReplayDelivery(ctx, d.Source, d.ID, digest); found || err != nil {
				return saved, err
			}
		}
		return Result{}, ErrInput
	}
	if clocked, ok := s.repo.(ClockedConsumerRepository); ok {
		return clocked.ConsumeWithClock(ctx, d, digest, func() time.Time { return s.now().UTC() }, s.config.AggregationWindow)
	}
	return s.repo.Consume(ctx, d, digest, s.now().UTC(), s.config.AggregationWindow)
}
func (s *Service) List(ctx context.Context, owner, after string, limit int) (Page, error) {
	return s.ListFiltered(ctx, owner, ListQuery{Cursor: after, Limit: limit})
}

func (s *Service) ListFiltered(ctx context.Context, owner string, q ListQuery) (Page, error) {
	if e := ownerOK(owner); e != nil {
		return Page{}, e
	}
	if q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 1024 {
		return Page{}, ErrInput
	}
	if q.Box != "" && !validBox(q.Box) {
		return Page{}, ErrInput
	}
	if q.Type != "" && !validNotificationType(q.Type) {
		return Page{}, ErrInput
	}
	var cursor Cursor
	if q.Cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil {
			return Page{}, ErrInput
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if dec.Decode(&cursor) != nil {
			return Page{}, ErrInput
		}
		if cursor.UpdatedAt.IsZero() {
			return Page{}, ErrInput
		}
		id, e := parseUUID(cursor.ID)
		if e != nil {
			return Page{}, ErrInput
		}
		cursor.ID = id
		if cursor.Box != q.Box || cursor.Type != q.Type || cursor.UnreadOnly != q.UnreadOnly {
			return Page{}, ErrInput
		}
	}
	rows, e := s.repo.ListFiltered(ctx, owner, q, cursor, q.Limit+1, s.retentionCutoffs(s.now().UTC()))
	if e != nil {
		return Page{}, e
	}
	out := Page{Items: rows}
	if len(rows) > q.Limit {
		out.Items = rows[:q.Limit]
		last := out.Items[q.Limit-1]
		b, _ := json.Marshal(Cursor{
			UpdatedAt: last.UpdatedAt, ID: last.ID, Box: q.Box, Type: q.Type,
			UnreadOnly: q.UnreadOnly,
		})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	if e = s.resolvePage(ctx, owner, out.Items); e != nil {
		return Page{}, e
	}
	return out, nil
}
func (s *Service) Unread(ctx context.Context, owner string) (int64, error) {
	summary, err := s.UnreadSummary(ctx, owner)
	if err != nil {
		return 0, err
	}
	return summary.Count, nil
}
func (s *Service) UnreadSummary(ctx context.Context, owner string) (UnreadSummary, error) {
	if e := ownerOK(owner); e != nil {
		return UnreadSummary{}, e
	}
	return s.repo.UnreadSummary(ctx, owner, s.now().UTC().Add(-s.config.UnreadRetention))
}
func (s *Service) MarkRead(ctx context.Context, owner, id string) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	normalized, e := parseUUID(id)
	if e != nil {
		return ErrInput
	}
	now := s.now().UTC()
	return s.repo.MarkRead(ctx, owner, normalized, now, s.retentionCutoffs(now))
}
func (s *Service) MarkAllRead(ctx context.Context, owner string) error {
	return s.MarkAllReadFiltered(ctx, owner, ReadFilter{})
}
func (s *Service) MarkAllReadFiltered(ctx context.Context, owner string, filter ReadFilter) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	if filter.Box != "" && !validBox(filter.Box) {
		return ErrInput
	}
	if filter.Type != "" && !validNotificationType(filter.Type) {
		return ErrInput
	}
	now := s.now().UTC()
	return s.repo.MarkAllReadFiltered(ctx, owner, filter, now, now.Add(-s.config.UnreadRetention))
}
func (s *Service) Delete(ctx context.Context, owner, id string) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	normalized, err := parseUUID(id)
	if err != nil {
		return ErrInput
	}
	return s.repo.Delete(ctx, owner, normalized)
}
func (s *Service) Preferences(ctx context.Context, owner string) (Preferences, error) {
	if e := ownerOK(owner); e != nil {
		return Preferences{}, e
	}
	return s.repo.Preferences(ctx, owner)
}

func (s *Service) UpdatePreferences(ctx context.Context, owner string, patch PreferencesUpdate) (Preferences, error) {
	if e := ownerOK(owner); e != nil {
		return Preferences{}, e
	}
	if patch.ReplyEnabled == nil && patch.LikeEnabled == nil {
		return Preferences{}, ErrInput
	}
	return s.repo.UpdatePreferences(ctx, owner, patch)
}

func (s *Service) retentionCutoffs(now time.Time) RetentionCutoffs {
	return RetentionCutoffs{Read: now.Add(-s.config.ReadRetention), Unread: now.Add(-s.config.UnreadRetention)}
}

// Cleanup performs one bounded batch. The host schedules further batches.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	started := time.Now()
	deleted, err := s.repo.Cleanup(ctx, s.retentionCutoffs(s.now().UTC()), s.config.CleanupBatch)
	if s.config.Observer != nil {
		s.config.Observer.ObserveCleanup(CleanupObservation{
			Deleted:   deleted,
			FullBatch: err == nil && deleted == int64(s.config.CleanupBatch),
			Err:       err,
			Duration:  time.Since(started),
		})
	}
	return deleted, err
}

// MaintenanceStats reports PII-free retention backlog and deduplication table growth.
func (s *Service) MaintenanceStats(ctx context.Context) (MaintenanceStats, error) {
	return s.repo.MaintenanceStats(ctx, s.retentionCutoffs(s.now().UTC()))
}

// CloseAccount permanently deletes personal notifications and preferences for owner
// and installs a durable write fence against delayed event delivery or access.
func (s *Service) CloseAccount(ctx context.Context, owner, closureID string) error {
	if !valid(owner, 256) || !valid(closureID, 256) {
		return ErrInput
	}
	closer, ok := s.repo.(AccountCloser)
	if !ok {
		return ErrUnavailable
	}
	return closer.CloseAccount(ctx, owner, closureID, s.now().UTC())
}

// CloseOwner is an alias for CloseAccount matching owner-key closure callers.
func (s *Service) CloseOwner(ctx context.Context, owner, closureID string) error {
	return s.CloseAccount(ctx, owner, closureID)
}
