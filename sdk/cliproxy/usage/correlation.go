package usage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// RequestIDQuality identifies how a proxy request ID was assigned.
type RequestIDQuality string

const (
	RequestIDObserved  RequestIDQuality = "observed"
	RequestIDSynthetic RequestIDQuality = "synthetic"
)

type proxyRequestIDContextKey struct{}

var fallbackRequestIDCounter atomic.Uint64

// NewProxyRequestID returns a lowercase 128-bit identifier. The hashed fallback
// remains process-unique when the operating system random source is unavailable.
func NewProxyRequestID() string {
	var value [16]byte
	if _, errRead := rand.Read(value[:]); errRead == nil {
		return hex.EncodeToString(value[:])
	}

	var material [24]byte
	binary.BigEndian.PutUint64(material[0:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(material[8:16], fallbackRequestIDCounter.Add(1))
	binary.BigEndian.PutUint64(material[16:24], uint64(os.Getpid()))
	sum := sha256.Sum256(material[:])
	return hex.EncodeToString(sum[:16])
}

// ValidProxyRequestID reports whether value is a lowercase 128-bit hex ID.
func ValidProxyRequestID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// WithProxyRequestID stores a valid request ID in ctx. Invalid IDs are ignored.
func WithProxyRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID = strings.TrimSpace(requestID)
	if !ValidProxyRequestID(requestID) {
		return ctx
	}
	return context.WithValue(ctx, proxyRequestIDContextKey{}, requestID)
}

// ProxyRequestIDFromContext returns the request ID stored in ctx.
func ProxyRequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(proxyRequestIDContextKey{}).(string)
	requestID = strings.TrimSpace(requestID)
	if !ValidProxyRequestID(requestID) {
		return ""
	}
	return requestID
}

// EnsureProxyRequestID returns a context with one observed request ID.
func EnsureProxyRequestID(ctx context.Context) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if requestID := ProxyRequestIDFromContext(ctx); requestID != "" {
		return ctx, requestID
	}
	requestID := NewProxyRequestID()
	return WithProxyRequestID(ctx, requestID), requestID
}

func normalizeRecordRequestID(ctx context.Context, record Record) Record {
	record.ProxyRequestID = strings.TrimSpace(record.ProxyRequestID)
	if ValidProxyRequestID(record.ProxyRequestID) {
		if record.RequestIDQuality != RequestIDSynthetic {
			record.RequestIDQuality = RequestIDObserved
		}
		return record
	}
	if requestID := ProxyRequestIDFromContext(ctx); requestID != "" {
		record.ProxyRequestID = requestID
		record.RequestIDQuality = RequestIDObserved
		return record
	}
	record.ProxyRequestID = NewProxyRequestID()
	record.RequestIDQuality = RequestIDSynthetic
	return record
}
