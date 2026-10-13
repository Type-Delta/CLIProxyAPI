package executor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

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
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
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
