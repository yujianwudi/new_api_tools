package toolstore

import (
	"encoding/json"
	"time"
)

type OperationStatus string

const (
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationDenied    OperationStatus = "denied"
	OperationCancelled OperationStatus = "cancelled"
)

type OperationAudit struct {
	ID             int64
	RequestID      string
	Actor          string
	SourceIP       string
	AuthMethod     string
	Action         string
	TargetType     string
	TargetID       string
	Reason         string
	BeforeJSON     json.RawMessage
	AfterJSON      json.RawMessage
	Status         OperationStatus
	ErrorCode      string
	IdempotencyKey string
	OccurredAt     time.Time
	CreatedAt      time.Time
}

type OperationAuditInput struct {
	RequestID      string
	Actor          string
	SourceIP       string
	AuthMethod     string
	Action         string
	TargetType     string
	TargetID       string
	Reason         string
	BeforeJSON     json.RawMessage
	AfterJSON      json.RawMessage
	Status         OperationStatus
	ErrorCode      string
	IdempotencyKey string
	OccurredAt     time.Time
}

type OperationAuditFilter struct {
	RequestID  string
	Actor      string
	Action     string
	TargetType string
	TargetID   string
	Status     OperationStatus
	BeforeID   int64
	Limit      int
}

type OperationAuditPage struct {
	Items      []OperationAudit
	NextCursor int64
	HasMore    bool
}

type RiskSeverity string

const (
	RiskSeverityLow      RiskSeverity = "low"
	RiskSeverityMedium   RiskSeverity = "medium"
	RiskSeverityHigh     RiskSeverity = "high"
	RiskSeverityCritical RiskSeverity = "critical"
)

type RiskCaseStatus string

const (
	RiskCaseOpen          RiskCaseStatus = "open"
	RiskCaseInvestigating RiskCaseStatus = "investigating"
	RiskCaseMitigated     RiskCaseStatus = "mitigated"
	RiskCaseClosed        RiskCaseStatus = "closed"
)

type RiskCase struct {
	ID          int64          `json:"id"`
	CaseKey     string         `json:"case_key"`
	Title       string         `json:"title"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Severity    RiskSeverity   `json:"severity"`
	Status      RiskCaseStatus `json:"status"`
	Assignee    string         `json:"assignee"`
	Summary     string         `json:"summary"`
	OpenedAt    time.Time      `json:"opened_at"`
	ClosedAt    *time.Time     `json:"closed_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type RiskCaseInput struct {
	CaseKey     string
	Title       string
	SubjectType string
	SubjectID   string
	Severity    RiskSeverity
	Status      RiskCaseStatus
	Assignee    string
	Summary     string
	OpenedAt    time.Time
	ClosedAt    *time.Time
}

type RiskCaseUpdate struct {
	ID       int64
	Title    string
	Severity RiskSeverity
	Status   RiskCaseStatus
	Assignee string
	Summary  string
	ClosedAt *time.Time
}

type RiskCaseFilter struct {
	SubjectType string
	SubjectID   string
	Severity    RiskSeverity
	Status      RiskCaseStatus
	Assignee    string
	BeforeID    int64
	Limit       int
}

type RiskCasePage struct {
	Items      []RiskCase
	NextCursor int64
	HasMore    bool
}

type RiskCaseEvent struct {
	ID             int64           `json:"id"`
	CaseID         int64           `json:"case_id"`
	EventType      string          `json:"event_type"`
	Actor          string          `json:"actor"`
	DetailsJSON    json.RawMessage `json:"details"`
	IdempotencyKey string          `json:"-"`
	OccurredAt     time.Time       `json:"occurred_at"`
	CreatedAt      time.Time       `json:"created_at"`
}

type RiskCaseEventInput struct {
	CaseID         int64
	EventType      string
	Actor          string
	DetailsJSON    json.RawMessage
	IdempotencyKey string
	OccurredAt     time.Time
}

type RiskCaseEventFilter struct {
	CaseID    int64
	EventType string
	BeforeID  int64
	Limit     int
}

type RiskCaseEventPage struct {
	Items      []RiskCaseEvent
	NextCursor int64
	HasMore    bool
}

type NoteVisibility string

const (
	NoteInternal NoteVisibility = "internal"
	NoteCustomer NoteVisibility = "customer"
)

type SupportNote struct {
	ID             int64          `json:"id"`
	SubjectType    string         `json:"subject_type"`
	SubjectID      string         `json:"subject_id"`
	Author         string         `json:"author"`
	Body           string         `json:"body"`
	Visibility     NoteVisibility `json:"visibility"`
	IdempotencyKey string         `json:"idempotency_key"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      *time.Time     `json:"deleted_at"`
}

type SupportNoteInput struct {
	SubjectType    string
	SubjectID      string
	Author         string
	Body           string
	Visibility     NoteVisibility
	IdempotencyKey string
}

type SupportNoteUpdate struct {
	ID         int64
	Body       string
	Visibility NoteVisibility
}

type SupportNoteFilter struct {
	SubjectType    string
	SubjectID      string
	Author         string
	Visibility     NoteVisibility
	IncludeDeleted bool
	BeforeID       int64
	Limit          int
}

type SupportNotePage struct {
	Items      []SupportNote
	NextCursor int64
	HasMore    bool
}

// PriceSnapshot stores both the source decimal and its exact scaled integer.
// No float64 enters this API. For example, 0.000001 is amount_minor=1 with
// minor_unit_scale=6.
type PriceSnapshot struct {
	ID             int64           `json:"id"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Operation      string          `json:"operation"`
	Component      string          `json:"component"`
	Currency       string          `json:"currency"`
	Unit           string          `json:"unit"`
	UnitSize       int64           `json:"unit_size"`
	AmountDecimal  string          `json:"amount_decimal"`
	AmountMinor    int64           `json:"amount_minor"`
	MinorUnitScale int             `json:"minor_unit_scale"`
	Source         string          `json:"source"`
	MetadataJSON   json.RawMessage `json:"metadata"`
	IdempotencyKey string          `json:"idempotency_key"`
	EffectiveAt    time.Time       `json:"effective_at"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	CreatedAt      time.Time       `json:"created_at"`
}

type PriceSnapshotInput struct {
	Provider       string
	Model          string
	Operation      string
	Component      string
	Currency       string
	Unit           string
	UnitSize       int64
	AmountDecimal  string
	AmountMinor    int64
	MinorUnitScale int
	Source         string
	MetadataJSON   json.RawMessage
	IdempotencyKey string
	EffectiveAt    time.Time
	ExpiresAt      *time.Time
}

type PriceSnapshotFilter struct {
	Provider  string
	Model     string
	Operation string
	Component string
	Currency  string
	ActiveAt  *time.Time
	BeforeID  int64
	Limit     int
}

type PriceSnapshotPage struct {
	Items      []PriceSnapshot
	NextCursor int64
	HasMore    bool
}

type ReconciliationStatus string

const (
	ReconciliationRunning   ReconciliationStatus = "running"
	ReconciliationSucceeded ReconciliationStatus = "succeeded"
	ReconciliationFailed    ReconciliationStatus = "failed"
	ReconciliationCancelled ReconciliationStatus = "cancelled"
)

type ReconciliationRun struct {
	ID               int64
	RunKey           string
	Kind             string
	Status           ReconciliationStatus
	WindowStart      time.Time
	WindowEnd        time.Time
	StartedAt        time.Time
	FinishedAt       *time.Time
	ScannedCount     int64
	MatchedCount     int64
	DiscrepancyCount int64
	DiscrepancyMinor int64
	Currency         string
	SummaryJSON      json.RawMessage
	ErrorCode        string
	ErrorMessage     string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ReconciliationRunInput struct {
	RunKey           string
	Kind             string
	Status           ReconciliationStatus
	WindowStart      time.Time
	WindowEnd        time.Time
	StartedAt        time.Time
	FinishedAt       *time.Time
	ScannedCount     int64
	MatchedCount     int64
	DiscrepancyCount int64
	DiscrepancyMinor int64
	Currency         string
	SummaryJSON      json.RawMessage
	ErrorCode        string
	ErrorMessage     string
}

type ReconciliationRunUpdate struct {
	ID               int64
	Status           ReconciliationStatus
	FinishedAt       *time.Time
	ScannedCount     int64
	MatchedCount     int64
	DiscrepancyCount int64
	DiscrepancyMinor int64
	SummaryJSON      json.RawMessage
	ErrorCode        string
	ErrorMessage     string
}

type ReconciliationRunFilter struct {
	Kind     string
	Status   ReconciliationStatus
	BeforeID int64
	Limit    int
}

type ReconciliationRunPage struct {
	Items      []ReconciliationRun
	NextCursor int64
	HasMore    bool
}
