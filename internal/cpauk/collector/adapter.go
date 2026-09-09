package collector

import (
	"context"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type Intake interface {
	Generation() uint64
	Enqueue(uint64, Event) bool
	EnqueueProxyResponse(uint64, ProxyResponsePatch) bool
	Rejected()
	Truncated(int64)
}

type Event = model.Event

// Adapter is the only CPAUK type that accepts a raw usage.Record. It is tied
// to one intake generation, so callbacks retained across a swap can only drop.
type Adapter struct {
	intake     Intake
	sanitizer  *Sanitizer
	generation uint64
	dropped    atomic.Int64
}

func NewAdapter(intake Intake, sanitizer *Sanitizer) *Adapter {
	generation := uint64(0)
	if intake != nil {
		generation = intake.Generation()
	}
	return &Adapter{intake: intake, sanitizer: sanitizer, generation: generation}
}

func (a *Adapter) HandleUsage(ctx context.Context, record coreusage.Record) {
	if a == nil {
		return
	}
	defer func() {
		if recover() != nil {
			a.dropped.Add(1)
			safeReject(a.intake)
		}
	}()
	if a.intake == nil || a.sanitizer == nil {
		a.reject(0)
		return
	}
	result, err := a.sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		a.reject(0)
		return
	}
	if result.TruncatedFields > 0 {
		a.intake.Truncated(result.TruncatedFields)
	}
	if !a.intake.Enqueue(a.generation, result.Event) {
		a.dropped.Add(1)
	}
}

// HandleProxyResponse queues the downstream outcome so the CPA -> Client leg of
// every attempt for that request is completed. It never blocks.
func (a *Adapter) HandleProxyResponse(response coreusage.ProxyResponse) {
	if a == nil || a.intake == nil {
		return
	}
	defer func() {
		if recover() != nil {
			a.dropped.Add(1)
		}
	}()
	if !model.IsCorrelationID(response.ProxyRequestID) || response.StatusCode < 100 || response.StatusCode > 599 {
		return
	}
	patch := ProxyResponsePatch{
		ProxyRequestID: response.ProxyRequestID,
		StatusCode:     response.StatusCode,
		Error:          boundedProxyError(response.Error),
		RespondedAt:    response.RespondedAt.UTC(),
	}
	if !a.intake.EnqueueProxyResponse(a.generation, patch) {
		a.dropped.Add(1)
	}
}

func boundedProxyError(value string) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		return ""
	}
	if len(value) <= model.MaxProxyErrorBytes {
		return value
	}
	value = value[:model.MaxProxyErrorBytes-len(truncationMarker)]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + truncationMarker
}

func adaptRecord(record coreusage.Record) Source {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	inputTokens := detail.InputTokens
	if detail.TokenBreakdown.Valid() && (detail.TokenBreakdown.Input.TotalTokens > 0 || detail.InputTokens == 0) {
		inputTokens = detail.TokenBreakdown.Input.TotalTokens
	}
	outputTokens := detail.OutputTokens
	if detail.TokenBreakdown.Valid() && (detail.TokenBreakdown.Output.TotalTokens > 0 || detail.OutputTokens == 0) {
		outputTokens = detail.TokenBreakdown.Output.TotalTokens
	}
	requestQuality := model.RequestIDObserved
	if record.RequestIDQuality == coreusage.RequestIDSynthetic {
		requestQuality = model.RequestIDSynthetic
	}
	responseTier := record.ResponseServiceTier
	if responseTier == "" {
		responseTier = record.Detail.ResponseServiceTier
	}
	return Source{
		ProxyRequestID: record.ProxyRequestID, RequestQuality: requestQuality,
		EndpointClass: record.EndpointClass, Provider: record.Provider,
		ExecutorType: record.ExecutorType, Model: record.Model, Alias: record.Alias,
		APIKey: record.APIKey, AuthID: record.AuthID, AuthIndex: record.AuthIndex,
		AuthType: record.AuthType, ServiceTier: record.ServiceTier, ResponseTier: responseTier,
		Generated: record.Generate, RequestedAt: record.RequestedAt, Latency: record.Latency,
		GenerationTime: record.GenerationTime, TTFT: record.TTFT, Failed: record.Failed,
		StatusCode: upstreamStatus(record), UpstreamStatusCode: record.UpstreamStatusCode, FailureBody: record.Fail.Body,
		ClientMethod: record.ClientMethod, ClientPath: record.ClientPath, ReceivedAt: record.ReceivedAt,
		UpstreamMethod: record.UpstreamMethod, UpstreamURL: record.UpstreamURL, UpstreamSentAt: record.UpstreamSentAt,
		RawUsage: detail.RawUsage,
		Tokens: SourceTokens{
			Input: inputTokens, Output: outputTokens,
			Reasoning: detail.ReasoningTokens, Cached: detail.CachedTokens,
			CacheRead: detail.CacheReadTokens, CacheCreation: detail.CacheCreationTokens,
			Total:   detail.TotalTokens,
			Quality: model.TokenQuality(detail.TokenQuality),
		},
	}
}

// upstreamStatus prefers the observed provider status so successful attempts
// record 200 too; failures without a transport status keep the failure code.
func upstreamStatus(record coreusage.Record) int {
	if record.UpstreamStatusCode != 0 {
		return record.UpstreamStatusCode
	}
	return record.Fail.StatusCode
}

func (a *Adapter) Dropped() int64 {
	if a == nil {
		return 0
	}
	return a.dropped.Load()
}

func (a *Adapter) reject(_ int64) {
	a.dropped.Add(1)
	safeReject(a.intake)
}

func safeReject(intake Intake) {
	defer func() { _ = recover() }()
	if intake != nil {
		intake.Rejected()
	}
}
