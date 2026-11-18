package management

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/livehandle"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiveRequestsShape(t *testing.T) {
	key := "private-key-for-live-test"
	id := "live-handler-test"
	keyID := config.APIKeyID(key)
	livehandle.Default.Start(livehandle.Request{ID: id, KeyID: keyID})
	defer livehandle.Default.Finish(id, "finished", 200)
	livehandle.Default.StartAttempt(id, livehandle.Attempt{}, "raw-handler-session", "")
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: key, Label: "Studio"}}}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/v8/management/live-requests", nil)
	h.GetLiveRequests(c)
	body := rec.Body.String()
	for _, want := range []string{`"server_time"`, `"retention_seconds":30`, `"requests"`, `"id":"` + keyID[:8] + `"`, `"label":"Studio"`, `"ended_at":null`, `"session":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "raw-handler-session") || strings.Contains(body, key) || strings.Contains(body, keyID) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response %s", body)
	}
}

func TestLiveStreamChecksCallerAgainWithoutCountingFailures(t *testing.T) {
	valid, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.MinCost)
	h := &Handler{
		cfg:            &config.Config{RemoteManagement: config.RemoteManagement{SecretKey: string(valid)}},
		failedAttempts: make(map[string]*attemptInfo),
	}
	// The middleware admitted "viewer", then the key was revoked before the handler ran.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// A deadline, so a stream that wrongly opens ends the test with frames instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Request = httptest.NewRequestWithContext(ctx, "GET", "/v8/management/live-requests/stream", nil)
	c.Request.RemoteAddr = "127.0.0.1:1234"
	c.Request.Header.Set("Authorization", "Bearer viewer")
	h.StreamLiveRequests(c)
	if rec.Code != 401 || strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("revoked key admitted: %d %s", rec.Code, rec.Body.String())
	}
	// Re-checking established streams never counts as failed logins: six revoked streams later,
	// a valid key from the same address still gets in.
	for i := 0; i < 6; i++ {
		if _, allowed, _, _ := h.checkManagementScope("127.0.0.1", true, "viewer", false); allowed {
			t.Fatal("revoked key allowed")
		}
	}
	if _, allowed, status, message := h.authenticateManagementScope("127.0.0.1", true, "admin"); !allowed {
		t.Fatalf("valid key locked out: %d %s", status, message)
	}
}

func TestLiveRequestsShowCurrentCredentialLabels(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "live-rename", Provider: "codex", Attributes: map[string]string{"label": "Before"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	snapshot := livehandle.Snapshot{Requests: []livehandle.Request{{ID: "r", Attempts: []livehandle.Attempt{{AuthID: auth.ID, Credential: livehandle.Credential{Label: "Before"}}}}}}
	auth.Attributes["label"] = "After"
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h.decorateLiveRequests(&snapshot)
	if got := snapshot.Requests[0].Attempts[0].Credential.Label; got != "After" {
		t.Fatalf("credential label %q", got)
	}
	// A credential removed since keeps the label the attempt started with.
	gone := livehandle.Snapshot{Requests: []livehandle.Request{{ID: "r", Attempts: []livehandle.Attempt{{AuthID: "removed", Credential: livehandle.Credential{Label: "Kept"}}}}}}
	h.decorateLiveRequests(&gone)
	if got := gone.Requests[0].Attempts[0].Credential.Label; got != "Kept" {
		t.Fatalf("removed credential label %q", got)
	}
}

func TestLiveRequestDecorationDoesNotRaceKeyEdits(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "k1", Label: "One"}}}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// As management writes do: edit the shared key list under the handler lock.
			h.mu.Lock()
			h.cfg.APIKeys = append(h.cfg.APIKeys[:0], config.APIKeyEntry{Key: "k1", Label: fmt.Sprint("One ", i)})
			h.mu.Unlock()
		}
	}()
	snapshot := livehandle.Snapshot{Requests: []livehandle.Request{{ID: "r", KeyID: config.APIKeyID("k1")}}}
	for i := 0; i < 200; i++ {
		h.decorateLiveRequests(&snapshot)
	}
	<-done
}
