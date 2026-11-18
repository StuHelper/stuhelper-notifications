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
	return s.repo.Consume(ctx, d, hex.EncodeToString(hash[:]), s.now().UTC(), s.config.AggregationWindow)
}
func (s *Service) List(ctx context.Context, owner, after string, limit int) (Page, error) {
	return s.list(ctx, owner, after, limit, false)
}

// ListWithContentNames opts into the expanded title parameter contract.
func (s *Service) ListWithContentNames(ctx context.Context, owner, after string, limit int) (Page, error) {
	return s.list(ctx, owner, after, limit, true)
}
func (s *Service) list(ctx context.Context, owner, after string, limit int, includeNames bool) (Page, error) {
	if e := ownerOK(owner); e != nil {
		return Page{}, e
	}
	if limit < 1 || limit > 100 || len(after) > 1024 {
		return Page{}, ErrInput
	}
	var cursor Cursor
	if after != "" {
		b, e := base64.RawURLEncoding.DecodeString(after)
		if e != nil {
			return Page{}, ErrInput
		}
		if json.Unmarshal(b, &cursor) != nil || cursor.CreatedAt.IsZero() {
			return Page{}, ErrInput
		}
		normalizedID, e := parseUUID(cursor.ID)
		if e != nil {
			return Page{}, ErrInput
		}
		cursor.ID = normalizedID
	}
	rows, e := s.repo.List(ctx, owner, cursor, limit+1)
	if e != nil {
		return Page{}, e
	}
	out := Page{Items: rows}
	if len(rows) > limit {
		out.Items = rows[:limit]
		last := out.Items[limit-1]
		b, _ := json.Marshal(Cursor{last.CreatedAt, last.ID})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	s.resolvePage(ctx, owner, out.Items, includeNames)
	return out, nil
}
func (s *Service) Unread(ctx context.Context, owner string) (int64, error) {
	if e := ownerOK(owner); e != nil {
		return 0, e
	}
	return s.repo.Unread(ctx, owner)
}
func (s *Service) MarkRead(ctx context.Context, owner, id string) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	normalizedID, e := parseUUID(id)
	if e != nil {
		return ErrInput
	}
	return s.repo.MarkRead(ctx, owner, normalizedID, s.now().UTC())
}
func (s *Service) MarkAllRead(ctx context.Context, owner string) error {
	if e := ownerOK(owner); e != nil {
		return e
	}
	return s.repo.MarkAllRead(ctx, owner, s.now().UTC())
}
func (s *Service) Preferences(ctx context.Context, owner string, enabled *bool) (Preferences, error) {
	if e := ownerOK(owner); e != nil {
		return Preferences{}, e
	}
	return s.repo.Preferences(ctx, owner, enabled)
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
