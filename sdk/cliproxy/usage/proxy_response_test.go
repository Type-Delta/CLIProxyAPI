package usage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPublishProxyResponseFillsIDBoundsErrorAndSurvivesPanics(t *testing.T) {
	var observed []ProxyResponse
	SetProxyResponseObserver(func(response ProxyResponse) {
		observed = append(observed, response)
		panic("observer panic must not escape")
	})
	defer SetProxyResponseObserver(nil)

	PublishProxyResponse(context.Background(), ProxyResponse{StatusCode: 200})
	if len(observed) != 0 {
		t.Fatalf("response without a correlation ID was delivered: %+v", observed)
	}
	ctx := WithProxyRequestID(context.Background(), "d1371f43e6b8362d05d7567ed5fcc2ad")
	PublishProxyResponse(ctx, ProxyResponse{StatusCode: 502, Error: strings.Repeat("x", MaxProxyErrorBytes+10)})
	if len(observed) != 1 {
		t.Fatalf("observed %d responses, want 1", len(observed))
	}
	if observed[0].ProxyRequestID != "d1371f43e6b8362d05d7567ed5fcc2ad" || len(observed[0].Error) != MaxProxyErrorBytes || observed[0].RespondedAt.IsZero() {
		t.Fatalf("response = %+v", observed[0])
	}
}

func TestPublishFillsHopMetadataFromContext(t *testing.T) {
	receivedAt := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	ctx := WithClientRequestLine(context.Background(), "POST", "/v1/messages")
	ctx = WithRequestReceivedAt(ctx, receivedAt)
	record := fillHopMetadata(ctx, Record{UpstreamURL: "https://api.example.com/v1/messages", Detail: Detail{RawUsage: strings.Repeat("{", MaxRawUsageBytes+1)}})
	if record.ClientMethod != "POST" || record.ClientPath != "/v1/messages" || !record.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("client hop = %+v", record)
	}
	if len(record.Detail.RawUsage) != MaxRawUsageBytes {
		t.Fatalf("raw usage length = %d", len(record.Detail.RawUsage))
	}
}
