package notifications

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInput           = errors.New("invalid_input")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not_found")
	ErrConflict        = errors.New("idempotency_conflict")
	ErrUnavailable     = errors.New("unavailable")
)

const (
	ReasonDatabaseNil             = "db_nil"
	ReasonDatabaseConnection      = "db_connection"
	ReasonDatabaseConstraint      = "db_constraint"
	ReasonDatabasePrivilegeSchema = "db_privilege_or_schema"
	ReasonDatabaseSerialization   = "db_serialization"
	ReasonDatabaseResources       = "db_resources"
	ReasonDatabaseState           = "db_state"
	ReasonDatabaseIntervention    = "db_intervention"
	ReasonDatabaseException       = "db_exception"
	ReasonDatabaseError           = "db_error"
	ReasonCorruptStoredJSON       = "corrupt_stored_json"
	ReasonRuntimeUnsafePrivilege  = "runtime_unsafe_privilege"
	ReasonRuntimeMissingSchema    = "runtime_missing_schema_usage"
	ReasonRuntimeMissingReadWrite = "runtime_missing_select_insert"
	ReasonRuntimeMissingUpdate    = "runtime_missing_update"
	ReasonRuntimeMissingDelete    = "runtime_missing_delete"
	ReasonResolveTimeout          = "resolve_timeout"
	ReasonResolveCanceled         = "resolve_canceled"
	ReasonResolveError            = "resolve_error"
	ReasonResolveInvalidStatus    = "resolve_invalid_status"
	ReasonResolveInvalidName      = "resolve_invalid_name"
)

// UnavailableError wraps ErrUnavailable with a closed, desensitized reason code.
// It never carries SQL queries, parameters, DSNs, or user identifiers.
type UnavailableError struct {
	Reason string
}

func (e UnavailableError) Error() string {
	if e.Reason == "" {
		return ErrUnavailable.Error()
	}
	return ErrUnavailable.Error() + ": " + e.Reason
}

func (e UnavailableError) Unwrap() error {
	return ErrUnavailable
}

// UnavailableReason extracts the desensitized reason code when err wraps UnavailableError.
func UnavailableReason(err error) string {
	var u UnavailableError
	if errors.As(err, &u) {
		return u.Reason
	}
	return ""
}

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
	UpdatedAt    time.Time         `json:"updated_at"`
	ReadAt       *time.Time        `json:"read_at"`
	TargetStatus string            `json:"target_status"`
	Count        int64             `json:"count"`
}
type Page struct {
	Items      []Notification `json:"items"`
	NextCursor string         `json:"next_cursor"`
}

const (
	BoxInteraction  = "interaction"
	BoxReview       = "review"
	BoxVerification = "verification"
	BoxSystem       = "system"
)

type UnreadByBox struct {
	Interaction  int64 `json:"interaction"`
	Review       int64 `json:"review"`
	Verification int64 `json:"verification"`
	System       int64 `json:"system"`
}

type UnreadByType struct {
	Interaction int64 `json:"interaction"`
	Business    int64 `json:"business"`
	Security    int64 `json:"security"`
	Account     int64 `json:"account"`
}

type UnreadSummary struct {
	Count  int64        `json:"count"`
	ByBox  UnreadByBox  `json:"by_box"`
	ByType UnreadByType `json:"by_type"`
}

type ListQuery struct {
	Cursor     string
	Limit      int
	Box        string
	Type       string
	UnreadOnly bool
}

type ReadFilter struct {
	Box  string
	Type string
}

type Preferences struct {
	ReplyEnabled bool `json:"reply_enabled"`
	LikeEnabled  bool `json:"like_enabled"`
}
type PreferencesUpdate struct {
	ReplyEnabled *bool `json:"reply_enabled,omitempty"`
	LikeEnabled  *bool `json:"like_enabled,omitempty"`
}

// Event is a trusted, complete notification mapping of one source event.
// Source, ID, EventType, Subject and Revision retain the producer's identity.
// For interaction events, Subject must identify the independent interaction entity
// (such as a comment ID or a unique target+actor like relation), never a shared target.
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
	SuppressionInteractionDisabled  = "interaction_disabled"
	SuppressionStaleRevision        = "stale_revision"
	SuppressionAccountClosed        = "account_closed"
	SuppressionDuplicateInteraction = "duplicate_interaction"
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
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	ID         string    `json:"ID"`
	Box        string    `json:"box,omitempty"`
	Type       string    `json:"type,omitempty"`
	UnreadOnly bool      `json:"unread_only,omitempty"`
}
type Repository interface {
	Consume(context.Context, Event, string, time.Time, time.Duration) (Result, error)
	List(context.Context, string, Cursor, int, RetentionCutoffs) ([]Notification, error)
	ListFiltered(context.Context, string, ListQuery, Cursor, int, RetentionCutoffs) ([]Notification, error)
	Unread(context.Context, string, time.Time) (int64, error)
	UnreadSummary(context.Context, string, time.Time) (UnreadSummary, error)
	MarkRead(context.Context, string, string, time.Time, RetentionCutoffs) error
	MarkAllRead(context.Context, string, time.Time, time.Time) error
	MarkAllReadFiltered(context.Context, string, ReadFilter, time.Time, time.Time) error
	Delete(context.Context, string, string) error
	Preferences(context.Context, string) (Preferences, error)
	UpdatePreferences(context.Context, string, PreferencesUpdate) (Preferences, error)
	Cleanup(context.Context, RetentionCutoffs, int) (int64, error)
	MaintenanceStats(context.Context, RetentionCutoffs) (MaintenanceStats, error)
}

// AccountCloser permanently removes personal notifications and preferences while
// recording an owner closure fence in the same transaction.
type AccountCloser interface {
	CloseAccount(context.Context, string, string, time.Time) error
}

// ClockedConsumerRepository allows sampling the event timestamp after acquiring
// database locks so concurrent deliveries never observe inverted timestamps.
type ClockedConsumerRepository interface {
	ConsumeWithClock(context.Context, Event, string, func() time.Time, time.Duration) (Result, error)
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

// RetentionCutoffs are the latest timestamps whose items are already due for deletion.
type RetentionCutoffs struct {
	Read   time.Time
	Unread time.Time
}

const (
	DefaultReadRetention   = 180 * 24 * time.Hour
	DefaultUnreadRetention = 365 * 24 * time.Hour
)

type Config struct {
	ReadRetention     time.Duration
	UnreadRetention   time.Duration
	AggregationWindow time.Duration
	CleanupBatch      int
	ResolveTimeout    time.Duration
	// Diagnostics optionally receives desensitized operation and reason codes.
	Diagnostics func(operation, reason string)
	Observer    Observer
}

// TargetRef is a comparable key for a page-scoped, deduplicated lookup.
type TargetRef struct {
	Source string
	Target Target
}
type TargetResolution struct {
	Status    string
	Name      string
	ContextID string
}

// BatchResolver performs current authorization and name lookup through each
// owning module's public batch port. Missing results are unavailable. Resolver
// remains supported for older hosts; new backend assemblies implement both.
type BatchResolver interface {
	ResolveBatch(context.Context, string, []TargetRef) (map[TargetRef]TargetResolution, error)
}
