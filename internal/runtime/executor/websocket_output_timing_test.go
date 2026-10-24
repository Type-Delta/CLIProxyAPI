package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestWebsocketOutputTimingUsesSourceTimestampsAndPressure(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, pressure := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "-observed", true: "-backpressure"}[pressure], func(t *testing.T) {
				ctx := context.Background()
				records := make(chan usage.Record, 1)
				unregister, err := usage.RegisterAccountingNamedPlugin("ws-output-timing", generationRecordCapture{records})
				if err != nil {
					t.Fatal(err)
				}
				defer unregister(ctx)
				r := helps.NewUsageReporter(ctx, provider, "generation-test", nil)
				r.StartUpstreamTiming()
				conn := &websocket.Conn{}
				queue := make(chan codexWebsocketRead, 3)
				first := time.Now().Add(20 * time.Second)
				payload := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat(" token", 50) + `"}`)
				for i := range 3 {
					queue <- codexWebsocketRead{conn: conn, msgType: websocket.TextMessage, payload: payload, observedAt: first.Add(time.Duration(i) * 100 * time.Millisecond), timingUnreliable: pressure && i == 2}
				}
				for range 3 {
					var got []byte
					if provider == "codex" {
						_, got, err = readCodexWebsocketMessage(ctx, nil, conn, queue, r)
					} else {
						_, got, err = readXAIWebsocketMessage(ctx, nil, conn, queue, r)
					}
					if err != nil || string(got) != string(payload) {
						t.Fatalf("reader changed frame: err=%v", err)
					}
				}
				r.Publish(ctx, usage.Detail{OutputTokens: 9999})
				var record usage.Record
				select {
				case record = <-records:
				case <-time.After(5 * time.Second):
					t.Fatal("usage record not published")
				}
				timing := record.OutputTiming
				if timing == nil || timing.Source != "websocket_reader" || timing.ArrivalCount != 3 || timing.LastArrivalNS-timing.FirstArrivalNS != int64(200*time.Millisecond) {
					t.Fatalf("consumer timestamps replaced source times: %+v", timing)
				}
				if pressure {
					if timing.Status != "unavailable" || timing.Reason != "backpressure" {
						t.Fatalf("pressure reason lost: %+v", timing)
					}
				} else if timing.Status != "observed" || timing.WindowTokens != 100 {
					t.Fatalf("matching tokens changed: %+v", timing)
				}
			})
		}
	}
}

func TestXAIWebsocketReaderErrorsIdentifyProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		conn  *websocket.Conn
		queue chan codexWebsocketRead
	}{
		{name: "missing"},
		{name: "closed", conn: &websocket.Conn{}, queue: make(chan codexWebsocketRead)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.queue != nil {
				close(tc.queue)
			}
			_, _, err := readXAIWebsocketMessage(context.Background(), nil, tc.conn, tc.queue, nil)
			if err == nil || !strings.HasPrefix(err.Error(), "xai websockets executor:") || strings.Contains(err.Error(), "codex") {
				t.Fatalf("xAI reader error carries wrong provider: %v", err)
			}
		})
	}
}

func TestWebsocketEarlyPressureRetainsTimingProvenance(t *testing.T) {
	for _, msgType := range []int{websocket.BinaryMessage, websocket.TextMessage} {
		t.Run(map[int]string{websocket.BinaryMessage: "nontext", websocket.TextMessage: "error"}[msgType], func(t *testing.T) {
			ctx := context.Background()
			records := make(chan usage.Record, 1)
			unregister, err := usage.RegisterAccountingNamedPlugin("early-pressure", generationRecordCapture{records})
			if err != nil {
				t.Fatal(err)
			}
			defer unregister(ctx)
			r := helps.NewUsageReporter(ctx, "codex", "generation-test", nil)
			r.StartUpstreamTiming()
			conn := &websocket.Conn{}
			queue := make(chan codexWebsocketRead, 1)
			event := codexWebsocketRead{conn: conn, msgType: msgType, timingUnreliable: true}
			if msgType == websocket.TextMessage {
				event.err = context.Canceled
			}
			queue <- event
			_, _, _ = readCodexWebsocketMessage(ctx, nil, conn, queue, r)
			r.Publish(ctx, usage.Detail{OutputTokens: 1})
			var record usage.Record
			select {
			case record = <-records:
			case <-time.After(5 * time.Second):
				t.Fatal("usage record not published")
			}
			got := record.OutputTiming
			if got == nil || got.Protocol != "responses" || got.Source != "websocket_reader" || got.Coverage != "visible_deltas" || got.Status != "unavailable" || got.Reason != "backpressure" {
				t.Fatalf("early invalidation lost provenance: %+v", got)
			}
		})
	}
}
