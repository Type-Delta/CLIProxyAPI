package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestLocalLatencySeparatesHeadersTokensAndLegacyFallback(t *testing.T) {
	receivedAt := time.Now()
	reporter := NewUsageReporter(usage.WithRequestReceivedAt(context.Background(), receivedAt), "openai", "model", nil)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `{"choices":[{"delta":{"content":"hello"}}]}`)
	}))
	defer server.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	client := &http.Client{Transport: usageTTFTRoundTripper{base: http.DefaultTransport, reporter: reporter}}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	record := reporter.buildRecord(usage.Detail{}, false)
	if record.RoutingTime == nil || *record.RoutingTime != record.UpstreamSentAt.Sub(receivedAt) {
		t.Fatalf("HTTP routing observation=%+v", record)
	}
	if record.ProviderLatency == nil || *record.ProviderLatency < 0 || record.FirstTokenLatency != nil {
		t.Fatalf("header observation=%+v", record)
	}
	provider := *record.ProviderLatency
	// Metadata preserves existing TTFT fallback but cannot count as a token observation.
	ObserveChatTokenEvent(reporter, []byte(`{"error":{"message":"waiting"}}`))
	if !reporter.IsTTFTSet() || reporter.buildRecord(usage.Detail{}, false).FirstTokenLatency != nil {
		t.Fatal("fallback became strict token latency")
	}
	releaseOnce.Do(func() { close(release) })
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	ObserveChatTokenEvent(reporter, body)
	record = reporter.buildRecord(usage.Detail{}, false)
	if record.FirstTokenLatency == nil || *record.FirstTokenLatency < provider {
		t.Fatalf("token observation=%+v", record)
	}
	first := *record.FirstTokenLatency
	reporter.ObserveUpstreamResponse()
	ObserveChatTokenEvent(reporter, []byte(`[DONE]`))
	if *reporter.buildRecord(usage.Detail{}, false).FirstTokenLatency != first || *reporter.buildRecord(usage.Detail{}, false).ProviderLatency != provider {
		t.Fatal("later frames overwrote first arrivals")
	}
}

func TestLocalLatencyDispatchResetsAndMissingStaysNull(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "model", nil)
	reporter.ObserveUpstreamResponse()
	reporter.ObserveGenerationToken()
	if reporter.buildRecord(usage.Detail{}, false).ProviderLatency != nil || reporter.buildRecord(usage.Detail{}, false).FirstTokenLatency != nil {
		t.Fatal("missing dispatch fabricated timing")
	}
	reporter.StartUpstreamTiming()
	reporter.dispatchAt = time.Now().Add(-time.Hour)
	reporter.ObserveUpstreamResponse()
	reporter.ObserveGenerationToken()
	if *reporter.buildRecord(usage.Detail{}, false).ProviderLatency < time.Hour {
		t.Fatal("test did not observe first dispatch")
	}
	reporter.StartUpstreamTiming()
	if reporter.buildRecord(usage.Detail{}, false).ProviderLatency != nil || reporter.buildRecord(usage.Detail{}, false).FirstTokenLatency != nil {
		t.Fatal("retry retained previous observations")
	}
	reporter.ObserveUpstreamResponse()
	if *reporter.buildRecord(usage.Detail{}, false).ProviderLatency >= time.Hour {
		t.Fatal("reused connection retained old dispatch")
	}
	// Returned records must not retain mutable reporter timing pointers.
	value := reporter.buildRecord(usage.Detail{}, false).ProviderLatency
	*value = time.Hour
	if *reporter.buildRecord(usage.Detail{}, false).ProviderLatency == time.Hour {
		t.Fatal("observation aliases reporter state")
	}
}

func TestLocalLatencyTransportFailureHasNoResponseObservation(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "openai", "model", nil)
	request, _ := http.NewRequest(http.MethodGet, "http://example.invalid", strings.NewReader(""))
	transport := usageTTFTRoundTripper{base: localLatencyFailedTransport{}, reporter: reporter}
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("expected injected error")
	}
	if reporter.buildRecord(usage.Detail{}, false).ProviderLatency != nil || reporter.buildRecord(usage.Detail{}, false).FirstTokenLatency != nil {
		t.Fatal("transport failure fabricated observations")
	}
}

type localLatencyFailedTransport struct{}

func (localLatencyFailedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestDispatchTimestampMatchesLocalLatencyOrigin(t *testing.T) {
	for _, provider := range []string{"codex", "xai", "openai"} {
		t.Run(provider, func(t *testing.T) {
			reporter := NewUsageReporter(context.Background(), provider, "model", nil)
			if provider == "openai" {
				req, _ := http.NewRequest(http.MethodPost, "https://example.com/first", nil)
				reporter.recordUpstreamRequest(req, time.Unix(100, 0))
			}
			reporter.StartUpstreamTiming()
			record := reporter.buildRecord(usage.Detail{}, false)
			if !record.UpstreamSentAt.Equal(reporter.dispatchAt) {
				t.Fatalf("dispatch timestamp %v does not match latency origin %v", record.UpstreamSentAt, reporter.dispatchAt)
			}
		})
	}
}

func TestRoutingObservationSharedAcrossDispatchesAndReporters(t *testing.T) {
	received := time.Unix(100, 0)
	ctx := usage.WithRequestReceivedAt(context.Background(), received)
	// Seed the first dispatch explicitly to avoid wall-clock ordering assumptions.
	usage.ObserveRequestRouting(ctx, received.Add(12*time.Millisecond))
	for range 2 {
		reporter := NewUsageReporter(ctx, "codex", "model", nil)
		for range 2 {
			reporter.StartUpstreamTiming()
			record := reporter.buildRecord(usage.Detail{}, false)
			if record.RoutingTime == nil || *record.RoutingTime != 12*time.Millisecond {
				t.Fatalf("retry included earlier provider wait in routing: %+v", record.RoutingTime)
			}
			*record.RoutingTime = time.Hour
		}
	}
	reporter := NewUsageReporter(context.Background(), "codex", "model", nil)
	reporter.StartUpstreamTiming()
	if reporter.buildRecord(usage.Detail{}, false).RoutingTime != nil {
		t.Fatal("unknown request receipt fabricated routing")
	}
}
