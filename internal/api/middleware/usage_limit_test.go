package middleware

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

const usageLimitTestKey = "test-key"

func TestUsageLimitMiddlewareAllowsRequest(t *testing.T) {
	tracker := newUsageLimitTestTracker()
	called := false
	engine := usageLimitTestEngine(tracker, ProtocolOpenAI, usageLimitTestKey, &called)

	response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestUsageLimitMiddlewareRejectsRequestWithProtocolBodiesAndHeaders(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		body     func(message string) map[string]any
	}{
		{
			name:     "openai",
			protocol: ProtocolOpenAI,
			body: func(message string) map[string]any {
				return map[string]any{"error": map[string]any{
					"message": message,
					"type":    "rate_limit_exceeded",
					"code":    "usage_limit_exceeded",
				}}
			},
		},
		{
			name:     "anthropic",
			protocol: ProtocolAnthropic,
			body: func(message string) map[string]any {
				return map[string]any{
					"type": "error",
					"error": map[string]any{
						"type":    "rate_limit_error",
						"message": message,
					},
				}
			},
		},
		{
			name:     "gemini",
			protocol: ProtocolGemini,
			body: func(message string) map[string]any {
				return map[string]any{"error": map[string]any{
					"code":    float64(http.StatusTooManyRequests),
					"message": message,
					"status":  "RESOURCE_EXHAUSTED",
				}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := newUsageLimitTestTracker()
			called := false
			engine := usageLimitTestEngine(tracker, test.protocol, usageLimitTestKey, &called)

			if response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test"); response.Code != http.StatusNoContent {
				t.Fatalf("initial request status = %d, want %d", response.Code, http.StatusNoContent)
			}
			called = false
			response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test")
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
			}
			if called {
				t.Fatal("next handler was called for a rejected request")
			}

			resetUnix, errReset := strconv.ParseInt(response.Header().Get("X-RateLimit-Reset"), 10, 64)
			if errReset != nil || resetUnix == 0 {
				t.Fatalf("X-RateLimit-Reset = %q, want unix timestamp", response.Header().Get("X-RateLimit-Reset"))
			}
			retryAfter, errRetryAfter := strconv.ParseInt(response.Header().Get("Retry-After"), 10, 64)
			if errRetryAfter != nil || retryAfter < 1 {
				t.Fatalf("Retry-After = %q, want positive seconds", response.Header().Get("Retry-After"))
			}
			expectedRetryAfter := int64(math.Ceil(time.Until(time.Unix(resetUnix, 0)).Seconds()))
			if expectedRetryAfter < 1 {
				expectedRetryAfter = 1
			}
			if retryAfter < expectedRetryAfter-1 || retryAfter > expectedRetryAfter {
				t.Fatalf("Retry-After = %d, want %d or %d", retryAfter, expectedRetryAfter-1, expectedRetryAfter)
			}
			if got := response.Header().Get("X-RateLimit-Limit"); got != "1" {
				t.Fatalf("X-RateLimit-Limit = %q, want %q", got, "1")
			}
			if got := response.Header().Get("X-RateLimit-Remaining"); got != "0" {
				t.Fatalf("X-RateLimit-Remaining = %q, want %q", got, "0")
			}

			message := "usage limit exceeded: requests limit of 1 reached; resets at " + time.Unix(resetUnix, 0).UTC().Format(time.RFC3339)
			var body map[string]any
			if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			if got, want := body, test.body(message); !equalJSONValues(got, want) {
				t.Fatalf("body = %#v, want %#v", got, want)
			}
		})
	}
}

func TestUsageLimitMiddlewareLifetimeLimitOmitsResetHeaders(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{
		usageLimitTestKey: {MaxRequests: 1},
	})
	called := false
	engine := usageLimitTestEngine(tracker, ProtocolOpenAI, usageLimitTestKey, &called)

	if response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test"); response.Code != http.StatusNoContent {
		t.Fatalf("initial request status = %d, want %d", response.Code, http.StatusNoContent)
	}
	called = false
	response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	if called {
		t.Fatal("next handler was called for a rejected request")
	}
	if got := response.Header().Get("X-RateLimit-Limit"); got != "1" {
		t.Fatalf("X-RateLimit-Limit = %q, want 1", got)
	}
	if got := response.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if got := response.Header().Get("X-RateLimit-Reset"); got != "" {
		t.Fatalf("X-RateLimit-Reset = %q, want omitted", got)
	}
	if got := response.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want omitted", got)
	}

	var body map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	want := map[string]any{"error": map[string]any{
		"message": "usage limit exceeded: requests limit of 1 reached",
		"type":    "rate_limit_exceeded",
		"code":    "usage_limit_exceeded",
	}}
	if !equalJSONValues(body, want) {
		t.Fatalf("body = %#v, want %#v", body, want)
	}
}

func TestUsageLimitMiddlewarePassesThroughWithoutTrackerOrKey(t *testing.T) {
	tests := []struct {
		name    string
		tracker *usagelimit.Tracker
		key     string
	}{
		{name: "nil tracker", key: usageLimitTestKey},
		{name: "empty key", tracker: newUsageLimitTestTracker()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			engine := usageLimitTestEngine(test.tracker, ProtocolOpenAI, test.key, &called)
			response := serveUsageLimitTestRequest(engine, http.MethodPost, "/test")
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
			}
			if !called {
				t.Fatal("next handler was not called")
			}
		})
	}
}

func TestUsageLimitMiddlewareSkipsRoutesWithoutConsumingQuota(t *testing.T) {
	tests := []struct {
		method string
		path   string
		route  string
	}{
		{method: http.MethodGet, path: "/v1/models", route: "/v1/models"},
		{method: http.MethodGet, path: "/v1beta/models", route: "/v1beta/models"},
		{method: http.MethodPost, path: "/v1/messages/count_tokens", route: "/v1/messages/count_tokens"},
		{method: http.MethodGet, path: "/v1/videos/request-1", route: "/v1/videos/:request_id"},
		{method: http.MethodGet, path: "/openai/v1/videos/video-1", route: "/openai/v1/videos/:video_id"},
		{method: http.MethodGet, path: "/openai/v1/videos/video-1/content", route: "/openai/v1/videos/:video_id/content"},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.route, func(t *testing.T) {
			tracker := newUsageLimitTestTracker()
			called := false
			engine := gin.New()
			engine.Use(func(c *gin.Context) {
				c.Set("userApiKey", usageLimitTestKey)
				c.Next()
			})
			engine.Use(UsageLimitMiddleware(tracker, ProtocolOpenAI))
			engine.Handle(test.method, test.route, func(c *gin.Context) {
				called = true
				c.Status(http.StatusNoContent)
			})

			response := serveUsageLimitTestRequest(engine, test.method, test.path)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
			}
			if !called {
				t.Fatal("next handler was not called")
			}
			snapshot := tracker.Snapshot(usageLimitTestKey, time.Now())
			if snapshot == nil || snapshot.RequestsUsed != 0 {
				t.Fatalf("request usage after skipped route = %#v, want zero", snapshot)
			}
		})
	}
}

func newUsageLimitTestTracker() *usagelimit.Tracker {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{
		usageLimitTestKey: {MaxRequests: 1, Resets: usagelimit.PeriodHourly},
	})
	return tracker
}

func usageLimitTestEngine(tracker *usagelimit.Tracker, protocol Protocol, key string, called *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if key != "" {
			c.Set("userApiKey", key)
		}
		c.Next()
	})
	engine.Use(UsageLimitMiddleware(tracker, protocol))
	engine.POST("/test", func(c *gin.Context) {
		*called = true
		c.Status(http.StatusNoContent)
	})
	return engine
}

func serveUsageLimitTestRequest(engine *gin.Engine, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func equalJSONValues(got, want map[string]any) bool {
	gotJSON, errGot := json.Marshal(got)
	wantJSON, errWant := json.Marshal(want)
	return errGot == nil && errWant == nil && string(gotJSON) == string(wantJSON)
}
