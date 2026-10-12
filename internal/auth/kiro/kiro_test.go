package kiro

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshTokenSupportsDesktopAndOIDCShapes(t *testing.T) {
	tests := []struct {
		name     string
		storage  *TokenStorage
		wantPath string
		body     string
	}{
		{name: "desktop", storage: &TokenStorage{RefreshToken: "desktop-refresh", Region: "us-east-1"}, wantPath: "/refreshToken", body: `{"accessToken":"desktop-access","refreshToken":"desktop-next","expiresIn":3600}`},
		{name: "oidc", storage: &TokenStorage{RefreshToken: "oidc-refresh", ClientID: "client", ClientSecret: "secret", Region: "eu-west-1", AuthMethod: "oidc"}, wantPath: "/token", body: `{"accessToken":"oidc-access","refreshToken":"oidc-next","expiresIn":3600,"profileArn":"arn:profile"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *http.Request
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header), Request: req}, nil
			})}
			service := NewAuthWithHTTPClient(client, "us-east-1")
			response, err := service.RefreshToken(context.Background(), tt.storage)
			if err != nil {
				t.Fatalf("RefreshToken() error = %v", err)
			}
			if response.AccessToken == "" || got == nil || got.URL.Path != tt.wantPath {
				t.Fatalf("request=%v response=%+v", got, response)
			}
			if got.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", got.Header.Get("Content-Type"))
			}
		})
	}
}

func TestPortalURLAndCallback(t *testing.T) {
	portal := BuildPortalURL("state-1", "challenge-1", PortalRedirectURI)
	parsed, err := url.Parse(portal)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "app.kiro.dev" || parsed.Path != "/signin" || parsed.Query().Get("redirect_from") != "kirocli" {
		t.Fatalf("portal URL = %s", portal)
	}
	callback, err := ParsePortalCallback("http://localhost:3128/signin/callback?login_option=awsidc&issuer_url=https%3A%2F%2Fd.example%2Fstart&idc_region=us-east-1&state=state-1")
	if err != nil {
		t.Fatal(err)
	}
	if callback.LoginOption != "awsidc" || callback.IssuerURL != "https://d.example/start" || callback.IDCRegion != "us-east-1" || callback.State != "state-1" {
		t.Fatalf("callback = %+v", callback)
	}
}

func TestTokenStorageUnmarshalNativeCamelCase(t *testing.T) {
	var storage TokenStorage
	if err := json.Unmarshal([]byte(`{"type":"kiro","accessToken":"a","refreshToken":"r","expiresAt":"2026-01-01T00:00:00Z","profileArn":"arn","clientId":"id","clientSecret":"secret","authMethod":"oidc"}`), &storage); err != nil {
		t.Fatal(err)
	}
	if storage.AccessToken != "a" || storage.RefreshToken != "r" || storage.ProfileARN != "arn" || storage.ClientID != "id" || storage.AuthMethod != "oidc" {
		t.Fatalf("storage = %+v", storage)
	}
}

func TestResolveProfileARN(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "q.us-east-1.amazonaws.com" || req.Header.Get("X-Amz-Target") != "AmazonCodeWhispererService.ListAvailableProfiles" {
			t.Fatalf("profile request = %s target=%q", req.URL, req.Header.Get("X-Amz-Target"))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"profiles":[{"arn":"arn:profile"}]}`)), Header: make(http.Header), Request: req}, nil
	})}
	profile, err := NewAuthWithHTTPClient(client, "us-east-1").ResolveProfileARN(context.Background(), "access", "us-east-1")
	if err != nil || profile != "arn:profile" {
		t.Fatalf("ResolveProfileARN() = %q, %v", profile, err)
	}
}

func TestRefreshTokenClassifiesRevokedRefreshToken(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"message":"Your access token could not be refreshed because your refresh token was revoked. Please log out and sign in again."}`)),
			Header:     make(http.Header), Request: req,
		}, nil
	})}
	_, err := NewAuthWithHTTPClient(client, "us-east-1").RefreshToken(context.Background(), &TokenStorage{
		RefreshToken: "revoked", ClientID: "client", ClientSecret: "secret", AuthMethod: "oidc", Region: "us-east-1",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("RefreshToken() error = %v, want invalid_grant revoked error", err)
	}
}
