package chstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"time"
)

// The legacy decoder preserves SQLite scalar values without repricing or outcome classification.
const c4bEventSelect = `schema_version, attempt_id, proxy_request_id, request_id_quality, key_id,
requested_at_ns, provider, executor_type, model, requested_alias, endpoint_class, auth_type,
credential_id, credential_id_algorithm, succeeded, upstream_status_code, error_class,
latency_ms, time_to_first_token_ms, service_tier_requested, service_tier_used, generated,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, accounting_schema, token_quality, known_cost_nano, unpriced_tokens,
price_rule_id, price_source, import_batch_id, generation_time_ms,
client_method, client_path, received_at_ns, upstream_method, upstream_url, upstream_sent_at_ns,
upstream_usage_raw, upstream_error_body, proxy_status_code, proxy_error, responded_at_ns, first_token_latency_ms, provider_latency_ms, routing_time_ms, session_digest, output_timing, stream, reasoning_effort, provider_credits, price_usd_per_credit_nano, pricing_breakdown, session_id, outcome, failure_hop, cancellation_source,
(session_id IS NOT NULL OR outcome IS NOT NULL OR
(import_batch_id IS NULL AND requested_at_ns >= (SELECT applied_at_ns FROM schema_migrations WHERE version = 18))) AS session_captured`

type c4bEventScanner interface {
	Scan(...any) error
}

func c4bScanEvent(rows c4bEventScanner) (model.Event, error) {
	var event model.Event
	var requestedNS int64
	var requestQuality, tokenQuality string
	var requestedAlias, authType, credentialID, credentialAlgorithm, sessionDigest sql.NullString
	var status sql.NullInt64
	var errorClass, tierRequested, tierUsed, priceRuleID, priceSource, importBatchID sql.NullString
	var ttft, generation, firstTokenLatency, providerLatency, routingTime sql.NullInt64
	var knownCost sql.NullInt64
	var clientMethod, clientPath, upstreamMethod, upstreamURL, usageRaw, errorBody, proxyError sql.NullString
	var receivedNS, sentNS, respondedNS, proxyStatus sql.NullInt64
	var outputTiming, reasoningEffort sql.NullString
	var stream sql.NullBool
	var credits, breakdown, sessionID, outcome, failureHop, cancellationSource sql.NullString
	var creditRate sql.NullInt64
	err := rows.Scan(&event.SchemaVersion, &event.AttemptID, &event.ProxyRequestID, &requestQuality,
		&event.KeyID, &requestedNS, &event.Provider, &event.ExecutorType, &event.Model, &requestedAlias,
		&event.EndpointClass, &authType, &credentialID, &credentialAlgorithm, &event.Succeeded,
		&status, &errorClass, &event.LatencyMS, &ttft, &tierRequested, &tierUsed, &event.Generated,
		&event.Tokens.Input, &event.Tokens.Output, &event.Tokens.Reasoning, &event.Tokens.Cached,
		&event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total,
		&event.Tokens.Schema, &tokenQuality, &knownCost, &event.UnpricedTokens,
		&priceRuleID, &priceSource, &importBatchID, &generation,
		&clientMethod, &clientPath, &receivedNS, &upstreamMethod, &upstreamURL, &sentNS,
		&usageRaw, &errorBody, &proxyStatus, &proxyError, &respondedNS, &firstTokenLatency, &providerLatency, &routingTime, &sessionDigest, &outputTiming, &stream, &reasoningEffort, &credits, &creditRate, &breakdown, &sessionID, &outcome, &failureHop, &cancellationSource, &event.SessionCaptured)
	if err != nil {
		return model.Event{}, fmt.Errorf("scan analytics event: %w", err)
	}
	event.Outcome = outcome.String
	event.FailureHop = c4bNullableString(failureHop)
	event.CancellationSource = c4bNullableString(cancellationSource)
	event.SessionID = c4bNullableString(sessionID)
	if event.SessionID != nil {
		id, source := model.SplitSessionIdentity(*event.SessionID)
		if model.IsDerivedSessionIdentity(*event.SessionID) {
			source = "derived"
		}
		event.SessionID = &id
		if source != "" {
			event.SessionSource = &source
		}
	}
	if breakdown.Valid {
		event.PricingBreakdown = &model.PricingBreakdown{}
		if err := json.Unmarshal([]byte(breakdown.String), event.PricingBreakdown); err != nil {
			return model.Event{}, fmt.Errorf("decode pricing breakdown: %w", err)
		}
	}
	event.ProviderCredits = c4bNullableString(credits)
	if creditRate.Valid {
		rate := model.NanoUSD(creditRate.Int64)
		event.PriceUSDPerCredit = &rate
	}
	if stream.Valid {
		event.Stream = &stream.Bool
	}
	event.ReasoningEffort = c4bNullableString(reasoningEffort)
	if outputTiming.Valid {
		event.OutputTiming = &model.OutputTiming{}
		if err := json.Unmarshal([]byte(outputTiming.String), event.OutputTiming); err != nil || event.OutputTiming.Validate() != nil {
			// Optional telemetry must not hide the request's accounting record.
			event.OutputTiming = nil
		}
	}
	if routingTime.Valid {
		event.RoutingTimeMS = &routingTime.Int64
	}
	event.ClientMethod = c4bNullableString(clientMethod)
	event.ClientPath = c4bNullableString(clientPath)
	event.ReceivedAt = c4bNullableTime(receivedNS)
	event.UpstreamMethod = c4bNullableString(upstreamMethod)
	event.UpstreamURL = c4bNullableString(upstreamURL)
	event.UpstreamSentAt = c4bNullableTime(sentNS)
	if usageRaw.Valid && usageRaw.String != "" {
		raw := model.RawJSON(usageRaw.String)
		event.UpstreamUsageRaw = &raw
	}
	event.UpstreamErrorBody = c4bNullableString(errorBody)
	if proxyStatus.Valid {
		value := int(proxyStatus.Int64)
		event.ProxyStatusCode = &value
	}
	event.ProxyError = c4bNullableString(proxyError)
	event.RespondedAt = c4bNullableTime(respondedNS)
	event.RequestIDQuality = model.RequestIDQuality(requestQuality)
	event.RequestedAt = time.Unix(0, requestedNS).UTC()
	event.RequestedAlias = c4bNullableString(requestedAlias)
	event.AuthType = c4bNullableString(authType)
	event.CredentialID = c4bNullableString(credentialID)
	event.CredentialIDAlgorithm = c4bNullableString(credentialAlgorithm)
	event.SessionDigest = c4bNullableString(sessionDigest)
	event.ErrorClass = c4bNullableString(errorClass)
	event.ServiceTierRequested = c4bNullableString(tierRequested)
	event.ServiceTierUsed = c4bNullableString(tierUsed)
	if priceRuleID.Valid {
		event.PriceRuleID = priceRuleID.String
	}
	if priceSource.Valid {
		event.PriceSource = priceSource.String
	}
	if importBatchID.Valid {
		event.ImportBatchID = importBatchID.String
		event.Source = "import"
	} else {
		event.Source = "native"
	}
	event.Tokens.Quality = model.TokenQuality(tokenQuality)
	if knownCost.Valid {
		value := model.NanoUSD(knownCost.Int64)
		event.KnownCost = &value
	}
	if status.Valid {
		value := int(status.Int64)
		event.UpstreamStatusCode = &value
	}
	if firstTokenLatency.Valid {
		event.FirstTokenLatencyMS = &firstTokenLatency.Int64
	}
	if providerLatency.Valid {
		event.ProviderLatencyMS = &providerLatency.Int64
	}
	if generation.Valid {
		event.GenerationTimeMS = &generation.Int64
	}
	if ttft.Valid {
		value := ttft.Int64
		event.TimeToFirstTokenMS = &value
	}
	return event, nil
}

func c4bNullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	at := time.Unix(0, value.Int64).UTC()
	return &at
}

func c4bNullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
