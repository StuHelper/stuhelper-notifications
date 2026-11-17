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

// Event is a trusted, complete notification mapping of one source event.
// Source, ID, EventType, Subject and Revision retain the producer's identity.
// The relay validates the CloudEvents envelope and maps only supported results.
type Event struct {
	Source      string
	ID          string
	EventType   string
	Subject     string
	Revision    int64
	Recipients  []string
	Type        string
	Target      Target
	TitleKey    string
	TitleParams map[string]string
}

const (
	SuppressionInteractionDisabled = "interaction_disabled"
	SuppressionStaleRevision       = "stale_revision"
)

type RecipientResult struct {
	Recipient         string `json:"recipient"`
	ID                string `json:"id"`
	SuppressionReason string `json:"suppression_reason"`
}
type Result struct {
	Recipients []RecipientResult `json:"recipients"`
	Ignored    bool              `json:"ignored"`
}
type Consumer interface {
	Consume(context.Context, Event) (Result, error)
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
	Consume(context.Context, Event, string, time.Time, time.Duration) (Result, error)
	List(context.Context, string, Cursor, int) ([]Notification, error)
	Unread(context.Context, string) (int64, error)
	MarkRead(context.Context, string, string, time.Time) error
	MarkAllRead(context.Context, string, time.Time) error
	Preferences(context.Context, string, *bool) (Preferences, error)
	Cleanup(context.Context, time.Time, int) (int64, error)
	MaintenanceStats(context.Context, time.Time) (MaintenanceStats, error)
}

type MaintenanceStats struct {
	ExpiredItems          int64      `json:"expired_items"`
	OldestExpiredAt       *time.Time `json:"oldest_expired_at"`
	EstimatedInboxRows    int64      `json:"estimated_inbox_rows"`
	EstimatedRevisionRows int64      `json:"estimated_revision_rows"`
}

type ConsumeObservation struct {
	Source     string
	Type       string
	TitleKey   string
	Delivered  int
	Suppressed map[string]int
	Ignored    bool
	Err        error
	Duration   time.Duration
}

type ResolveObservation struct {
	Batch        bool
	Targets      int
	Accessible   int
	Inaccessible int
	Unavailable  int
	Duration     time.Duration
}

type CleanupObservation struct {
	Deleted   int64
	FullBatch bool
	Err       error
	Duration  time.Duration
}

type Observer interface {
	ObserveConsume(ConsumeObservation)
	ObserveResolve(ResolveObservation)
	ObserveCleanup(CleanupObservation)
}

type Config struct {
	Retention         time.Duration
	AggregationWindow time.Duration
	CleanupBatch      int
	ResolveTimeout    time.Duration
	Observer          Observer
}

// TargetRef is a comparable key for a page-scoped, deduplicated lookup.
type TargetRef struct {
	Source string
	Target Target
}
type TargetResolution struct {
	Status string
	Name   string
}

// BatchResolver performs current authorization and name lookup through each
// owning module's public batch port. Missing results are unavailable. Resolver
// remains supported for older hosts; new backend assemblies implement both.
type BatchResolver interface {
	ResolveBatch(context.Context, string, []TargetRef) (map[TargetRef]TargetResolution, error)
}
