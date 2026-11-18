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
	if repo == nil || resolver == nil || config.Retention <= 0 || config.AggregationWindow <= 0 || config.AggregationWindow > config.Retention || config.CleanupBatch < 1 || config.CleanupBatch > 10000 || config.ResolveTimeout <= 0 {
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
func (s *Service) Consume(ctx context.Context, d Event) (Result, error) {
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

// ListWithContentNames opts into the expanded title parameter contract.
func (s *Service) ListWithContentNames(ctx context.Context, owner, after string, limit int) (Page, error) {
	return s.ListFiltered(ctx, owner, ListQuery{Cursor: after, Limit: limit, IncludeContentNames: true})
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
			cursor.UpdatedAt = cursor.CreatedAt
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
		u, e := uuid.Parse(cursor.ID)
		if e != nil {
			return Page{}, ErrInput
		}
		cursor.ID = u.String()
	}
	rows, e := s.repo.ListFiltered(ctx, owner, q, cursor, q.Limit+1)
	if e != nil {
		return Page{}, e
	}
	out := Page{Items: rows}
	if len(rows) > q.Limit {
		out.Items = rows[:q.Limit]
		last := out.Items[q.Limit-1]
		b, _ := json.Marshal(Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID, Box: q.Box, Type: q.Type, UnreadOnly: q.UnreadOnly})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	if e = s.resolvePage(ctx, owner, out.Items, q.IncludeContentNames); e != nil {
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
	return s.repo.UnreadSummary(ctx, owner)
}
func (s *Service) MarkRead(ctx context.Context, owner, id string) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	normalized, e := parseUUID(id)
	if e != nil {
		return ErrInput
	}
	return s.repo.MarkRead(ctx, owner, normalized, s.now().UTC())
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
	return s.repo.MarkAllReadFiltered(ctx, owner, filter, s.now().UTC())
}
func (s *Service) Preferences(ctx context.Context, owner string, enabled *bool) (Preferences, error) {
	if e := ownerOK(owner); e != nil {
		return Preferences{}, e
	}
	if enabled == nil {
		return s.repo.Preferences(ctx, owner, nil)
	}
	return s.UpdatePreferences(ctx, owner, PreferencesUpdate{InteractionEnabled: enabled})
}

func (s *Service) UpdatePreferences(ctx context.Context, owner string, patch PreferencesUpdate) (Preferences, error) {
	if e := ownerOK(owner); e != nil {
		return Preferences{}, e
	}
	if patch.InteractionEnabled == nil && patch.ReplyEnabled == nil && patch.LikeEnabled == nil {
		return Preferences{}, ErrInput
	}
	return s.repo.UpdatePreferences(ctx, owner, patch)
}

// Cleanup performs one bounded batch. The host schedules further batches.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	return s.repo.Cleanup(ctx, s.now().UTC().Add(-s.config.Retention), s.config.CleanupBatch)
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
