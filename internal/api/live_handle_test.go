package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/livehandle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestLiveHandleAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/responses", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			router := gin.New()
			router.Use(ProxyRequestIDMiddleware(), accessAuthMiddleware(nil, false))
			var id string
			router.Any(path, func(c *gin.Context) {
				id = coreusage.ProxyRequestIDFromContext(c.Request.Context())
				found := false
				for _, req := range livehandle.Default.Snapshot().Requests {
					if req.ID == id {
						found = true
						if req.State != "routing" {
							t.Fatal(req.State)
						}
					}
				}
				if found != (path != "/v1/models") {
					t.Fatalf("registered %v", found)
				}
				c.Status(201)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
			found := false
			for _, req := range livehandle.Default.Snapshot().Requests {
				if req.ID == id {
					found = true
					if req.State != "finished" || req.HTTPStatus != 201 {
						t.Fatalf("%+v", req)
					}
				}
			}
			if found != (path != "/v1/models") {
				t.Fatalf("registered after %v", found)
			}
		})
	}
}

func TestLiveHandleHTTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	router.POST("/v1/responses", ProxyRequestIDMiddleware(), accessAuthMiddleware(nil, false), func(c *gin.Context) {
		ctx := c.Request.Context()
		livehandle.Default.SetModel(coreusage.ProxyRequestIDFromContext(ctx), "test-model")
		reporter := helps.NewUsageReporter(ctx, "test-provider", "upstream-model", nil)
		helps.ObserveResponsesTokenEvent(reporter, []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call"}}`))
		reporter.ObserveOutputDeltaAt("hello", time.Now())
		close(entered)
		<-release
		reporter.Publish(ctx, coreusage.Detail{InputTokens: 2, OutputTokens: 3})
		c.Status(200)
	})
	handler := management.NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	router.GET("/v8/management/live-requests", handler.GetLiveRequests)
	server := httptest.NewServer(router)
	defer server.Close()
	go func() {
		defer close(done)
		resp, err := http.Post(server.URL+"/v1/responses", "application/json", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	resp, err := http.Get(server.URL + "/v8/management/live-requests")
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	close(release)
	<-done
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), `"state":"streaming"`) || !strings.Contains(string(body), `"output_bytes":5`) || !strings.Contains(string(body), `"requested_model":"test-model"`) || !strings.Contains(string(body), `"tool_calls":1`) {
		t.Fatalf("live response %d %s %v", resp.StatusCode, body, err)
	}
}
