package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKiroExecutorUsesDiscoveredContextUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				capture := newKiroUsageCapture(t, t.Name())
				var catalogCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/ListAvailableModels" {
						catalogCalls.Add(1)
						if r.Method != http.MethodGet || r.URL.Query().Get("origin") != "AI_EDITOR" {
							t.Errorf("catalog request = %s %s", r.Method, r.URL)
						}
						if r.Header.Get("X-Amz-Target") != "" || r.Header.Get("X-Amzn-Kiro-Agent-Mode") != "" {
							t.Error("catalog request contains inference-only headers")
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-opus-5.5","tokenLimits":{"maxInputTokens":800000,"maxOutputTokens":64000}}]}`)
						return
					}
					writeKiroTestEvents(t, w, []kiroTestEvent{
						{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}},
						{name: "contextUsageEvent", payload: map[string]any{"contextUsagePercentage": 25.0}},
					})
				}))
				t.Cleanup(server.Close)
				executor := NewKiroExecutor(&config.Config{})
				request := cliproxyexecutor.Request{Model: "claude-opus-5-5(high)", Payload: []byte(`{"model":"claude-opus-5-5(high)","messages":[{"role":"user","content":"Reply OK"}]}`)}
				if format == sdktranslator.FormatOpenAIResponse {
					request.Payload = []byte(`{"model":"claude-opus-5-5(high)","input":"Reply OK"}`)
				}
				opts := cliproxyexecutor.Options{SourceFormat: format, Stream: stream}
				ctx := kiroUsageTestContext(server, t.Name())
				var body []byte
				if stream {
					result, errStream := executor.ExecuteStream(ctx, kiroUsageTestAuth(), request, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						body = append(body, chunk.Payload...)
						body = append(body, '\n')
					}
				} else {
					result, errExecute := executor.Execute(ctx, kiroUsageTestAuth(), request, opts)
					if errExecute != nil {
						t.Fatal(errExecute)
					}
					body = result.Payload
				}
				record := capture.await(t)
				if record.Detail.TotalTokens != 200000 || record.Detail.InputTokens+record.Detail.OutputTokens != 200000 {
					t.Fatalf("usage = %+v, want 25%% of the discovered 800000-token window", record.Detail)
				}
				if record.Detail.OutputTokens == 0 || record.Detail.TokenQuality != coreusage.TokenQualityEstimated {
					t.Fatalf("usage lost output estimation: %+v", record.Detail)
				}
				if !strings.HasSuffix(record.UpstreamURL, "/generateAssistantResponse") {
					t.Fatalf("catalog traffic replaced inference diagnostics: upstream=%q", record.UpstreamURL)
				}
				if catalogCalls.Load() != 1 {
					t.Fatalf("catalog calls = %d, want 1", catalogCalls.Load())
				}
				usage := gjson.GetBytes(body, "usage")
				if stream {
					for _, line := range bytes.Split(body, []byte("\n")) {
						payload := bytes.TrimPrefix(line, []byte("data: "))
						if gjson.GetBytes(payload, "usage.input_tokens").Exists() || gjson.GetBytes(payload, "usage.prompt_tokens").Exists() {
							usage = gjson.GetBytes(payload, "usage")
						}
						if gjson.GetBytes(payload, "type").String() == "response.completed" {
							usage = gjson.GetBytes(payload, "response.usage")
						}
					}
				}
				input := usage.Get("input_tokens").Int() + usage.Get("prompt_tokens").Int()
				output := usage.Get("output_tokens").Int() + usage.Get("completion_tokens").Int()
				if input != record.Detail.InputTokens || output != record.Detail.OutputTokens {
					t.Fatalf("client usage %s differs from recorded usage %+v", usage.Raw, record.Detail)
				}
			})
		}
	}
}

func TestKiroExecutorUnknownContextLimitsKeepLocalEstimate(t *testing.T) {
	for _, catalog := range []string{`{"models":[]}`, `{"models":[{"modelId":"another-model","tokenLimits":{"maxInputTokens":800000}}]}`, `invalid`} {
		t.Run(catalog, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ListAvailableModels" {
					_, _ = io.WriteString(w, catalog)
					return
				}
				writeKiroTestEvents(t, w, []kiroTestEvent{
					{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}},
					{name: "contextUsageEvent", payload: map[string]any{"contextUsagePercentage": 25.0}},
				})
			}))
			t.Cleanup(server.Close)
			payload := []byte(`{"messages":[{"role":"user","content":"Reply OK"}]}`)
			want, errCount := helps.CountClaudeInputTokens(payload)
			if errCount != nil {
				t.Fatal(errCount)
			}
			resp, errExecute := NewKiroExecutor(nil).Execute(kiroUsageTestContext(server, t.Name()), kiroUsageTestAuth(), cliproxyexecutor.Request{
				Model: "claude-opus-5-5", Payload: payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if errExecute != nil {
				t.Fatal(errExecute)
			}
			if got := gjson.GetBytes(resp.Payload, "usage.input_tokens").Int(); got != want {
				t.Fatalf("unknown window changed input usage from %d to %d", want, got)
			}
		})
	}
}

func TestKiroCountTokensUsesInferenceEstimator(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5-5","max_tokens":64000,"metadata":{"irrelevant":"` + strings.Repeat("metadata", 100) + `"},"messages":[{"role":"user","content":"Reply OK"}]}`)
	want, errCount := helps.CountClaudeInputTokens(payload)
	if errCount != nil {
		t.Fatal(errCount)
	}
	resp, errCount := NewKiroExecutor(nil).CountTokens(context.Background(), nil, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errCount != nil {
		t.Fatal(errCount)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != want {
		t.Fatalf("input_tokens = %d, want inference estimate %d", got, want)
	}
}

func TestKiroContextWindowUsesConfiguredWireModelAndProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ListAvailableModels" {
			if r.URL.Query().Get("profileArn") != "configured-profile" {
				t.Errorf("catalog profile = %q, want configured profile", r.URL.Query().Get("profileArn"))
			}
			_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-opus-5.5","tokenLimits":{"maxInputTokens":800000}},{"modelId":"claude-sonnet-4.5","tokenLimits":{"maxInputTokens":320000}}]}`)
			return
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Error(errRead)
		}
		if gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String() != "claude-sonnet-4.5" || gjson.GetBytes(body, "profileArn").String() != "configured-profile" {
			t.Errorf("configured wire fields overwritten: %s", body)
		}
		writeKiroTestEvents(t, w, []kiroTestEvent{
			{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}},
			{name: "contextUsageEvent", payload: map[string]any{"contextUsagePercentage": 25.0}},
		})
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "claude-opus-*"}},
		Params: map[string]any{"conversationState.currentMessage.userInputMessage.modelId": "claude-sonnet-4.5", "profileArn": "configured-profile"},
	}}}}
	resp, errExecute := NewKiroExecutor(cfg).Execute(kiroUsageTestContext(server, t.Name()), kiroUsageTestAuth(), cliproxyexecutor.Request{
		Model: "claude-opus-5-5", Payload: []byte(`{"messages":[{"role":"user","content":"Reply OK"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if total := gjson.GetBytes(resp.Payload, "usage.input_tokens").Int() + gjson.GetBytes(resp.Payload, "usage.output_tokens").Int(); total != 80000 {
		t.Fatalf("total usage = %d, want 25%% of configured model's 320000-token window", total)
	}
}

func TestKiroContextCorrectionDoesNotBufferContent(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ListAvailableModels" {
			_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-opus-5.5","tokenLimits":{"maxInputTokens":800000}}]}`)
			return
		}
		writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}}})
		w.(http.Flusher).Flush()
		<-release
		writeKiroTestEvents(t, w, []kiroTestEvent{{name: "contextUsageEvent", payload: map[string]any{"contextUsagePercentage": 25.0}}})
	}))
	t.Cleanup(server.Close)
	defer close(release)
	result, errStream := NewKiroExecutor(nil).ExecuteStream(kiroUsageTestContext(server, t.Name()), kiroUsageTestAuth(), cliproxyexecutor.Request{
		Model: "claude-opus-5-5", Payload: []byte(`{"messages":[{"role":"user","content":"Reply OK"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
	if errStream != nil {
		t.Fatal(errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if bytes.Contains(chunk.Payload, []byte(`"text":"OK"`)) {
			return
		}
	}
	t.Fatal("no content arrived before terminal context metadata")
}

func TestKiroExecutorExecutePublishesUsage(t *testing.T) {
	const alias = "kiro-execute-usage"
	capture := newKiroUsageCapture(t, alias)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		writeKiroTestEvents(t, w, []kiroTestEvent{
			{name: "assistantResponseEvent", payload: map[string]any{"content": "hello from Kiro"}},
		})
	}))
	t.Cleanup(server.Close)

	executor := NewKiroExecutor(&config.Config{})
	ctx := kiroUsageTestContext(server, alias)
	resp, errExecute := executor.Execute(ctx, kiroUsageTestAuth(), cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Count this request"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute returned an empty response")
	}

	record := capture.await(t)
	assertKiroUsageRecord(t, record)
	if record.Stream || !record.StreamKnown {
		t.Fatal("nonstream client execution inherited upstream EventStream mode")
	}
	if record.FirstTokenLatency == nil || record.GenerationTime == nil {
		t.Fatalf("Kiro content has no source timing: first=%v generation=%v", record.FirstTokenLatency, record.GenerationTime)
	}
}

func TestKiroExecutorExecuteStreamPublishesUsageOnce(t *testing.T) {
	const alias = "kiro-stream-usage"
	capture := newKiroUsageCapture(t, alias)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeKiroTestEvents(t, w, []kiroTestEvent{
			{name: "assistantResponseEvent", payload: map[string]any{"content": "hello from Kiro"}},
		})
	}))
	t.Cleanup(server.Close)

	executor := NewKiroExecutor(&config.Config{})
	ctx := kiroUsageTestContext(server, alias)
	result, errStream := executor.ExecuteStream(ctx, kiroUsageTestAuth(), cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Count this request"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	record := capture.await(t)
	assertKiroUsageRecord(t, record)
	if !record.Stream || !record.StreamKnown {
		t.Fatal("direct streaming execution lost client stream mode")
	}
	if record.FirstTokenLatency == nil || record.GenerationTime == nil {
		t.Fatalf("Kiro content has no source timing: first=%v generation=%v", record.FirstTokenLatency, record.GenerationTime)
	}
	select {
	case extra := <-capture.records:
		t.Fatalf("unexpected second usage record: %+v", extra)
	default:
	}
}

func TestKiroExecutorTracks429Failure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"limited"}`))
	}))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name string
		run  func(context.Context, *KiroExecutor, cliproxyexecutor.Request) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, executor *KiroExecutor, request cliproxyexecutor.Request) error {
				_, errExecute := executor.Execute(ctx, kiroUsageTestAuth(), request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
				return errExecute
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, executor *KiroExecutor, request cliproxyexecutor.Request) error {
				_, errStream := executor.ExecuteStream(ctx, kiroUsageTestAuth(), request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
				return errStream
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newKiroUsageCapture(t, t.Name())
			ctx := kiroUsageTestContext(server, t.Name())
			request := cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-6",
				Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Count this request"}]}`),
			}
			if errUpstream := tc.run(ctx, NewKiroExecutor(&config.Config{}), request); errUpstream == nil {
				t.Fatal("executor returned nil error for a 429 response")
			}

			record := capture.await(t)
			if record.Provider != "kiro" {
				t.Fatalf("Provider = %q, want kiro", record.Provider)
			}
			if !record.Failed {
				t.Fatal("429 usage record is not marked failed")
			}
			if record.Fail.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("failure status = %d, want %d", record.Fail.StatusCode, http.StatusTooManyRequests)
			}
		})
	}
}

type kiroTestEvent struct {
	name    string
	payload map[string]any
}

func writeKiroTestEvents(t *testing.T, w http.ResponseWriter, events []kiroTestEvent) {
	t.Helper()
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	for _, event := range events {
		frame := kiroTestEventFrame(t, event.name, event.payload)
		if _, errWrite := w.Write(frame); errWrite != nil {
			t.Errorf("write Kiro event: %v", errWrite)
			return
		}
	}
}

func kiroTestEventFrame(t *testing.T, eventName string, payload map[string]any) []byte {
	t.Helper()
	data, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal Kiro event payload: %v", errMarshal)
	}
	headerName := ":event-type"
	if len(headerName) > 255 || len(eventName) > 65535 {
		t.Fatalf("Kiro event header is too large")
	}
	headers := make([]byte, 0, 1+len(headerName)+1+2+len(eventName))
	headers = append(headers, byte(len(headerName)))
	headers = append(headers, headerName...)
	headers = append(headers, 7)
	var valueLength [2]byte
	binary.BigEndian.PutUint16(valueLength[:], uint16(len(eventName)))
	headers = append(headers, valueLength[:]...)
	headers = append(headers, eventName...)

	totalLength := 12 + len(headers) + len(data) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers...)
	frame = append(frame, data...)
	messageCRC := crc32.ChecksumIEEE(frame)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], messageCRC)
	frame = append(frame, crc[:]...)
	return frame
}

func kiroUsageTestContext(server *httptest.Server, alias string) context.Context {
	serverURL, _ := url.Parse(server.URL)
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = serverURL.Scheme
		req.URL.Host = serverURL.Host
		return server.Client().Transport.RoundTrip(req)
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	return coreusage.WithRequestedModelAlias(ctx, alias)
}

func kiroUsageTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "kiro-test-auth",
		Provider: "kiro",
		Metadata: map[string]any{"access_token": "test-token"},
	}
}

func newKiroUsageCapture(t *testing.T, alias string) *multiProviderUsageCapture {
	t.Helper()
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		_ = coreusage.DefaultManager().UnregisterNamed(context.Background(), t.Name())
	})
	return capture
}

func assertKiroUsageRecord(t *testing.T, record coreusage.Record) {
	t.Helper()
	if record.Provider != "kiro" {
		t.Fatalf("Provider = %q, want kiro", record.Provider)
	}
	if record.Model != "claude-sonnet-4-6" {
		t.Fatalf("Model = %q, want claude-sonnet-4-6", record.Model)
	}
	if record.Failed {
		t.Fatalf("usage record marked failed: %s", record.Fail.Body)
	}
	if record.Detail.InputTokens <= 0 {
		t.Fatalf("InputTokens = %d, want non-zero", record.Detail.InputTokens)
	}
	if record.Detail.OutputTokens <= 0 {
		t.Fatalf("OutputTokens = %d, want non-zero", record.Detail.OutputTokens)
	}
	if strings.TrimSpace(record.Fail.Body) != "" {
		t.Fatalf("unexpected failure body: %q", record.Fail.Body)
	}
}

func TestKiroBinarySourceTimingIncludesSuppressedReasoningAndIgnoresMetadata(t *testing.T) {
	for _, tc := range []struct {
		name      string
		events    []kiroTestEvent
		wantCount int64
	}{
		{"reasoning", []kiroTestEvent{{name: "reasoningContentEvent", payload: map[string]any{"text": "suppressed reasoning"}}, {name: "meteringEvent", payload: map[string]any{"outputTokens": 9999}}}, 1},
		{"tool", []kiroTestEvent{{name: "toolUseEvent", payload: map[string]any{"toolUseId": "tool-1", "name": "run", "input": "{}"}}, {name: "toolUseEvent", payload: map[string]any{"toolUseId": "tool-1", "stop": true}}}, 1},
		{"metadata", []kiroTestEvent{{name: "meteringEvent", payload: map[string]any{"outputTokens": 9999}}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newKiroUsageCapture(t, t.Name())
			ctx := coreusage.WithRequestedModelAlias(context.Background(), t.Name())
			r := helps.NewUsageReporter(ctx, "kiro", "claude-sonnet-4-6", nil)
			r.StartUpstreamTiming()
			var raw []byte
			for _, event := range tc.events {
				raw = append(raw, kiroTestEventFrame(t, event.name, event.payload)...)
			}
			body := trackKiroGenerationBody(ctx, r, io.NopCloser(bytes.NewReader(raw)))
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, got) {
				t.Fatal("binary observer changed source bytes")
			}
			r.Publish(ctx, coreusage.Detail{OutputTokens: 9999})
			record := capture.await(t)
			timing := record.OutputTiming
			if timing == nil || timing.Protocol != "kiro" || timing.Source != "source_reader" || timing.Coverage != "wire_deltas" || timing.ArrivalCount != tc.wantCount {
				t.Fatalf("binary source evidence = %+v", timing)
			}
			if timing.Status != "unavailable" || timing.Reason != "insufficient_arrivals" {
				t.Fatalf("single read fabricated interval: %+v", timing)
			}
			if (record.FirstTokenLatency != nil) != (tc.wantCount > 0) {
				t.Fatalf("substantive source timing = %v", record.FirstTokenLatency)
			}
		})
	}
}

func TestKiroExecutorCreditSnapshotsAndFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/fail=%t", stream, fail), func(t *testing.T) {
				capture := newKiroUsageCapture(t, t.Name())
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeKiroTestEvents(t, w, []kiroTestEvent{
						{name: "meteringEvent", payload: map[string]any{"unit": "CREDIT", "usage": json.Number("1.234567890123456789")}},
						{name: "meteringEvent", payload: map[string]any{"unit": "tokens", "usage": 99}},
						{name: "meteringEvent", payload: map[string]any{"unit": "credit", "usage": -1}},
						{name: "meteringEvent", payload: map[string]any{"unit": "credit", "usage": json.Number("1e-18")}},
						{name: "meteringEvent", payload: map[string]any{"unit": "credit", "usage": json.Number("1e-19")}},
						{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}},
					})
					if fail {
						_, _ = w.Write([]byte("truncated"))
					}
				}))
				defer server.Close()
				executor := NewKiroExecutor(&config.Config{})
				request := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: stream}
				ctx := kiroUsageTestContext(server, t.Name())
				var err error
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, err = executor.ExecuteStream(ctx, kiroUsageTestAuth(), request, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = executor.Execute(ctx, kiroUsageTestAuth(), request, opts)
				}
				if (err != nil) != fail {
					t.Fatalf("error=%v, fail=%t", err, fail)
				}
				record := capture.await(t)
				if record.Detail.ProviderCredits != "0.000000000000000001" || record.Failed != fail {
					t.Fatalf("record: %+v", record)
				}
				if record.Detail.CacheReadTokens != 0 || record.Detail.CacheCreationTokens != 0 || record.Detail.TokenQuality != coreusage.TokenQualityEstimated {
					t.Fatalf("usage: %+v", record.Detail)
				}
			})
		}
	}
}

func TestKiroMeteringZeroAndNumericTokenFields(t *testing.T) {
	events := []kiroTestEvent{
		{name: "meteringEvent", payload: map[string]any{"unit": "CREDIT", "usage": json.Number("2.1"), "contextUsagePercentage": json.Number("2.5"), "inputTokens": json.Number("1e2"), "outputTokens": json.Number("2.0")}},
		{name: "meteringEvent", payload: map[string]any{"unit": "CREDIT", "usage": json.Number("0.00")}},
		{name: "meteringEvent", payload: map[string]any{"usage": json.Number("4")}},
		{name: "metadataEvent", payload: map[string]any{"unit": "CREDIT", "usage": json.Number("5")}},
	}
	var frames []byte
	for _, event := range events {
		frames = append(frames, kiroTestEventFrame(t, event.name, event.payload)...)
	}
	result, err := collectKiroEvents(bytes.NewReader(frames), "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	detail := result.usageDetail()
	if detail.ProviderCredits != "0" || detail.InputTokens != 100 || detail.OutputTokens != 2 || result.contextUsagePercentage == nil || *result.contextUsagePercentage != 2.5 {
		t.Fatalf("detail=%+v percentage=%v", detail, result.contextUsagePercentage)
	}
}
