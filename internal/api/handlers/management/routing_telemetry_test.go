package management

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestAnalyticsGETProxyRequestIDFilter(t *testing.T) {
	values := url.Values{"start": {"2026-08-01T00:00:00Z"}, "end": {"2026-08-02T00:00:00Z"}, "time_zone": {"UTC"}, "proxy_request_id": {"0123456789abcdef0123456789abcdef"}}
	q, err := analyticsGETQuery(values, model.OperationEvents)
	if err != nil {
		t.Fatal(err)
	}
	if string(q.Filters["proxy_request_id"]) != `["0123456789abcdef0123456789abcdef"]` {
		t.Fatalf("filters %v", q.Filters)
	}
	values.Set("proxy_request_id", "invalid")
	if _, err := analyticsGETQuery(values, model.OperationEvents); err == nil {
		t.Fatal("accepted invalid correlation ID")
	}
}

func TestResolveAnalyticsRoutingTracePreservesStrategyAndProvenance(t *testing.T) {
	trace := routing.Trace{Version: 1, Mode: routing.ModeLocal, Pick: 0, Candidates: []routing.Candidate{{ID: "credential"}}, StrategyStep: &routing.StrategyStep{Applied: routing.StrategyGreedySoonestRecovery, Last: -1, Wanted: 0, Weights: []routing.StrategyWeight{{Index: 0, Share: 1, Weight: 42, RecoverySource: "manual_reset"}}}, Affinity: &routing.AffinityStep{State: routing.AffinityKept, Bound: 0, BoundAtMs: 123, BoundRequestID: "0123456789abcdef0123456789abcdef"}}
	raw, _ := json.Marshal(trace)
	event := model.Event{RoutingTrace: raw}
	resolveAnalyticsRoutingTrace(&Handler{}, nil, &event)
	var got routing.Trace
	if err := json.Unmarshal(event.RoutingTrace, &got); err != nil {
		t.Fatal(err)
	}
	if got.StrategyStep == nil || got.StrategyStep.Weights[0].RecoverySource != "manual_reset" || got.Affinity.BoundRequestID != trace.Affinity.BoundRequestID || got.Affinity.BoundAtMs != 123 {
		t.Fatalf("trace %s", event.RoutingTrace)
	}
}

func TestRoutingDryRunBoundAnalyticsID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}, TTL: time.Hour})
	defer selector.Stop()
	manager := coreauth.NewManager(nil, selector, nil)
	provider, modelName := "routing-analytics-test", "routing-analytics-model"
	manager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
	for _, id := range []string{"routing-analytics-a", "routing-analytics-b"} {
		if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: modelName}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	opts := executor.Options{Headers: http.Header{"X-Session-Id": {"analytics-binding"}}}
	ctx := usage.WithProxyRequestID(context.Background(), "11111111111111111111111111111111")
	bound, err := selector.Pick(ctx, provider, modelName, opts, manager.List())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.AnalyticsState{model.StateReady, model.StateDisabled} {
		t.Run(string(state), func(t *testing.T) {
			service := &providerRowsDisplayTestService{analyticsHandlerService: &analyticsHandlerService{state: state}}
			h := &Handler{authManager: manager, cfg: &config.Config{}, analytics: service}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(`{"model":"routing-analytics-model","session_id":"analytics-binding"}`))
			h.PostRoutingDryRun(c)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var result struct {
				Trace routing.Trace `json:"trace"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Trace.Affinity.State != routing.AffinityKept {
				t.Fatalf("affinity=%+v", result.Trace.Affinity)
			}
			raw, dropped := model.SanitizeRoutingTrace(&routing.Trace{Version: 1, Candidates: []routing.Candidate{{ID: bound.ID, Provider: bound.Provider, AuthIndex: bound.EnsureIndex()}}}, ([32]byte)([]byte(strings.Repeat("k", 32))), true, provider)
			if dropped {
				t.Fatal("trace unexpectedly dropped")
			}
			var stored routing.Trace
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			for i, candidate := range result.Trace.Candidates {
				want := ""
				if state == model.StateReady && i == result.Trace.Affinity.Bound {
					want = stored.Candidates[0].ID
				}
				if candidate.AnalyticsID != want {
					t.Fatalf("candidate=%+v want analytics_id=%q", candidate, want)
				}
			}
		})
	}
}

func TestRoutingDryRunWebsocketInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(&refreshRecordExecutor{provider: "codex"})
	for _, a := range []*coreauth.Auth{
		{ID: "routing-http", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"priority": "10"}},
		{ID: "routing-ws", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"websockets": "true"}},
	} {
		if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "routing-ws-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	h := &Handler{authManager: manager, cfg: &config.Config{}}
	for _, ws := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(fmt.Sprintf(`{"model":"routing-ws-model","downstream_websocket":%t}`, ws)))
		h.PostRoutingDryRun(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var result struct {
			Trace routing.Trace `json:"trace"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want := "routing-http"
		if ws {
			want = "routing-ws"
		}
		if result.Trace.Pick < 0 || result.Trace.Candidates[result.Trace.Pick].ID != want {
			t.Fatalf("ws=%t trace=%+v", ws, result.Trace)
		}
	}
}
