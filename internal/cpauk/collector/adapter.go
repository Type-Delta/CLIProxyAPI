package collector

import (
	"context"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type Intake interface {
	Generation() uint64
	Enqueue(uint64, Event) bool
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

func adaptRecord(record coreusage.Record) Source {
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
		TTFT: record.TTFT, Failed: record.Failed, StatusCode: record.Fail.StatusCode,
		Tokens: SourceTokens{
			Input: record.Detail.InputTokens, Output: record.Detail.OutputTokens,
			Reasoning: record.Detail.ReasoningTokens, Cached: record.Detail.CachedTokens,
			CacheRead: record.Detail.CacheReadTokens, CacheCreation: record.Detail.CacheCreationTokens,
			Total:   record.Detail.TotalTokens,
			Quality: model.TokenQuality(record.Detail.TokenQuality),
		},
	}
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
