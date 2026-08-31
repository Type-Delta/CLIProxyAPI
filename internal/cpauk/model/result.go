package model

import (
	"cmp"
	"slices"
	"time"
)

type AnalyticsState string

const (
	StateDisabled    AnalyticsState = "disabled"
	StateStarting    AnalyticsState = "starting"
	StateReady       AnalyticsState = "ready"
	StateDegraded    AnalyticsState = "degraded"
	StateCircuitOpen AnalyticsState = "circuit_open"
	StateStopping    AnalyticsState = "stopping"
)

func (s AnalyticsState) Valid() bool {
	switch s {
	case StateDisabled, StateStarting, StateReady, StateDegraded, StateCircuitOpen, StateStopping:
		return true
	default:
		return false
	}
}

type ErrorCode string

const (
	ErrorAnalyticsDisabled       ErrorCode = "analytics_disabled"
	ErrorAnalyticsUnavailable    ErrorCode = "analytics_unavailable"
	ErrorAnalyticsMaintenance    ErrorCode = "analytics_maintenance"
	ErrorAnalyticsInvalidQuery   ErrorCode = "analytics_invalid_query"
	ErrorAnalyticsExportTooLarge ErrorCode = "analytics_export_too_large"
	ErrorAnalyticsThrottled      ErrorCode = "analytics_throttled"
	ErrorAnalyticsInternal       ErrorCode = "analytics_internal"
	ErrorAnalyticsBackupInvalid  ErrorCode = "analytics_backup_invalid"
	ErrorStructuredKeysRequired  ErrorCode = "structured_api_keys_required"
)

type ErrorDetail struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason"`
}

type ErrorBody struct {
	Code      ErrorCode     `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id,omitempty"`
	Details   []ErrorDetail `json:"details,omitempty"`
}

type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type QueueSnapshot struct {
	Capacity int64 `json:"capacity"`
	Depth    int64 `json:"depth"`
	Dropped  int64 `json:"dropped"`
	MaxBytes int64 `json:"max_bytes"`
}

type Capabilities struct {
	APISchemaVersions   []int          `json:"api_schema_versions"`
	EventSchemaVersion  int            `json:"event_schema_version"`
	Supported           bool           `json:"supported"`
	Enabled             bool           `json:"enabled"`
	Available           bool           `json:"available"`
	Degraded            bool           `json:"degraded"`
	State               AnalyticsState `json:"state"`
	StorageDriver       string         `json:"storage_driver"`
	StorageScope        string         `json:"storage_scope"`
	KeyIDAlgorithm      string         `json:"key_id_algorithm"`
	StructuredKeys      bool           `json:"structured_keys"`
	SharedEnforcement   bool           `json:"shared_enforcement"`
	ManagementQueryV1   bool           `json:"management_query_v1"`
	ViewerV1            bool           `json:"viewer_v1"`
	Queue               QueueSnapshot  `json:"queue"`
	LastSuccessfulWrite *time.Time     `json:"last_successful_write_at"`
}

type Health struct {
	State                AnalyticsState `json:"state"`
	Category             string         `json:"category,omitempty"`
	Message              string         `json:"message,omitempty"`
	Queue                QueueSnapshot  `json:"queue"`
	LastSuccessfulWrite  *time.Time     `json:"last_successful_write_at"`
	LastPanicCategory    string         `json:"last_panic_category,omitempty"`
	LastPanicAt          *time.Time     `json:"last_panic_at,omitempty"`
	RestartCount         int            `json:"restart_count"`
	RestartWindowSeconds int            `json:"restart_window_seconds"`
	RejectedEvents       int64          `json:"rejected_events"`
	TruncatedFields      int64          `json:"truncated_fields"`
	AbandonedEvents      int64          `json:"abandoned_events"`
	RetentionCutoff      *time.Time     `json:"retention_cutoff,omitempty"`
}

type Range struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	TimeZone string    `json:"time_zone"`
}

type ResponseMeta struct {
	SchemaVersion         int        `json:"schema_version"`
	Range                 Range      `json:"range"`
	Degraded              bool       `json:"degraded"`
	DroppedEvents         int64      `json:"dropped_events"`
	LastSuccessfulWriteAt *time.Time `json:"last_successful_write_at"`
	NextCursor            string     `json:"next_cursor,omitempty"`
}

type Summary struct {
	Meta             ResponseMeta `json:"meta"`
	ProxyRequests    int64        `json:"proxy_requests"`
	UpstreamAttempts int64        `json:"upstream_attempts"`
	Tokens           TokenUsage   `json:"tokens"`
	KnownCost        NanoUSD      `json:"known_cost_usd"`
	UnpricedTokens   int64        `json:"unpriced_tokens"`
}

type TimeseriesPoint struct {
	Start            time.Time  `json:"start"`
	End              time.Time  `json:"end"`
	ProxyRequests    int64      `json:"proxy_requests"`
	UpstreamAttempts int64      `json:"upstream_attempts"`
	Tokens           TokenUsage `json:"tokens"`
	KnownCost        NanoUSD    `json:"known_cost_usd"`
	UnpricedTokens   int64      `json:"unpriced_tokens"`
}

type Timeseries struct {
	Meta   ResponseMeta      `json:"meta"`
	Points []TimeseriesPoint `json:"points"`
}

type DimensionRow struct {
	Value            string     `json:"value"`
	ProxyRequests    int64      `json:"proxy_requests"`
	UpstreamAttempts int64      `json:"upstream_attempts"`
	Tokens           TokenUsage `json:"tokens"`
	KnownCost        NanoUSD    `json:"known_cost_usd"`
	UnpricedTokens   int64      `json:"unpriced_tokens"`
}

type DimensionPage struct {
	Meta      ResponseMeta   `json:"meta"`
	Dimension string         `json:"dimension"`
	Rows      []DimensionRow `json:"rows"`
}

type EventPage struct {
	Meta   ResponseMeta `json:"meta"`
	Events []Event      `json:"events"`
}

type KeyStatus string

const (
	KeyStatusConfigured KeyStatus = "configured"
	KeyStatusRotated    KeyStatus = "rotated"
	KeyStatusDeleted    KeyStatus = "deleted"
	KeyStatusHistorical KeyStatus = "historical"
	KeyStatusConflict   KeyStatus = "identity_conflict"
)

type KeyIdentity struct {
	KeyID           string     `json:"key_id"`
	ShortKeyID      string     `json:"short_key_id"`
	Label           string     `json:"label,omitempty"`
	Status          KeyStatus  `json:"status"`
	ConfigIndexes   []int      `json:"config_indexes,omitempty"`
	FirstActivityAt *time.Time `json:"first_activity_at"`
	LastActivityAt  *time.Time `json:"last_activity_at"`
	TotalTokens     int64      `json:"total_tokens"`
	KnownCost       NanoUSD    `json:"known_cost_usd"`
	UnpricedTokens  int64      `json:"unpriced_tokens"`
}

type LimitWindow string

const (
	LimitWindowHour     LimitWindow = "hour"
	LimitWindowDay      LimitWindow = "day"
	LimitWindowWeek     LimitWindow = "week"
	LimitWindowMonth    LimitWindow = "month"
	LimitWindowLifetime LimitWindow = "lifetime"
)

type KeyLimit struct {
	KeyID            string      `json:"key_id"`
	RequestLimit     *int64      `json:"request_limit"`
	TokenLimit       *int64      `json:"token_limit"`
	Window           LimitWindow `json:"window"`
	RequestsConsumed int64       `json:"requests_consumed"`
	TokensConsumed   int64       `json:"tokens_consumed"`
	NextResetAt      *time.Time  `json:"next_reset_at"`
	ConfigIndex      int         `json:"config_index"`
	ConfigRevision   string      `json:"config_revision"`
}

type LeaderboardRow struct {
	Rank             int        `json:"rank"`
	KeyID            string     `json:"key_id"`
	ShortKeyID       string     `json:"short_key_id"`
	Label            string     `json:"label,omitempty"`
	ProxyRequests    int64      `json:"proxy_requests"`
	UpstreamAttempts int64      `json:"upstream_attempts"`
	Tokens           TokenUsage `json:"tokens"`
	KnownCost        NanoUSD    `json:"known_cost_usd"`
	UnpricedTokens   int64      `json:"unpriced_tokens"`
	PercentOfTotal   string     `json:"percent_of_total"`
}

type LeaderboardPage struct {
	Meta   ResponseMeta     `json:"meta"`
	SortBy LeaderboardSort  `json:"sort_by"`
	Rows   []LeaderboardRow `json:"rows"`
}

// SortLeaderboard applies the contract tie-breaker and assigns ranks from one.
func SortLeaderboard(rows []LeaderboardRow, sortBy LeaderboardSort) {
	SortLeaderboardPage(rows, sortBy, 0)
}

// SortLeaderboardPage preserves global ranks when rows are fetched after a
// cursor. rankOffset is the final rank from the preceding page.
func SortLeaderboardPage(rows []LeaderboardRow, sortBy LeaderboardSort, rankOffset int) {
	slices.SortStableFunc(rows, func(left, right LeaderboardRow) int {
		var metric int
		switch sortBy {
		case LeaderboardSortTokens:
			metric = cmp.Compare(right.Tokens.Total, left.Tokens.Total)
		case LeaderboardSortCost:
			metric = cmp.Compare(right.KnownCost, left.KnownCost)
		}
		if metric != 0 {
			return metric
		}
		return cmp.Compare(left.KeyID, right.KeyID)
	})
	for index := range rows {
		rows[index].Rank = rankOffset + index + 1
	}
}

// Viewer DTOs intentionally contain no key digest. The authenticated viewer
// session supplies scope on the server.
type ViewerCapabilities struct {
	APISchemaVersion int       `json:"api_schema_version"`
	AllowedViews     []string  `json:"allowed_views"`
	Label            string    `json:"label,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type ViewerSummary struct {
	Meta             ResponseMeta `json:"meta"`
	Label            string       `json:"label,omitempty"`
	ProxyRequests    int64        `json:"proxy_requests"`
	UpstreamAttempts int64        `json:"upstream_attempts"`
	Tokens           TokenUsage   `json:"tokens"`
	KnownCost        NanoUSD      `json:"known_cost_usd"`
	UnpricedTokens   int64        `json:"unpriced_tokens"`
}

type ViewerEvent struct {
	AttemptID          string     `json:"attempt_id"`
	ProxyRequestID     string     `json:"proxy_request_id"`
	RequestedAt        time.Time  `json:"requested_at"`
	Provider           string     `json:"provider"`
	Model              string     `json:"model"`
	EndpointClass      string     `json:"endpoint_class"`
	Succeeded          bool       `json:"succeeded"`
	UpstreamStatusCode *int       `json:"upstream_status_code"`
	ErrorClass         *string    `json:"error_class"`
	LatencyMS          int64      `json:"latency_ms"`
	Tokens             TokenUsage `json:"tokens"`
	KnownCost          *NanoUSD   `json:"known_cost_usd"`
	UnpricedTokens     int64      `json:"unpriced_tokens"`
}

type ViewerEventPage struct {
	Meta   ResponseMeta  `json:"meta"`
	Label  string        `json:"label,omitempty"`
	Events []ViewerEvent `json:"events"`
}

type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

type JobStatus struct {
	JobID      string         `json:"job_id"`
	Kind       string         `json:"kind"`
	State      JobState       `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	StartedAt  *time.Time     `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
	Progress   int            `json:"progress_percent"`
	Checkpoint string         `json:"checkpoint,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	Error      *ErrorBody     `json:"error,omitempty"`
	Cancelable bool           `json:"cancelable"`
}

type ImportResult struct {
	BatchID     string `json:"batch_id"`
	DryRun      bool   `json:"dry_run"`
	RowsRead    int64  `json:"rows_read"`
	Transformed int64  `json:"transformed"`
	Inserted    int64  `json:"inserted"`
	Skipped     int64  `json:"skipped"`
	Rejected    int64  `json:"rejected"`
	Reconciled  bool   `json:"reconciled"`
}
