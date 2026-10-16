package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
)

func TestV8AnalyticsConditionalReadUsesAuthenticatedService(t *testing.T) {
	server, _ := newPricingManagementServer(t, &integrationPricingFetcher{})
	httpServer := httptest.NewServer(server.engine)
	t.Cleanup(httpServer.Close)
	query := `{"schema_version":2,"operation":"summary","range":{"preset":"last_n_days","n":7,"time_zone":"Asia/Bangkok"}}`
	header := base64.RawURLEncoding.EncodeToString([]byte(query))
	get := func(key, etag string) (int, http.Header, []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/v8/management/analytics/query", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Analytics-Query", header)
		request.Header.Set("If-None-Match", etag)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, body
	}
	const key = "management-pricing-integration-secret"
	status, headers, first := get(key, "")
	if status != http.StatusOK || headers.Get("ETag") == "" {
		t.Fatalf("first query: %d %s", status, first)
	}
	etag := headers.Get("ETag")
	status, _, body := get(key, etag)
	if status != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("conditional query: %d %s", status, body)
	}
	status, _, body = get(key, "")
	if status != http.StatusOK || string(first) != string(body) {
		t.Fatalf("second client's snapshot differs: %d %s", status, body)
	}
	status, _, _ = get("", etag)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cache access: %d", status)
	}
	server.mgmt.SetAnalyticsService(cpauk.NewDisabled())
	status, _, body = get(key, etag)
	if status == http.StatusNotModified || status == http.StatusOK {
		t.Fatalf("disabled service reused snapshot: %d %s", status, body)
	}
}
