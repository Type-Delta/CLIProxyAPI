package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestRequestsStreamUsesClientIntent(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	st, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(st, t.TempDir(), 0, 30)
	for _, tc := range []struct {
		name, path, body, transport, timeline string
		status                                int
		responseStream, want                  bool
	}{
		{name: "chat stream rejected as JSON", path: "/v1/chat/completions", body: `{"model":"m","stream":true,"messages":[]}`, status: 403, want: true},
		{name: "responses stream rejected as JSON", path: "/v1/responses", body: `{"model":"m","stream":true,"input":[]}`, status: 403, want: true},
		{name: "claude stream rejected as JSON", path: "/v1/messages", body: `{"model":"m","stream":true,"messages":[]}`, status: 403, want: true},
		{name: "non streaming", path: "/v1/messages", body: `{"model":"m","stream":false,"messages":[]}`, status: 200},
		{name: "successful streaming", path: "/v1/messages", body: `{"model":"m","stream":true,"messages":[]}`, status: 200, responseStream: true, want: true},
		{name: "explicit false overrides response", path: "/v1/responses", body: `{"model":"m","stream":false,"input":[]}`, status: 200, responseStream: true},
		{name: "omitted stream", path: "/v1/completions", body: `{"model":"m","prompt":"hi"}`, status: 403},
		{name: "completions stream", path: "/v1/completions", body: `{"model":"m","stream":true,"prompt":"hi"}`, status: 403, want: true},
		{name: "gemini streaming action", path: "/v1beta/models/m:streamGenerateContent", body: `{"contents":[]}`, status: 403, want: true},
		{name: "gemini SSE query", path: "/v1beta/models/m:generateContent?alt=sse", body: `{"contents":[]}`, status: 403, want: true},
		{name: "gemini action overrides body", path: "/v1beta/models/m:streamGenerateContent", body: `{"stream":false,"contents":[]}`, status: 403, want: true},
		{name: "gemini JSON query", path: "/v1beta/models/m:generateContent?alt=json", body: `{"contents":[]}`, status: 200},
		{name: "gemini interactions", path: "/v1beta/interactions", body: `{"model":"m","stream":true,"input":"hi"}`, status: 403, want: true},
		{name: "gemini non streaming", path: "/v1beta/models/m:generateContent", body: `{"contents":[]}`, status: 200},
		{name: "websocket explicit false still streams", path: "/v1/responses", transport: record.TransportWebsocket, body: `{"stream":false,"input":[]}`, status: 403, want: true},
		{name: "websocket failed before response", path: "/v1/responses", transport: record.TransportWebsocket, timeline: `{"event":"request","payload":"{\"type\":\"response.create\",\"model\":\"m\",\"input\":[]}"}`, status: 403, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &record.Request{RequestID: tc.name, StartedAt: time.Now().UTC(), URL: tc.path, Body: []byte(tc.body), Transport: tc.transport, WebsocketTimeline: []byte(tc.timeline), Status: tc.status, Streaming: tc.responseStream, Response: []byte(`{"error":"forbidden"}`)}
			if err := ing.Process(ctx, r); err != nil {
				t.Fatal(err)
			}
			var got bool
			if err := db.Conn().QueryRow(ctx, "SELECT stream FROM `"+db.Database()+"`.requests FINAL WHERE request_id = ?", r.RequestID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("stored stream=%v, want %v (client body=%s, response streaming=%v, status=%d)", got, tc.want, tc.body, tc.responseStream, tc.status)
			}
		})
	}
}
