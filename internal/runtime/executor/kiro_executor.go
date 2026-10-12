package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	kiroUserAgent     = "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-cpa"
	kiroUserAgentHint = "aws-sdk-js/1.0.27 KiroIDE-0.7.45-cpa"
)

// KiroExecutor sends Claude-compatible requests to the Kiro runtime endpoint.
type KiroExecutor struct {
	cfg *config.Config
	mu  sync.Mutex
}

func NewKiroExecutor(cfg *config.Config) *KiroExecutor { return &KiroExecutor{cfg: cfg} }
func (e *KiroExecutor) Identifier() string             { return kiroauth.Provider }

func (e *KiroExecutor) endpoint(auth *cliproxyauth.Auth) string {
	region := kiroauth.DefaultRegion
	if auth != nil {
		region = authValue(auth, "region", region)
		if authValue(auth, "profile_arn", "") == "" {
			// AWS Builder/Identity Center credentials without a CodeWhisperer
			// profile are served by the legacy q.* endpoint, which does not
			// require profileArn. Paid Kiro profiles continue using runtime.*.
			return fmt.Sprintf(kiroauth.LegacyRuntimeURLTemplate, strings.TrimSpace(region))
		}
	}
	return fmt.Sprintf(kiroauth.RuntimeURLTemplate, strings.TrimSpace(region))
}

func (e *KiroExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := authValue(auth, "access_token", "")
	if token == "" {
		token = authValue(auth, "accessToken", "")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	req.Header.Set("User-Agent", kiroUserAgent)
	req.Header.Set("x-amz-user-agent", kiroUserAgentHint)
	req.Header.Set("x-amz-target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	return nil
}

func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	request := req.WithContext(ctx)
	if err := e.PrepareRequest(request, auth); err != nil {
		return nil, err
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return client.Do(request)
}

func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "kiro executor: auth is nil"}
	}
	storage := &kiroauth.TokenStorage{
		AccessToken: authValue(auth, "access_token", ""), RefreshToken: authValue(auth, "refresh_token", ""),
		ClientID: authValue(auth, "client_id", ""), ClientSecret: authValue(auth, "client_secret", ""),
		ProfileARN: authValue(auth, "profile_arn", ""), Region: authValue(auth, "region", kiroauth.DefaultRegion), AuthMethod: authValue(auth, "auth_method", ""),
	}
	authSvc := kiroauth.NewAuth(e.cfg)
	refreshed, err := authSvc.RefreshToken(ctx, storage)
	if err != nil {
		return nil, err
	}
	if err := kiroauth.ApplyTokenResponse(storage, refreshed); err != nil {
		return nil, err
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	for key, value := range map[string]any{"access_token": storage.AccessToken, "refresh_token": storage.RefreshToken, "token_type": storage.TokenType, "profile_arn": storage.ProfileARN, "expired": storage.ExpiresAt, "last_refresh": storage.LastRefresh} {
		if value != "" {
			updated.Metadata[key] = value
		}
	}
	return updated, nil
}

// ShouldPrepareRequestAuth reports whether a profile lookup can enrich a
// profile-less Kiro credential before its first request.
func (e *KiroExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || authValue(auth, "access_token", "") == "" || authValue(auth, "profile_arn", "") != "" {
		return false
	}
	if auth.Metadata != nil {
		if attempted, ok := auth.Metadata["profile_lookup_attempted"].(bool); ok && attempted {
			return false
		}
	}
	return true
}

// PrepareRequestAuth discovers the profile omitted by some Identity Center
// device-flow token responses. A failed or empty lookup is non-fatal because
// profile-less credentials can still use the legacy q.* runtime endpoint.
func (e *KiroExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["profile_lookup_attempted"] = true
	region := authValue(auth, "region", kiroauth.DefaultRegion)
	profile, err := kiroauth.NewAuth(e.cfg).ResolveProfileARN(ctx, authValue(auth, "access_token", ""), region)
	if err != nil {
		return updated, nil
	}
	if profile == "" {
		return updated, nil
	}
	updated.Metadata["profile_arn"] = profile
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	updated.Attributes["profile_arn"] = profile
	return updated, nil
}

func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opts.Alt == "responses/compact" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "kiro executor: compact is not supported"}
	}
	conversationID := executionConversationID(opts, req)
	profile := authValue(auth, "profile_arn", "")
	requestPayload := kiroRequestPayload(req, opts)
	body, err := buildKiroPayload(req.Model, requestPayload, profile, conversationID)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint(auth), bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if err := e.PrepareRequest(request, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	response, err := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(request)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return cliproxyexecutor.Response{}, statusErr{
			code:       response.StatusCode,
			msg:        fmt.Sprintf("kiro upstream returned %d: %s", response.StatusCode, strings.TrimSpace(string(data))),
			retryAfter: openAICompatRetryAfter(response.StatusCode, response.Header, nil, time.Now()),
		}
	}
	result, err := collectKiroEvents(response.Body, req.Model, requestPayload)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	claudePayload := result.claudeResponse(req.Model)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var patch any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatClaude, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), requestPayload, claudePayload, &patch)
	if len(out) == 0 {
		out = claudePayload
	}
	return cliproxyexecutor.Response{Payload: out, Headers: response.Header.Clone()}, nil
}

func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	profile := authValue(auth, "profile_arn", "")
	requestPayload := kiroRequestPayload(req, opts)
	body, err := buildKiroPayload(req.Model, requestPayload, profile, executionConversationID(opts, req))
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint(auth), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if err := e.PrepareRequest(request, auth); err != nil {
		return nil, err
	}
	response, err := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		return nil, statusErr{
			code:       response.StatusCode,
			msg:        fmt.Sprintf("kiro upstream returned %d: %s", response.StatusCode, strings.TrimSpace(string(data))),
			retryAfter: openAICompatRetryAfter(response.StatusCode, response.Header, nil, time.Now()),
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk, 8)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var translateParam any
	go func() {
		defer close(out)
		defer response.Body.Close()
		events := newKiroEventAccumulator(req.Model, requestPayload)
		emit := func(payload []byte) bool {
			if len(payload) == 0 {
				return true
			}
			if responseFormat != sdktranslator.FormatClaude {
				return emitTranslatedKiroSSE(ctx, out, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), requestPayload, payload, &translateParam)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		startPayload := claudeSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "type": "message", "role": "assistant", "model": req.Model,
				"content": []any{}, "usage": map[string]any{"input_tokens": events.inputTokens},
			},
		})
		if !emit(startPayload) {
			return
		}
		if err := parseKiroEventStream(response.Body, func(eventName string, data map[string]any) bool { return emit(events.consume(eventName, data)) }); err != nil {
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		for _, payload := range events.finishSSE(req.Model) {
			if !emit(payload) {
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: response.Header.Clone(), Chunks: out}, nil
}

func emitTranslatedKiroSSE(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, responseFormat sdktranslator.Format, model string, original, requestPayload, payload []byte, param *any) bool {
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatClaude, responseFormat, model, original, requestPayload, line, param) {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
			case <-ctx.Done():
				return false
			}
		}
	}
	return true
}

// kiroRequestPayload converts the client protocol into Claude Messages before
// the Kiro-specific payload builder runs. Kiro exposes Claude-compatible models
// but its runtime wire format only accepts the Claude Messages shape.
func kiroRequestPayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) []byte {
	if len(req.Payload) == 0 || opts.SourceFormat == "" || opts.SourceFormat == sdktranslator.FormatClaude {
		return req.Payload
	}
	return sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatClaude, req.Model, req.Payload, opts.Stream)
}

func (e *KiroExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	count := len(req.Payload) / 4
	if count < 1 {
		count = 1
	}
	return cliproxyexecutor.Response{Payload: []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))}, nil
}

func authValue(auth *cliproxyauth.Auth, key, fallback string) string {
	if auth == nil || auth.Metadata == nil {
		return fallback
	}
	value := auth.Metadata[key]
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	aliases := map[string]string{"access_token": "accessToken", "refresh_token": "refreshToken", "client_id": "clientId", "client_secret": "clientSecret", "profile_arn": "profileArn", "auth_method": "authMethod", "region": "region"}
	if alias, ok := aliases[key]; ok {
		if text, ok := auth.Metadata[alias].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return fallback
}

func executionConversationID(opts cliproxyexecutor.Options, req cliproxyexecutor.Request) string {
	for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
		for _, key := range []string{cliproxyexecutor.CanonicalSessionIDMetadataKey, cliproxyexecutor.ExecutionSessionMetadataKey, "conversation_id"} {
			if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func claudeSSE(event string, payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	return []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
}

type kiroEventAccumulator struct {
	model, text, thinking     string
	inputTokens, outputTokens int
	toolUses                  []map[string]any
	lastText                  string
	lastThinking              string
	textStarted               bool
}

func newKiroEventAccumulator(model string, requestPayload []byte) *kiroEventAccumulator {
	accumulator := &kiroEventAccumulator{model: model}
	// Kiro does not return prompt token counts. Estimate the same Claude
	// request that is sent upstream so message_start is useful to clients.
	if count, err := helps.CountClaudeInputTokens(requestPayload); err == nil {
		accumulator.inputTokens = int(count)
	}
	return accumulator
}
func (a *kiroEventAccumulator) consume(eventName string, data map[string]any) []byte {
	switch eventName {
	case "assistantResponseEvent":
		value, _ := data["content"].(string)
		delta := kiroDelta(value, &a.lastText)
		a.text += delta
		if delta != "" {
			payload := claudeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
			if !a.textStarted {
				a.textStarted = true
				start := claudeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				return append(start, payload...)
			}
			return payload
		}
	case "reasoningContentEvent":
		value, _ := data["text"].(string)
		delta := kiroDelta(value, &a.lastThinking)
		a.thinking += delta
		if delta != "" {
			return claudeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": delta}})
		}
	case "toolUseEvent":
		if name, _ := data["name"].(string); name != "" {
			input := data["input"]
			id, _ := data["toolUseId"].(string)
			for _, existing := range a.toolUses {
				if existing["id"] == id && existing["name"] == name {
					existing["input"] = input
					return nil
				}
			}
			a.toolUses = append(a.toolUses, map[string]any{"id": id, "name": name, "input": input})
		}
	case "meteringEvent":
		// Some Kiro variants expose token fields here. The standard runtime
		// instead exposes billing usage and contextUsagePercentage, so keep the
		// explicit token fields when they are present and estimate below when
		// they are absent.
		if value, ok := numberField(data, "outputTokens", "totalOutputTokens"); ok && value > 0 {
			a.outputTokens = value
		}
		if value, ok := numberField(data, "inputTokens", "totalInputTokens"); ok && value > 0 {
			a.inputTokens = value
		}
	}
	return nil
}
func (a *kiroEventAccumulator) finishSSE(model string) [][]byte {
	a.ensureOutputTokens()
	var out [][]byte
	index := 0
	if a.textStarted {
		out = append(out, claudeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
		index++
	}
	for _, tool := range a.toolUses {
		out = append(out, claudeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tool["id"], "name": tool["name"], "input": tool["input"]}}))
		out = append(out, claudeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
		index++
	}
	stop := "end_turn"
	if len(a.toolUses) > 0 {
		stop = "tool_use"
	}
	out = append(out, claudeSSE("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": a.outputTokens}}), claudeSSE("message_stop", map[string]any{"type": "message_stop"}))
	return out
}
func (a *kiroEventAccumulator) claudeResponse(model string) []byte {
	a.ensureOutputTokens()
	content := []map[string]any{}
	if a.text != "" {
		content = append(content, map[string]any{"type": "text", "text": a.text})
	}
	for _, tool := range a.toolUses {
		content = append(content, map[string]any{"type": "tool_use", "id": tool["id"], "name": tool["name"], "input": tool["input"]})
	}
	stop := "end_turn"
	if len(a.toolUses) > 0 {
		stop = "tool_use"
	}
	data, _ := json.Marshal(map[string]any{"id": "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": a.inputTokens, "output_tokens": a.outputTokens}})
	return data
}

func (a *kiroEventAccumulator) ensureOutputTokens() {
	if a.outputTokens > 0 {
		return
	}
	parts := make([]string, 0, 1+len(a.toolUses))
	if a.text != "" {
		parts = append(parts, a.text)
	}
	if a.thinking != "" {
		parts = append(parts, a.thinking)
	}
	for _, tool := range a.toolUses {
		if encoded, err := json.Marshal(tool); err == nil {
			parts = append(parts, string(encoded))
		}
	}
	if len(parts) == 0 {
		return
	}
	if encoder, err := helps.TokenizerForModel(a.model); err == nil {
		if count, err := encoder.Count(strings.Join(parts, "\n")); err == nil {
			a.outputTokens = count
			return
		}
	}
	// Keep usage non-zero even if tokenizer initialization fails. This is a
	// deliberately rough fallback, never a claim of provider-exact billing.
	joined := strings.Join(parts, "\n")
	a.outputTokens = (len([]rune(joined)) + 3) / 4
}

func kiroDelta(value string, previous *string) string {
	if value == "" {
		return ""
	}
	if *previous == "" {
		*previous = value
		return value
	}
	if strings.HasPrefix(value, *previous) {
		delta := value[len(*previous):]
		*previous = value
		return delta
	}
	if value == *previous {
		return ""
	}
	*previous = value
	return value
}
func numberField(data map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		switch value := data[key].(type) {
		case float64:
			return int(value), true
		case json.Number:
			n, err := value.Int64()
			return int(n), err == nil
		case int:
			return value, true
		}
	}
	if nested, ok := data["usage"].(map[string]any); ok {
		return numberField(nested, keys...)
	}
	return 0, false
}

func collectKiroEvents(body io.Reader, model string, requestPayload []byte) (*kiroEventAccumulator, error) {
	result := newKiroEventAccumulator(model, requestPayload)
	err := parseKiroEventStream(body, func(eventName string, data map[string]any) bool { result.consume(eventName, data); return true })
	return result, err
}

func parseKiroEventStream(body io.Reader, callback func(string, map[string]any) bool) error {
	for {
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		totalLength := int(binary.BigEndian.Uint32(prelude[:4]))
		headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))
		if totalLength < 16 || headersLength > totalLength-16 {
			return fmt.Errorf("invalid Kiro event frame lengths")
		}
		remaining := make([]byte, totalLength-12)
		if _, err = io.ReadFull(body, remaining); err != nil {
			return err
		}
		payload := remaining[headersLength : len(remaining)-4]
		eventName := kiroEventName(remaining[:headersLength])
		if len(payload) == 0 || eventName == "" {
			continue
		}
		var data map[string]any
		if json.Unmarshal(payload, &data) != nil {
			continue
		}
		if callback != nil && !callback(eventName, data) {
			return nil
		}
	}
}

func kiroEventName(headers []byte) string {
	for index := 0; index < len(headers); {
		if index >= len(headers) {
			break
		}
		nameLen := int(headers[index])
		index++
		if index+nameLen > len(headers) {
			break
		}
		name := string(headers[index : index+nameLen])
		index += nameLen
		if index >= len(headers) {
			break
		}
		valueType := headers[index]
		index++
		switch valueType {
		case 7:
			if index+2 > len(headers) {
				return ""
			}
			length := int(binary.BigEndian.Uint16(headers[index : index+2]))
			index += 2
			if index+length > len(headers) {
				return ""
			}
			value := string(headers[index : index+length])
			index += length
			if name == ":event-type" || name == "event-type" {
				return value
			}
		default:
			index = skipKiroHeaderValue(headers, index, valueType)
		}
	}
	return ""
}
func skipKiroHeaderValue(headers []byte, index int, valueType byte) int {
	sizes := map[byte]int{0: 0, 1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 16}
	if size, ok := sizes[valueType]; ok {
		return index + size
	}
	if valueType == 8 || valueType == 9 {
		if index+2 > len(headers) {
			return len(headers)
		}
		return index + 2 + int(binary.BigEndian.Uint16(headers[index:index+2]))
	}
	return len(headers)
}
