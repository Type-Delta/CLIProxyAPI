package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/net/context"
)

// Handlers build their execution context on context.Background(), so the
// correlation values assigned by the request middleware must be carried over
// explicitly; otherwise analytics record every endpoint as unknown.
func TestGetContextWithCancelInheritsUsageCorrelation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true", nil)
	receivedAt := time.Date(2026, 9, 9, 1, 2, 3, 0, time.UTC)
	requestCtx := coreusage.WithProxyRequestID(ginCtx.Request.Context(), "d1371f43e6b8362d05d7567ed5fcc2ad")
	requestCtx = coreusage.WithEndpointClass(requestCtx, "chat_completions")
	requestCtx = coreusage.WithRequestReceivedAt(requestCtx, receivedAt)
	requestCtx = coreusage.WithClientRequestLine(requestCtx, http.MethodPost, "/v1/chat/completions")
	ginCtx.Request = ginCtx.Request.WithContext(requestCtx)

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	if got := coreusage.ProxyRequestIDFromContext(ctx); got != "d1371f43e6b8362d05d7567ed5fcc2ad" {
		t.Fatalf("proxy request ID = %q", got)
	}
	if got := coreusage.EndpointClassFromContext(ctx); got != "chat_completions" {
		t.Fatalf("endpoint class = %q", got)
	}
	coreusage.ObserveRequestRouting(requestCtx, receivedAt.Add(12*time.Millisecond))
	if got := coreusage.ObserveRequestRouting(ctx, receivedAt.Add(time.Hour)); got == nil || *got != 12*time.Millisecond {
		t.Fatalf("routing observation not shared: %v", got)
	}
	if got := coreusage.RequestReceivedAtFromContext(ctx); !got.Equal(receivedAt) {
		t.Fatalf("received at = %v", got)
	}
	if method, path := coreusage.ClientRequestLineFromContext(ctx); method != http.MethodPost || path != "/v1/chat/completions" {
		t.Fatalf("request line = %q %q", method, path)
	}
}

func TestGetContextWithCancelPublishesProxyResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var observed []coreusage.ProxyResponse
	coreusage.SetProxyResponseObserver(func(response coreusage.ProxyResponse) {
		observed = append(observed, response)
	})
	defer coreusage.SetProxyResponseObserver(nil)

	newContext := func(status int) (APIHandlerCancelFunc, *gin.Context) {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ginCtx.Request = ginCtx.Request.WithContext(coreusage.WithProxyRequestID(ginCtx.Request.Context(), "d1371f43e6b8362d05d7567ed5fcc2ad"))
		handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		_, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
		ginCtx.Status(status)
		return cancel, ginCtx
	}

	cancel, _ := newContext(http.StatusOK)
	cancel(nil)
	cancel, _ = newContext(http.StatusTooManyRequests)
	cancel(&interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("rate limited")})
	cancel, _ = newContext(http.StatusBadGateway)
	cancel(errors.New("upstream closed"))

	if len(observed) != 3 {
		t.Fatalf("observed %d responses, want 3", len(observed))
	}
	if observed[0].StatusCode != http.StatusOK || observed[0].Error != "" {
		t.Fatalf("success response = %+v", observed[0])
	}
	if observed[1].StatusCode != http.StatusTooManyRequests || observed[1].Error != "rate limited" {
		t.Fatalf("error message response = %+v", observed[1])
	}
	if observed[2].Error != "upstream closed" || observed[2].RespondedAt.IsZero() {
		t.Fatalf("error response = %+v", observed[2])
	}
	for _, response := range observed {
		if response.ProxyRequestID != "d1371f43e6b8362d05d7567ed5fcc2ad" {
			t.Fatalf("proxy request ID = %q", response.ProxyRequestID)
		}
	}
}
