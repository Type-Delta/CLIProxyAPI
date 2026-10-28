package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/crypto/bcrypt"
)

func TestAnalyticsClickHouseRealServer(t *testing.T) {
	ctx := context.Background()
	handle := chtest.Handle(t)
	db, err := handle.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		port, err := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(port), "\n")[0])
	}
	dir := t.TempDir()
	t.Setenv("WRITABLE_PATH", dir)
	legacyPath := filepath.Join(dir, "legacy", "analytics.db")
	rate := model.NanoUSD(1_000_000_000)
	legacy, err := store.Open(ctx, store.Config{Path: legacyPath, MaxStorageBytes: 64 << 20, PriceBook: aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "legacy-price", Model: "smoke-model", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "management-api"}}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event := model.Event{SchemaVersion: 1, AttemptID: strings.Repeat("a", 32), ProxyRequestID: strings.Repeat("b", 32), RequestIDQuality: model.RequestIDObserved, KeyID: model.KeyID("smoke-client"), RequestedAt: now.Add(-time.Hour), Provider: "smoke-provider", ExecutorType: "openai-compatible", Model: "smoke-model", EndpointClass: "chat_completions", Succeeded: true, Generated: true, Tokens: model.TokenUsage{Input: 10, Output: 5, Total: 15, Schema: model.AccountingSchemaNormalizedV1, Quality: model.TokenQualityExact}}
	if _, err := legacy.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "legacy-price", Model: "smoke-model", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "management-api"}}}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	identity := legacy.IdentityKeyArray()
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chat-i2","object":"chat.completion","model":"smoke-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer upstream.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("smoke-management"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "smoke-client"}}, RequestLog: true}, ClickHouse: config.DefaultClickHouseConfig(), Analytics: config.DefaultAnalyticsConfig()}
	cfg.RemoteManagement = config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}
	cfg.ClickHouse.DSN, cfg.ClickHouse.Database = dsn, db.Database()
	cfg.Analytics.Enabled, cfg.Analytics.Path = true, legacyPath
	cfg.Analytics.FlushInterval, cfg.Analytics.BatchSize = 10*time.Millisecond, 1
	cfg.Analytics.MaxStorageBytes, cfg.Analytics.MinFreeBytes = 64<<20, 0
	cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "smoke-provider", BaseURL: upstream.URL + "/v1", Models: []config.OpenAICompatibilityModel{{Name: "smoke-model", Alias: "smoke-model"}}}}
	start := func() (*Server, *httptest.Server) {
		t.Helper()
		server := newTestServerWithConfig(t, cfg)
		if !server.applyAccessConfig(nil, cfg) {
			t.Fatal("access configuration")
		}
		manager := server.handlers.AuthManager
		manager.RegisterExecutor(executor.NewOpenAICompatExecutor("smoke-provider", cfg))
		cred := &auth.Auth{ID: "analytics-smoke-auth", Provider: "smoke-provider", Status: auth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": "mock-upstream"}}
		if _, err := manager.Register(ctx, cred); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(cred.ID, cred.Provider, []*registry.ModelInfo{{ID: "smoke-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(cred.ID) })
		live := httptest.NewServer(server.engine)
		server.server = live.Config
		t.Cleanup(live.Close)
		t.Cleanup(func() { _ = server.Stop(ctx) })
		return server, live
	}
	request := func(live *httptest.Server, method, path, key string, body any) []byte {
		t.Helper()
		var data []byte
		if body != nil {
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			req, err := http.NewRequest(method, live.URL+path, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if method == "GET" && resp.StatusCode == http.StatusServiceUnavailable && bytes.Contains(response, []byte("analytics_maintenance")) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, response)
			}
			return response
		}
	}
	wait := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("analytics smoke condition timed out")
	}
	server, live := start()
	wait(func() bool { return server.analytics.Health().State == cpauk.StateReady })
	wait(func() bool {
		status := server.analytics.Health().LegacyImport
		return status != nil && status.State == "done"
	})
	health := request(live, "GET", "/v8/management/analytics/health", "smoke-management", nil)
	t.Logf("import health: %s", health)
	if server.analytics.Health().LegacyImport.Counts["events"] != 1 {
		t.Fatalf("legacy counts %+v", server.analytics.Health())
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(legacyPath), "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, identity[:]) {
		t.Fatal("identity changed")
	}
	query := "?start=" + url.QueryEscape(now.Add(-2*time.Hour).Format(time.RFC3339Nano)) + "&end=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339Nano)) + "&time_zone=UTC"
	summary := func(want int64) bool {
		data := request(live, "GET", "/v8/management/analytics/summary"+query, "smoke-management", nil)
		var result model.Summary
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result.UpstreamAttempts == want
	}
	if !summary(1) {
		t.Fatal("missing imported summary")
	}
	for _, endpoint := range []string{"timeseries", "events", "keys", "pricing"} {
		selection := query
		if endpoint == "timeseries" {
			selection += "&bucket_width=1h"
		}
		data := request(live, "GET", "/v8/management/analytics/"+endpoint+selection, "smoke-management", nil)
		switch endpoint {
		case "timeseries":
			var series model.Timeseries
			if err := json.Unmarshal(data, &series); err != nil {
				t.Fatal(err)
			}
			var attempts int64
			for _, point := range series.Points {
				attempts += point.UpstreamAttempts
			}
			if attempts != 1 {
				t.Fatalf("imported timeseries attempts %d", attempts)
			}
		case "events":
			var page model.EventPage
			if err := json.Unmarshal(data, &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 1 || page.Events[0].AttemptID != event.AttemptID {
				t.Fatalf("imported event page %+v", page)
			}
		case "keys":
			if !bytes.Contains(data, []byte(event.KeyID)) {
				t.Fatalf("missing imported key %s", data)
			}
		case "pricing":
			if !bytes.Contains(data, []byte("legacy-price")) {
				t.Fatalf("missing imported pricing %s", data)
			}
		}
		t.Logf("%s: %s", endpoint, data)
	}
	send := func() {
		request(live, "POST", "/v1/chat/completions", "smoke-client", map[string]any{"model": "smoke-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}})
	}
	send()
	wait(func() bool { return summary(2) })
	liveEvents := request(live, "GET", "/v8/management/analytics/events"+query, "smoke-management", nil)
	var livePage model.EventPage
	if err := json.Unmarshal(liveEvents, &livePage); err != nil {
		t.Fatal(err)
	}
	if len(livePage.Events) != 2 {
		t.Fatalf("combined events %s", liveEvents)
	}
	t.Logf("combined imported/live events: %s", liveEvents)
	job := func(path string, body any) model.JobStatus {
		t.Helper()
		data := request(live, "POST", path, "smoke-management", body)
		var status model.JobStatus
		if err := json.Unmarshal(data, &status); err != nil {
			t.Fatal(err)
		}
		wait(func() bool {
			data = request(live, "GET", "/v8/management/analytics/jobs/"+status.JobID, "smoke-management", nil)
			if err := json.Unmarshal(data, &status); err != nil {
				t.Fatal(err)
			}
			return status.State == model.JobSucceeded || status.State == model.JobFailed
		})
		if status.State != model.JobSucceeded {
			t.Fatalf("maintenance failed %s", data)
		}
		t.Logf("maintenance: %s", data)
		return status
	}
	backupPath := filepath.Join(dir, "backup.json")
	backup := job("/v8/management/analytics/backups", map[string]string{"path": backupPath})
	send()
	wait(func() bool { return summary(3) })
	backupID, _ := backup.Result["backup_id"].(string)
	job("/v8/management/analytics/backups/"+backupID+"/restore", map[string]string{"path": backupPath, "manifest": backupPath + ".manifest.json"})
	wait(func() bool { return summary(2) })
	before, found, err := db.Job(ctx, "legacy-import:analytics")
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	live.Close()
	server, live = start()
	wait(func() bool { return server.analytics.Health().State == cpauk.StateReady })
	wait(func() bool {
		return server.analytics.Health().LegacyImport != nil && server.analytics.Health().LegacyImport.State == "done"
	})
	if !summary(2) {
		t.Fatal("restart totals")
	}
	after, _, err := db.Job(ctx, "legacy-import:analytics")
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.Detail != before.Detail {
		t.Fatalf("import reran: before %+v after %+v", before, after)
	}
	current, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatal("legacy source changed")
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	live.Close()
	cfg.ClickHouse.DSN = ""
	server, live = start()
	wait(func() bool { return server.analytics.Health().Category == "clickhouse_not_configured" })
	data := request(live, "GET", "/v8/management/analytics/health", "smoke-management", nil)
	if !bytes.Contains(data, []byte("clickhouse_not_configured")) || !bytes.Contains(data, []byte("clickhouse.dsn")) {
		t.Fatalf("plain health %s", data)
	}
	t.Logf("plain health: %s", data)
	send()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
