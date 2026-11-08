package notifications

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInput           = errors.New("invalid_input")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrNotFound        = errors.New("not_found")
	ErrConflict        = errors.New("idempotency_conflict")
	ErrUnavailable     = errors.New("unavailable")
)

type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type Notification struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	SourceModule string            `json:"source_module"`
	Target       Target            `json:"target"`
	TitleKey     string            `json:"title_key"`
	TitleParams  map[string]string `json:"title_params"`
	CreatedAt    time.Time         `json:"created_at"`
	ReadAt       *time.Time        `json:"read_at"`
	TargetStatus string            `json:"target_status"`
	Count        int64             `json:"count"`
}
type Page struct {
	Items      []Notification `json:"items"`
	NextCursor string         `json:"next_cursor"`
}
type Preferences struct {
	InteractionEnabled bool `json:"interaction_enabled"`
}

// Delivery is trusted module input, never accepted directly from an HTTP client.
// Key must identify one immutable business event within SourceModule and Recipient.
type Delivery struct {
	Recipient    string
	Key          string
	Type         string
	SourceModule string
	Target       Target
	TitleKey     string
	TitleParams  map[string]string
}
type Result struct {
	ID         string
	Suppressed bool
}
type Sender interface {
	Deliver(context.Context, Delivery) (Result, error)
}

// Resolver enforces current target authorization for the recipient. Unknown targets
// must return inaccessible; dependency failures return an error.
type Resolver interface {
	Resolve(context.Context, string, string, Target) (string, error)
}
type Cursor struct {
	CreatedAt time.Time
	ID        string
}
type Repository interface {
	Deliver(context.Context, Delivery, string, time.Time, time.Duration) (Result, error)
	List(context.Context, string, Cursor, int) ([]Notification, error)
	Unread(context.Context, string) (int64, error)
	MarkRead(context.Context, string, string, time.Time) error
	MarkAllRead(context.Context, string, time.Time) error
	Preferences(context.Context, string, *bool) (Preferences, error)
	Cleanup(context.Context, time.Time, int) (int64, error)
}
type Config struct {
	Retention         time.Duration
	AggregationWindow time.Duration
	CleanupBatch      int
	ResolveTimeout    time.Duration
}
