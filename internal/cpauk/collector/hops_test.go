package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newHopSanitizer() *Sanitizer {
	return NewSanitizer(SanitizerOptions{IdentityKey: [32]byte{7}, StoreCredential: true})
}

func TestClassifyEndpointAcceptsMiddlewareClasses(t *testing.T) {
	for _, class := range []string{"chat_completions", "responses", "gemini_generate", "gemini_stream_generate", "models", "other", "moderations"} {
		if got := classifyEndpoint(class); got != class {
			t.Fatalf("classifyEndpoint(%q) = %q", class, got)
		}
	}
	if got := classifyEndpoint("/private/customer"); got != unknownValue {
		t.Fatalf("raw path classified as %q", got)
	}
}

func TestSanitizerStoresHopDiagnosticsAndSuccessStatus(t *testing.T) {
	record := validRecord()
	record.UpstreamStatusCode = http.StatusOK
	record.ClientMethod = http.MethodPost
	record.ClientPath = "/v1/messages?stream=true"
	record.ReceivedAt = time.Date(2026, 9, 9, 1, 0, 0, 0, time.FixedZone("BKK", 7*3600))
	record.UpstreamMethod = http.MethodPost
	record.UpstreamURL = "https://api.example.com/v1/messages?key=secret"
	record.UpstreamSentAt = record.ReceivedAt.Add(80 * time.Millisecond)
	record.Detail.RawUsage = `{"input_tokens":70,"output_tokens":30}`
	record.Fail.Body = "not stored on success"

	result, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	event := result.Event
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	if event.UpstreamStatusCode == nil || *event.UpstreamStatusCode != http.StatusOK {
		t.Fatalf("success status not stored: %v", event.UpstreamStatusCode)
	}
	if event.ClientPath == nil || *event.ClientPath != "/v1/messages" || event.UpstreamURL == nil || *event.UpstreamURL != "https://api.example.com/v1/messages" {
		t.Fatalf("query strings leaked: %v %v", event.ClientPath, event.UpstreamURL)
	}
	if event.ReceivedAt == nil || event.ReceivedAt.Location() != time.UTC || event.UpstreamSentAt == nil {
		t.Fatalf("timestamps = %v %v", event.ReceivedAt, event.UpstreamSentAt)
	}
	if event.UpstreamErrorBody != nil {
		t.Fatal("error body stored for a successful attempt")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"upstream_usage_raw":{"input_tokens":70,"output_tokens":30}`) {
		t.Fatalf("raw usage not emitted as JSON: %s", encoded)
	}
}

func TestSanitizerBoundsFailureBodyWithMarker(t *testing.T) {
	record := validRecord()
	record.Failed = true
	record.Fail = coreusage.Failure{StatusCode: 429, Body: strings.Repeat("e", model.MaxRawPayloadBytes+100)}
	result, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	body := result.Event.UpstreamErrorBody
	if body == nil || len(*body) != model.MaxRawPayloadBytes || !strings.HasSuffix(*body, truncationMarker) {
		t.Fatalf("error body = %v", body)
	}
	if result.Event.UpstreamStatusCode == nil || *result.Event.UpstreamStatusCode != 429 || result.TruncatedFields == 0 {
		t.Fatalf("status = %v truncated = %d", result.Event.UpstreamStatusCode, result.TruncatedFields)
	}
}

type patchRecordingWriter struct {
	events  []model.Event
	patches []ProxyResponsePatch
	done    chan struct{}
}

func (w *patchRecordingWriter) WriteBatch(context.Context, []model.Event) error { return nil }

func (w *patchRecordingWriter) WriteBatchWithPatches(_ context.Context, events []model.Event, patches []ProxyResponsePatch) error {
	w.events = append(w.events, events...)
	w.patches = append(w.patches, patches...)
	if len(w.patches) > 0 {
		select {
		case <-w.done:
		default:
			close(w.done)
		}
	}
	return nil
}

func TestProxyResponsePatchRidesTheSameQueueAfterItsEvent(t *testing.T) {
	writer := &patchRecordingWriter{done: make(chan struct{})}
	c, err := New(writer, Options{Capacity: 16, BatchSize: 8, FlushInterval: 10 * time.Millisecond, FailureThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	c.Start()
	defer func() { _ = c.Close(context.Background()) }()
	adapter := NewAdapter(c, newHopSanitizer())
	adapter.HandleUsage(context.Background(), validRecord())
	adapter.HandleProxyResponse(coreusage.ProxyResponse{ProxyRequestID: "d1371f43e6b8362d05d7567ed5fcc2ad", StatusCode: 200, RespondedAt: time.Now()})
	adapter.HandleProxyResponse(coreusage.ProxyResponse{ProxyRequestID: "not-an-id", StatusCode: 200})

	select {
	case <-writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("patch was never written")
	}
	if len(writer.events) != 1 || len(writer.patches) != 1 || writer.patches[0].ProxyRequestID != "d1371f43e6b8362d05d7567ed5fcc2ad" {
		t.Fatalf("events=%d patches=%+v", len(writer.events), writer.patches)
	}
}

func TestSanitizerClassifiesAuthSelectionFailureAsCPASide(t *testing.T) {
	record := validRecord()
	record.ExecutorType = "auth-selection"
	record.Failed = true
	record.Fail = coreusage.Failure{StatusCode: 429, Body: "auth_unavailable: no auth available"}
	result, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	event := result.Event
	if event.ErrorClass == nil || *event.ErrorClass != "auth_unavailable" {
		t.Fatalf("error class = %v", event.ErrorClass)
	}
	if event.UpstreamStatusCode != nil || event.UpstreamErrorBody != nil || event.UpstreamSentAt != nil {
		t.Fatalf("auth selection failure attributed to the provider: %+v", event)
	}
}

func TestSanitizerPreservesLargeGenerationDiagnostics(t *testing.T) {
	record := validRecord()
	record.Detail.RawUsage = `{"usage":{"input_tokens":123},"telemetry":"` + strings.Repeat("x", 12000) + `"}`
	result, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.UpstreamUsageRaw == nil || string(*result.Event.UpstreamUsageRaw) != coreusage.BoundRawUsage(record.Detail.RawUsage) {
		t.Fatal("large generation diagnostics were lost")
	}
	if err := result.Event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationDiagnosticsEnvelopeAllowsEscaping(t *testing.T) {
	record := validRecord()
	record.Failed = true
	record.Fail.Body = strings.Repeat("<", 4096)
	record.Detail.RawUsage = `{"usage":{"total_tokens":123},"telemetry":"` + strings.Repeat("x", 40000) + `"}`
	record.Model = strings.Repeat("<", 256)
	result, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Event.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Event.UpstreamUsageRaw == nil || len(*result.Event.UpstreamUsageRaw) < 40000 {
		t.Fatal("generation was truncated early")
	}
	if result.Event.UpstreamErrorBody == nil || len(*result.Event.UpstreamErrorBody) != 4096 {
		t.Fatal("error cap changed")
	}
}
