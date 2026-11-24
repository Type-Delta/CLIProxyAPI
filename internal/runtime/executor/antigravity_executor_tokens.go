package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if errGuard := e.checkAntigravityOAuthIncoming(auth, req, opts, true); errGuard != nil {
		return cliproxyexecutor.Response{}, helps.PublishAttemptFailure(ctx, e, baseModel, auth, errGuard)
	}
	if antigravityInTOSPause(e.cfg, auth) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity pool paused after TOS_VIOLATION"}
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayloadSource, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayloadSource)
	if errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	req.Payload = originalPayloadSource

	// Prepare payload once (doesn't depend on baseURL). A translation refusal is
	// observed before the token refresh or any upstream call.
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	groupInfo := helps.AntigravityGroupedModel(auth, baseModel, opts)
	if groupInfo != nil {
		modelInfo = groupInfo
	}
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Body: req.Payload, ModelInfo: modelInfo}
	translatedReq := helps.TranslateRequestEnvelopeWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq)
	if translatedReq.Err != nil {
		return cliproxyexecutor.Response{}, translatedReq.Err
	}
	payload := translatedReq.Body
	originalTranslatedForPayload := append([]byte(nil), payload...)

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	cliproxyauth.NotifyAccessTokenFingerprint(ctx, auth)
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	doc := ojson.NewDoc(payload)
	if errThinking := helps.ApplyAntigravityRequestThinkingDoc(doc, req, opts, groupInfo); errThinking != nil {
		return cliproxyexecutor.Response{}, errThinking
	}
	e.obfuscateSensitiveWordsDoc(doc)
	helps.PrepareAntigravityClaudeStructuredOutputDoc(baseModel, doc)
	sanitizeAntigravityGeminiRequestSignaturesDoc(baseModel, doc)
	if _, errReplay := prepareAntigravityGeminiCountTokensReasoningReplayPayloadDoc(ctx, baseModel, req, opts, doc); errReplay != nil {
		return cliproxyexecutor.Response{}, errReplay
	}
	ensureAntigravityGeminiLeadingUserContentDoc(baseModel, doc)

	_ = doc.Delete("project")
	_ = doc.Delete("model")
	_ = doc.Delete("requestId")
	_ = doc.Delete("requestType")
	_ = doc.Delete("userAgent")
	_ = doc.Delete("request.safetySettings")
	_ = doc.Delete("request.toolConfig")
	_ = doc.Delete("request.labels")
	_ = doc.Delete("request.sessionId")

	base := resolveAntigravityRequestBaseURL(auth)
	httpClient := e.antigravityInferenceClient(ctx, auth)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(antigravityCountTokensPath)
	if opts.Alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(opts.Alt))
	}

	ctx = helps.WithPayloadFinalizer(ctx, helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "request", originalTranslatedForPayload, req, opts))
	helps.FinalizePayloadDoc(ctx, doc)
	payload = doc.Bytes()
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
	if errReq != nil {
		return cliproxyexecutor.Response{}, errReq
	}
	// No httpReq.Close: keep the shared Antigravity connection pool usable.
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	if errGuard := e.prepareAntigravityInferenceHeaders(auth, httpReq, doc, token, true); errGuard != nil {
		return cliproxyexecutor.Response{}, helps.PublishAttemptFailure(ctx, e, baseModel, auth, errGuard)
	}

	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	httpResp, errDo := helps.WithAntigravityHTTPClientTrace(httpClient, auth, "count_tokens").Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return cliproxyexecutor.Response{}, errDo
	}

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return cliproxyexecutor.Response{}, errRead
	}

	if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
		count := ojson.GetBytesNoCopy(bodyBytes, "totalTokens").Int()
		translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, bodyBytes)
		return cliproxyexecutor.Response{Payload: translated, Headers: httpResp.Header.Clone()}, nil
	}

	if httpResp.StatusCode == http.StatusForbidden {
		e.applyAntigravityTOSState(e.cfg, auth, bodyBytes)
	}
	sErr := newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
	if httpResp.StatusCode == http.StatusTooManyRequests {
		closeAntigravityAuthIdleTransports(auth)
		if retryAfter, parseErr := helps.ParseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
			sErr.retryAfter = retryAfter
		}
	}
	return cliproxyexecutor.Response{}, sErr
}
