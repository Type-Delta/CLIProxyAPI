package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type kiroProbeRequest struct {
	path          string
	authorization string
	profileARN    string
	origin        string
	resourceType  string
	emailRequired string
}

func TestKiroUsageLimitsURL(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "kiro",
		Attributes: map[string]string{
			"region":      "eu-west-1",
			"profile_arn": "arn:aws:codewhisperer:eu-west-1:123456789012:profile/test",
		},
	}
	parsed, errParse := url.Parse(kiroUsageLimitsURL(auth))
	if errParse != nil {
		t.Fatalf("kiroUsageLimitsURL() parse error = %v", errParse)
	}
	if parsed.Host != "codewhisperer.eu-west-1.amazonaws.com" || parsed.Path != "/getUsageLimits" {
		t.Fatalf("Kiro usage URL = %s://%s%s, want codewhisperer.eu-west-1.amazonaws.com/getUsageLimits", parsed.Scheme, parsed.Host, parsed.Path)
	}
	if got := parsed.Query().Get("profileArn"); got != auth.Attributes["profile_arn"] {
		t.Fatalf("profileArn = %q, want %q", got, auth.Attributes["profile_arn"])
	}

	parsedDefault, errParseDefault := url.Parse(kiroUsageLimitsURL(&coreauth.Auth{Provider: "kiro"}))
	if errParseDefault != nil {
		t.Fatalf("default kiroUsageLimitsURL() parse error = %v", errParseDefault)
	}
	if parsedDefault.Host != "codewhisperer.us-east-1.amazonaws.com" {
		t.Fatalf("default Kiro usage host = %q, want codewhisperer.us-east-1.amazonaws.com", parsedDefault.Host)
	}

	metadataAuth := &coreauth.Auth{
		Provider: "kiro",
		Metadata: map[string]any{
			"region":     "ap-southeast-1",
			"profileArn": "arn:aws:codewhisperer:ap-southeast-1:123456789012:profile/metadata",
		},
	}
	parsedMetadata, errParseMetadata := url.Parse(kiroUsageLimitsURL(metadataAuth))
	if errParseMetadata != nil {
		t.Fatalf("metadata kiroUsageLimitsURL() parse error = %v", errParseMetadata)
	}
	if parsedMetadata.Host != "codewhisperer.ap-southeast-1.amazonaws.com" || parsedMetadata.Query().Get("profileArn") != metadataAuth.Metadata["profileArn"] {
		t.Fatalf("metadata Kiro usage URL = %s?profileArn=%q, want ap-southeast-1 metadata profile", parsedMetadata.Host, parsedMetadata.Query().Get("profileArn"))
	}
}

func TestRefreshQuotaForRoutingKiro(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	requestCh := make(chan kiroProbeRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCh <- kiroProbeRequest{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			profileARN:    r.URL.Query().Get("profileArn"),
			origin:        r.URL.Query().Get("origin"),
			resourceType:  r.URL.Query().Get("resourceType"),
			emailRequired: r.URL.Query().Get("isEmailRequired"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO+"},"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":1,"usageLimit":10,"nextDateReset":%d}]}`, reset.Unix())
	}))
	t.Cleanup(server.Close)
	originalEndpoint := kiroUsageLimitsEndpoint
	kiroUsageLimitsEndpoint = func(string) string { return server.URL }
	t.Cleanup(func() { kiroUsageLimitsEndpoint = originalEndpoint })

	manager := coreauth.NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kiro-gsr-refresh",
		Provider: "kiro",
		Attributes: map[string]string{
			"region":      "eu-west-1",
			"profile_arn": "arn:aws:codewhisperer:eu-west-1:123456789012:profile/test",
		},
		Metadata: map[string]any{"access_token": "kiro-access-token"},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	handler := &Handler{authManager: manager}
	if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, "claude-sonnet-4-5"); errRefresh != nil {
		t.Fatalf("RefreshQuotaForRouting() error = %v", errRefresh)
	}

	request := <-requestCh
	if request.path != "/getUsageLimits" {
		t.Fatalf("probe path = %q, want /getUsageLimits", request.path)
	}
	if request.authorization != "Bearer kiro-access-token" {
		t.Fatalf("Authorization = %q, want resolved Kiro access token", request.authorization)
	}
	if request.profileARN != auth.Attributes["profile_arn"] {
		t.Fatalf("profileArn = %q, want %q", request.profileARN, auth.Attributes["profile_arn"])
	}
	if request.origin != "AI_EDITOR" || request.resourceType != "AGENTIC_REQUEST" || request.emailRequired != "true" {
		t.Fatalf("probe query = origin %q resourceType %q isEmailRequired %q", request.origin, request.resourceType, request.emailRequired)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated Kiro auth not found")
	}
	if got := updated.Quota.Signals["monthly_reset_at"]; got != reset.Format(time.RFC3339) {
		t.Fatalf("monthly_reset_at = %q, want %q", got, reset.Format(time.RFC3339))
	}
	if got := updated.Quota.Signals["kiro_plan_type"]; got != "KIRO PRO+" {
		t.Fatalf("kiro_plan_type = %q, want KIRO PRO+", got)
	}
}

func TestRefreshQuotaForRoutingKiroMissingReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO"},"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":1,"usageLimit":10}]}`))
	}))
	t.Cleanup(server.Close)
	originalEndpoint := kiroUsageLimitsEndpoint
	kiroUsageLimitsEndpoint = func(string) string { return server.URL }
	t.Cleanup(func() { kiroUsageLimitsEndpoint = originalEndpoint })

	manager := coreauth.NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kiro-gsr-missing-reset",
		Provider: "kiro",
		Metadata: map[string]any{"access_token": "kiro-access-token"},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	handler := &Handler{authManager: manager}
	if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, "claude-sonnet-4-5"); errRefresh == nil {
		t.Fatal("RefreshQuotaForRouting() error = nil, want missing-reset error")
	}
}
