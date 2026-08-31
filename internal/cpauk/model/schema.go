package model

const (
	EventSchemaVersion = 1
	APISchemaVersion   = 1
	QuerySchemaVersion = 1

	MaxEventBytes        = 4 * 1024
	MaxStoredStringBytes = 256
	MaxQueryBodyBytes    = 64 * 1024
	MaxCursorBytes       = 2 * 1024
	MaxFilterValues      = 50
	MaxKeyFilters        = 100
	DefaultPageSize      = 100
	MaxPageSize          = 500
	MaxBuckets           = 10_000
	MaxExportRows        = 100_000
	MaxQueryRangeDays    = 400
)

// FieldSpec freezes the storage contract for one Event v1 field.
type FieldSpec struct {
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	Truncate bool   `json:"truncate"`
}

// EventV1FieldSpecs is the complete persisted-field allowlist. Fields absent
// from this map cannot be stored in an Event v1 row.
var EventV1FieldSpecs = map[string]FieldSpec{
	"schema_version":          {Type: "integer", Nullable: false},
	"attempt_id":              {Type: "hex-128", Nullable: false, MaxBytes: 32},
	"proxy_request_id":        {Type: "hex-128", Nullable: false, MaxBytes: 32},
	"request_id_quality":      {Type: "enum", Nullable: false, MaxBytes: 16},
	"key_id":                  {Type: "sha256", Nullable: false, MaxBytes: 64},
	"requested_at":            {Type: "rfc3339-utc", Nullable: false, MaxBytes: 30},
	"provider":                {Type: "string", Nullable: false, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"executor_type":           {Type: "string", Nullable: false, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"model":                   {Type: "string", Nullable: false, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"requested_alias":         {Type: "string", Nullable: true, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"endpoint_class":          {Type: "string", Nullable: false, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"auth_type":               {Type: "string", Nullable: true, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"credential_id":           {Type: "hmac-sha256", Nullable: true, MaxBytes: 64},
	"credential_id_algorithm": {Type: "enum", Nullable: true, MaxBytes: 18},
	"succeeded":               {Type: "boolean", Nullable: false},
	"upstream_status_code":    {Type: "integer", Nullable: true},
	"error_class":             {Type: "string", Nullable: true, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"latency_ms":              {Type: "integer", Nullable: false},
	"time_to_first_token_ms":  {Type: "integer", Nullable: true},
	"service_tier_requested":  {Type: "string", Nullable: true, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"service_tier_used":       {Type: "string", Nullable: true, MaxBytes: MaxStoredStringBytes, Truncate: true},
	"generated":               {Type: "boolean", Nullable: false},
	"tokens":                  {Type: "token-usage-v1", Nullable: false},
}

// TokenUsageV1FieldSpecs freezes every field nested below Event.tokens.
var TokenUsageV1FieldSpecs = map[string]FieldSpec{
	"input":             {Type: "integer", Nullable: false},
	"output":            {Type: "integer", Nullable: false},
	"reasoning":         {Type: "integer", Nullable: false},
	"cached":            {Type: "integer", Nullable: false},
	"cache_read":        {Type: "integer", Nullable: false},
	"cache_creation":    {Type: "integer", Nullable: false},
	"total":             {Type: "integer", Nullable: false},
	"accounting_schema": {Type: "string", Nullable: false, MaxBytes: MaxStoredStringBytes},
	"quality":           {Type: "enum", Nullable: false, MaxBytes: 16},
}

// ForbiddenEventFields names source data that the sanitizer must never copy.
var ForbiddenEventFields = map[string]struct{}{
	"api_key": {}, "raw_key": {}, "access_token": {}, "request_headers": {},
	"response_headers": {}, "request_body": {}, "response_body": {},
	"failure_body": {}, "ip_address": {}, "forwarded_for": {}, "user_agent": {},
	"auth_id": {}, "auth_index": {}, "source_metadata": {},
}
