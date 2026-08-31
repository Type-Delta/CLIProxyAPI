package model

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Operation string

const (
	OperationSummary     Operation = "summary"
	OperationTimeseries  Operation = "timeseries"
	OperationDimensions  Operation = "dimensions"
	OperationEvents      Operation = "events"
	OperationLeaderboard Operation = "leaderboard"
)

type LeaderboardSort string

const (
	LeaderboardSortTokens LeaderboardSort = "tokens"
	LeaderboardSortCost   LeaderboardSort = "cost"
)

type Query struct {
	SchemaVersion int                        `json:"schema_version"`
	Operation     Operation                  `json:"operation"`
	Start         time.Time                  `json:"start"`
	End           time.Time                  `json:"end"`
	TimeZone      string                     `json:"time_zone"`
	KeyIDs        []string                   `json:"key_ids,omitempty"`
	Filters       map[string]json.RawMessage `json:"filters,omitempty"`
	Cursor        string                     `json:"cursor,omitempty"`
	PageSize      int                        `json:"page_size,omitempty"`
	BucketWidth   string                     `json:"bucket_width,omitempty"`
	Dimension     string                     `json:"dimension,omitempty"`
	SortBy        LeaderboardSort            `json:"sort_by,omitempty"`
}

var allowedQueryFilters = map[Operation]map[string]string{
	OperationSummary: {
		"provider": "strings", "model": "strings", "credential_id": "digests",
		"endpoint_class": "strings", "auth_type": "strings", "service_tier": "strings",
		"success": "bool", "error_class": "strings", "status_code": "integers",
		"token_quality": "strings",
	},
	OperationTimeseries: {
		"provider": "strings", "model": "strings", "credential_id": "digests",
		"endpoint_class": "strings", "auth_type": "strings", "service_tier": "strings",
		"success": "bool", "error_class": "strings", "status_code": "integers",
		"token_quality": "strings",
	},
	OperationDimensions: {
		"provider": "strings", "model": "strings", "credential_id": "digests",
		"endpoint_class": "strings", "auth_type": "strings", "service_tier": "strings",
		"success": "bool", "error_class": "strings", "status_code": "integers",
		"token_quality": "strings",
	},
	OperationEvents: {
		"provider": "strings", "model": "strings", "credential_id": "digests",
		"endpoint_class": "strings", "auth_type": "strings", "service_tier": "strings",
		"success": "bool", "error_class": "strings", "status_code": "integers",
		"token_quality": "strings", "generated": "bool",
	},
	OperationLeaderboard: {
		"provider": "strings", "model": "strings", "endpoint_class": "strings",
		"auth_type": "strings", "service_tier": "strings", "success": "bool",
		"error_class": "strings", "status_code": "integers", "token_quality": "strings",
	},
}

var allowedDimensions = map[string]struct{}{
	"provider": {}, "model": {}, "credential": {}, "key": {}, "endpoint": {},
	"failure": {}, "latency": {}, "cache": {}, "service_tier": {},
}

var allowedBucketWidths = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"1h": time.Hour, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour,
}

// ParseQuery rejects unknown JSON fields before applying operation-specific
// validation. It also normalizes duplicate key filters without reordering them.
func ParseQuery(data []byte, codecs ...*CursorCodec) (Query, error) {
	if len(data) == 0 || len(data) > MaxQueryBodyBytes {
		return Query{}, fmt.Errorf("query body must contain 1 to %d bytes", MaxQueryBodyBytes)
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return Query{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var query Query
	if err := decoder.Decode(&query); err != nil {
		return Query{}, fmt.Errorf("decode query v1: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Query{}, err
	}
	if err := query.Validate(); err != nil {
		return Query{}, err
	}
	if query.Cursor != "" {
		if len(codecs) != 1 || codecs[0] == nil {
			return Query{}, fmt.Errorf("cursor query requires a cursor codec")
		}
		if err := query.ValidateCursor(codecs[0], CursorTransportBody); err != nil {
			return Query{}, err
		}
	}
	return query, nil
}

func (q *Query) Validate() error {
	if q.SchemaVersion != QuerySchemaVersion {
		return fmt.Errorf("unsupported query schema version %d", q.SchemaVersion)
	}
	filterKinds, operationOK := allowedQueryFilters[q.Operation]
	if !operationOK {
		return fmt.Errorf("unsupported operation %q", q.Operation)
	}
	if err := validateRange(q.Start, q.End, q.TimeZone); err != nil {
		return err
	}
	if len(q.KeyIDs) > MaxKeyFilters {
		return fmt.Errorf("key_ids exceeds %d values", MaxKeyFilters)
	}
	deduplicated := make([]string, 0, len(q.KeyIDs))
	seenKeyIDs := make(map[string]struct{}, len(q.KeyIDs))
	for _, keyID := range q.KeyIDs {
		if !IsFullKeyID(keyID) {
			return fmt.Errorf("invalid key ID filter")
		}
		if _, seen := seenKeyIDs[keyID]; !seen {
			seenKeyIDs[keyID] = struct{}{}
			deduplicated = append(deduplicated, keyID)
		}
	}
	q.KeyIDs = deduplicated
	for name, value := range q.Filters {
		kind, ok := filterKinds[name]
		if !ok {
			return fmt.Errorf("filter %q is not allowed for %s", name, q.Operation)
		}
		if err := validateFilterValue(name, kind, value); err != nil {
			return err
		}
		normalized, err := normalizeFilterValue(kind, value)
		if err != nil {
			return fmt.Errorf("normalize filter %s: %w", name, err)
		}
		q.Filters[name] = normalized
	}

	switch q.Operation {
	case OperationSummary:
		if q.Cursor != "" || q.PageSize != 0 || q.BucketWidth != "" || q.Dimension != "" || q.SortBy != "" {
			return fmt.Errorf("summary does not accept cursor, page_size, bucket_width, dimension, or sort_by")
		}
	case OperationTimeseries:
		width, ok := allowedBucketWidths[q.BucketWidth]
		if !ok {
			return fmt.Errorf("unsupported bucket_width %q", q.BucketWidth)
		}
		if bucketCount(q.Start, q.End, width) > MaxBuckets {
			return fmt.Errorf("timeseries exceeds %d buckets", MaxBuckets)
		}
		if q.Cursor != "" || q.PageSize != 0 || q.Dimension != "" || q.SortBy != "" {
			return fmt.Errorf("timeseries does not accept cursor, page_size, dimension, or sort_by")
		}
	case OperationDimensions:
		if _, ok := allowedDimensions[q.Dimension]; !ok {
			return fmt.Errorf("unsupported dimension %q", q.Dimension)
		}
		if q.BucketWidth != "" || q.SortBy != "" {
			return fmt.Errorf("dimensions does not accept bucket_width or sort_by")
		}
		if err := q.validatePage(); err != nil {
			return err
		}
	case OperationEvents:
		if q.BucketWidth != "" || q.Dimension != "" || q.SortBy != "" {
			return fmt.Errorf("events does not accept bucket_width, dimension, or sort_by")
		}
		if err := q.validatePage(); err != nil {
			return err
		}
	case OperationLeaderboard:
		if q.SortBy != LeaderboardSortTokens && q.SortBy != LeaderboardSortCost {
			return fmt.Errorf("leaderboard sort_by must be tokens or cost")
		}
		if q.BucketWidth != "" || q.Dimension != "" {
			return fmt.Errorf("leaderboard does not accept bucket_width or dimension")
		}
		if err := q.validatePage(); err != nil {
			return err
		}
	}
	return nil
}

// SelectionDigest binds a cursor to every field that can change the selected
// rows. Page size and cursor position are pagination state and are excluded.
func (q Query) SelectionDigest() (string, error) {
	keyIDs := slices.Clone(q.KeyIDs)
	slices.Sort(keyIDs)
	keyIDs = slices.Compact(keyIDs)
	if keyIDs == nil {
		keyIDs = []string{}
	}
	filters := q.Filters
	if filters == nil {
		filters = map[string]json.RawMessage{}
	}
	selection := struct {
		SchemaVersion int                        `json:"schema_version"`
		Operation     Operation                  `json:"operation"`
		Start         string                     `json:"start"`
		End           string                     `json:"end"`
		TimeZone      string                     `json:"time_zone"`
		KeyIDs        []string                   `json:"key_ids"`
		Filters       map[string]json.RawMessage `json:"filters"`
		BucketWidth   string                     `json:"bucket_width"`
		Dimension     string                     `json:"dimension"`
		SortBy        LeaderboardSort            `json:"sort_by"`
	}{
		SchemaVersion: q.SchemaVersion,
		Operation:     q.Operation,
		Start:         q.Start.UTC().Format(time.RFC3339Nano),
		End:           q.End.UTC().Format(time.RFC3339Nano),
		TimeZone:      q.TimeZone,
		KeyIDs:        keyIDs,
		Filters:       filters,
		BucketWidth:   q.BucketWidth,
		Dimension:     q.Dimension,
		SortBy:        q.SortBy,
	}
	data, err := json.Marshal(selection)
	if err != nil {
		return "", fmt.Errorf("encode normalized query selection: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

type CursorTransport string

const (
	CursorTransportBody CursorTransport = "body"
	CursorTransportGET  CursorTransport = "get"
)

// CursorAllowedInGET rejects cursors whose internal ordering or selection
// contains a key or credential identity, even though the wire token is opaque.
func (q Query) CursorAllowedInGET() bool {
	if q.Operation == OperationLeaderboard || len(q.KeyIDs) != 0 {
		return false
	}
	if _, ok := q.Filters["credential_id"]; ok {
		return false
	}
	return q.Operation != OperationDimensions || q.Dimension != "key" && q.Dimension != "credential"
}

func (q *Query) ValidateCursor(codec *CursorCodec, transport CursorTransport) error {
	if q.Cursor == "" {
		return nil
	}
	if codec == nil {
		return fmt.Errorf("cursor codec is required")
	}
	if transport == CursorTransportGET && !q.CursorAllowedInGET() {
		return fmt.Errorf("identity-bearing cursor is forbidden in GET queries")
	}
	cursor, err := codec.Decode(q.Cursor)
	if err != nil {
		return err
	}
	if cursor.Operation != q.Operation || cursor.SortBy != q.SortBy {
		return fmt.Errorf("cursor does not match query operation and sort")
	}
	selection, err := q.SelectionDigest()
	if err != nil {
		return err
	}
	if cursor.Selection != selection {
		return fmt.Errorf("cursor does not match the normalized query selection")
	}
	return nil
}

func (q *Query) validatePage() error {
	if q.PageSize == 0 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize < 1 || q.PageSize > MaxPageSize {
		return fmt.Errorf("page_size must be between 1 and %d", MaxPageSize)
	}
	if len(q.Cursor) > MaxCursorBytes {
		return fmt.Errorf("cursor exceeds %d bytes", MaxCursorBytes)
	}
	return nil
}

func validateRange(start, end time.Time, zoneName string) error {
	if start.IsZero() || end.IsZero() || start.Location() != time.UTC || end.Location() != time.UTC {
		return fmt.Errorf("start and end must be nonzero UTC timestamps")
	}
	if !start.Before(end) {
		return fmt.Errorf("start must precede end")
	}
	if end.Sub(start) > MaxQueryRangeDays*24*time.Hour {
		return fmt.Errorf("query range exceeds %d elapsed days", MaxQueryRangeDays)
	}
	if strings.TrimSpace(zoneName) != zoneName || zoneName == "" {
		return fmt.Errorf("time_zone is required")
	}
	if _, err := time.LoadLocation(zoneName); err != nil {
		return fmt.Errorf("invalid IANA time zone %q", zoneName)
	}
	return nil
}

func validateFilterValue(name, kind string, raw json.RawMessage) error {
	switch kind {
	case "bool":
		var value bool
		if err := decodeStrictValue(raw, &value); err != nil {
			return fmt.Errorf("filter %s must be a boolean", name)
		}
	case "strings", "digests":
		values, err := decodeStringOrStrings(raw)
		if err != nil || len(values) == 0 || len(values) > MaxFilterValues {
			return fmt.Errorf("filter %s must contain 1 to %d strings", name, MaxFilterValues)
		}
		for _, value := range values {
			if kind == "digests" {
				if !IsFullKeyID(value) {
					return fmt.Errorf("filter %s contains an invalid digest", name)
				}
			} else if err := validateBoundedString(name, value, false); err != nil {
				return err
			}
		}
	case "integers":
		values, err := decodeIntOrInts(raw)
		if err != nil || len(values) == 0 || len(values) > MaxFilterValues {
			return fmt.Errorf("filter %s must contain 1 to %d integers", name, MaxFilterValues)
		}
		for _, value := range values {
			if value < 100 || value > 599 {
				return fmt.Errorf("filter %s contains an invalid HTTP status", name)
			}
		}
	default:
		return fmt.Errorf("unknown filter contract %q", kind)
	}
	return nil
}

func normalizeFilterValue(kind string, raw json.RawMessage) (json.RawMessage, error) {
	var value any
	switch kind {
	case "bool":
		var parsed bool
		if err := decodeStrictValue(raw, &parsed); err != nil {
			return nil, err
		}
		value = parsed
	case "strings", "digests":
		parsed, err := decodeStringOrStrings(raw)
		if err != nil {
			return nil, err
		}
		slices.Sort(parsed)
		value = slices.Compact(parsed)
	case "integers":
		parsed, err := decodeIntOrInts(raw)
		if err != nil {
			return nil, err
		}
		slices.Sort(parsed)
		value = slices.Compact(parsed)
	default:
		return nil, fmt.Errorf("unknown filter contract %q", kind)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func decodeStringOrStrings(raw json.RawMessage) ([]string, error) {
	var one string
	if err := decodeStrictValue(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := decodeStrictValue(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func decodeIntOrInts(raw json.RawMessage) ([]int, error) {
	var one int
	if err := decodeStrictValue(raw, &one); err == nil {
		return []int{one}, nil
	}
	var many []int
	if err := decodeStrictValue(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func decodeStrictValue(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid query JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("object field name is not a string")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate object field %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func bucketCount(start, end time.Time, width time.Duration) int64 {
	return int64((end.Sub(start) + width - 1) / width)
}

type Cursor struct {
	Version     int             `json:"version"`
	Operation   Operation       `json:"operation"`
	SortBy      LeaderboardSort `json:"sort_by,omitempty"`
	Selection   string          `json:"selection"`
	RequestedAt *time.Time      `json:"requested_at,omitempty"`
	AttemptID   string          `json:"attempt_id,omitempty"`
	Metric      string          `json:"metric,omitempty"`
	KeyID       string          `json:"key_id,omitempty"`
	Value       string          `json:"value,omitempty"`
	Rank        int             `json:"rank,omitempty"`
}

type CursorCodec struct {
	aead cipher.AEAD
}

var cursorAssociatedData = []byte("cpauk-cursor-v1")

func NewCursorCodec(key []byte) (*CursorCodec, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cursor key must contain 32 bytes")
	}
	block, err := aes.NewCipher(slices.Clone(key))
	if err != nil {
		return nil, fmt.Errorf("create cursor cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create cursor AEAD: %w", err)
	}
	return &CursorCodec{aead: aead}, nil
}

func (c *CursorCodec) Encode(cursor Cursor) (string, error) {
	if c == nil || c.aead == nil {
		return "", fmt.Errorf("cursor codec is not initialized")
	}
	if err := cursor.Validate(); err != nil {
		return "", err
	}
	plaintext, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode cursor payload: %w", err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate cursor nonce: %w", err)
	}
	envelope := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	envelope[0] = 1
	envelope = append(envelope, nonce...)
	envelope = c.aead.Seal(envelope, nonce, plaintext, cursorAssociatedData)
	encoded := base64.RawURLEncoding.EncodeToString(envelope)
	if len(encoded) > MaxCursorBytes {
		return "", fmt.Errorf("encoded cursor exceeds %d bytes", MaxCursorBytes)
	}
	return encoded, nil
}

func (c *CursorCodec) Decode(value string) (Cursor, error) {
	if c == nil || c.aead == nil {
		return Cursor{}, fmt.Errorf("cursor codec is not initialized")
	}
	if value == "" || len(value) > MaxCursorBytes {
		return Cursor{}, fmt.Errorf("invalid cursor length")
	}
	envelope, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor encoding")
	}
	minimum := 1 + c.aead.NonceSize() + c.aead.Overhead()
	if len(envelope) < minimum || envelope[0] != 1 {
		return Cursor{}, fmt.Errorf("invalid cursor envelope")
	}
	nonce := envelope[1 : 1+c.aead.NonceSize()]
	ciphertext := envelope[1+c.aead.NonceSize():]
	data, err := c.aead.Open(nil, nonce, ciphertext, cursorAssociatedData)
	if err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor authentication")
	}
	var cursor Cursor
	if err := decodeStrictValue(data, &cursor); err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor payload: %w", err)
	}
	if err := cursor.Validate(); err != nil {
		return Cursor{}, err
	}
	return cursor, nil
}

func (c Cursor) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported cursor version %d", c.Version)
	}
	if !IsFullKeyID(c.Selection) {
		return fmt.Errorf("cursor selection must be a SHA-256 digest")
	}
	switch c.Operation {
	case OperationEvents:
		if c.SortBy != "" || c.RequestedAt == nil || c.RequestedAt.IsZero() || c.RequestedAt.Location() != time.UTC || !IsCorrelationID(c.AttemptID) || c.Metric != "" || c.KeyID != "" || c.Value != "" || c.Rank != 0 {
			return fmt.Errorf("invalid events cursor")
		}
	case OperationDimensions:
		if c.SortBy != "" || c.RequestedAt != nil || c.AttemptID != "" || c.KeyID != "" || c.Value == "" || c.Rank < 1 {
			return fmt.Errorf("invalid dimensions cursor")
		}
		if metric, err := strconv.ParseInt(c.Metric, 10, 64); err != nil || metric < 0 {
			return fmt.Errorf("invalid dimensions cursor metric")
		}
		if err := validateBoundedString("dimension cursor value", c.Value, false); err != nil {
			return err
		}
	case OperationLeaderboard:
		if c.RequestedAt != nil || c.AttemptID != "" || !IsFullKeyID(c.KeyID) || c.Value != "" || c.Rank < 1 {
			return fmt.Errorf("invalid leaderboard cursor")
		}
		if c.SortBy == LeaderboardSortTokens {
			if metric, err := strconv.ParseInt(c.Metric, 10, 64); err != nil || metric < 0 {
				return fmt.Errorf("invalid token leaderboard cursor metric")
			}
		} else if c.SortBy == LeaderboardSortCost {
			if metric, err := ParseNanoUSD(c.Metric); err != nil || metric < 0 {
				return fmt.Errorf("invalid cost leaderboard cursor metric")
			}
		} else {
			return fmt.Errorf("invalid leaderboard cursor sort")
		}
	default:
		return fmt.Errorf("operation %q does not support a cursor", c.Operation)
	}
	return nil
}
