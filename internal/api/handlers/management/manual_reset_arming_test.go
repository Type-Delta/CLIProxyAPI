package management

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeManualResetOffersMatchClaimBlockers(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, status, grant string
		want                bool
	}{
		{"usable at limit", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true`, true},
		{"needs limit", `"eligible":true,"at_limit":false`, `"resets_left":1,"usable_now":true`, false},
		{"limit not required", `"eligible":true,"at_limit":false`, `"resets_left":1,"usable_now":true,"use_requires_limit":false`, true},
		{"ineligible", `"eligible":false,"at_limit":true`, `"resets_left":1,"usable_now":true`, false},
		{"spent", `"eligible":true,"at_limit":true`, `"resets_left":0,"usable_now":true`, false},
		{"paused", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true,"paused":true`, false},
		{"not usable", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":false`, false},
		{"not started", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true,"starts_at":"2030-01-02T00:00:00Z"`, false},
		{"cooldown", `"eligible":true,"at_limit":true,"cooldown_until":"2030-01-01T01:00:00Z"`, `"resets_left":1,"usable_now":true`, false},
		{"no usage to give back", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true,"gives_back_weekly":true,"percent_used":{"seven_day":0}`, false},
		{"usage to give back", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true,"gives_back_weekly":true,"percent_used":{"seven_day":40}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"cedar_ember":{%s,"grants":[{"id":"grant-1",%s,"clears":["five_hour","seven_day","seven_day_overage_included"],"ends_at":"2030-01-03T00:00:00Z"}]}}`, test.status, test.grant)
			offers := claudeManualResetOffers([]byte(body), now)
			if (len(offers) == 1) != test.want {
				t.Fatalf("offers = %+v, want offer=%t", offers, test.want)
			}
			if test.want && (offers[0].grantID != "grant-1" || strings.Join(offers[0].refills, ",") != "five-hour,seven-day,seven-day-fable" || !offers[0].expiresAt.Equal(now.Add(48*time.Hour))) {
				t.Fatalf("offer = %+v", offers[0])
			}
		})
	}
}

func TestCodexManualResetOffersKeepAvailableCredits(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	body := `{"credits":[
		{"reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-02T00:00:00Z"},
		{"resetType":"codex_rate_limits","status":"available","expiresAt":1893542400},
		{"reset_type":"codex_rate_limits","status":"available"},
		{"reset_type":"codex_rate_limits","status":"redeemed","expires_at":"2030-01-02T00:00:00Z"},
		{"reset_type":"other","status":"available","expires_at":"2030-01-02T00:00:00Z"},
		{"reset_type":"codex_rate_limits","status":"available","expires_at":"2029-12-31T00:00:00Z"}]}`
	offers := codexManualResetOffers([]byte(body), now)
	if len(offers) != 3 || !offers[0].expiresAt.Equal(now.Add(24*time.Hour)) || !offers[1].expiresAt.Equal(now.Add(24*time.Hour)) || !offers[2].expiresAt.IsZero() {
		t.Fatalf("offers = %+v", offers)
	}
}

func TestPickManualResetOfferTriggers(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	weekly := manualResetOffer{grantID: "weekly", expiresAt: now.Add(48 * time.Hour), refills: []string{"seven-day"}}
	session := manualResetOffer{grantID: "session", expiresAt: now.Add(24 * time.Hour), refills: []string{"five-hour"}}
	expiring := manualResetOffer{grantID: "expiring", expiresAt: now.Add(manualResetExpiryLead), refills: []string{"five-hour"}}
	undated := manualResetOffer{grantID: "undated", refills: []string{"seven-day"}}
	for _, test := range []struct {
		name    string
		offers  []manualResetOffer
		blocked []string
		want    string
	}{
		{"healthy", []manualResetOffer{weekly, session}, nil, ""},
		{"refills blocked bar", []manualResetOffer{session, weekly}, []string{"seven-day"}, "weekly"},
		{"soonest matching", []manualResetOffer{undated, weekly}, []string{"seven-day"}, "weekly"},
		{"undated still usable", []manualResetOffer{undated}, []string{"seven-day"}, "undated"},
		{"unrelated bar", []manualResetOffer{weekly}, []string{"seven-day-opus"}, ""},
		{"expiring soon", []manualResetOffer{weekly, expiring}, nil, "expiring"},
		{"just outside lead", []manualResetOffer{{grantID: "later", expiresAt: now.Add(manualResetExpiryLead + time.Second)}}, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocked := make(map[string]bool)
			for _, bar := range test.blocked {
				blocked[bar] = true
			}
			offer, ok := pickManualResetOffer(test.offers, blocked, now)
			if ok != (test.want != "") || offer.grantID != test.want {
				t.Fatalf("picked %q (%t), want %q", offer.grantID, ok, test.want)
			}
		})
	}
}

func TestManualResetBlockedBars(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := []coreauth.QuotaReserveBar{
		{ID: "five-hour", RemainingPercent: 0, ResetAt: now.Add(time.Hour)},
		{ID: "weekly", RemainingPercent: 0, ResetAt: now.Add(-time.Minute)},
		{ID: "monthly", RemainingPercent: 0},
		{ID: "seven-day", RemainingPercent: 12, ResetAt: now.Add(time.Hour)},
	}
	blocked := manualResetBlockedBars(bars, []coreauth.QuotaReserveHit{{BarID: "seven-day"}, {BarID: "five-hour"}}, now)
	// Only weekly bars count: the 5-hour and monthly bars are spent but never trigger.
	if len(blocked) != 1 || !blocked["seven-day"] {
		t.Fatalf("blocked = %v", blocked)
	}
	blocked = manualResetBlockedBars([]coreauth.QuotaReserveBar{{ID: "weekly", RemainingPercent: 0}}, nil, now)
	if len(blocked) != 1 || !blocked["weekly"] {
		t.Fatalf("blocked = %v", blocked)
	}
}

func TestPatchManualResetArmedField(t *testing.T) {
	for _, test := range []struct {
		name, provider, body string
		wantArmed            bool
		wantErr              string
	}{
		{"arm codex", "codex", `true`, true, ""},
		{"arm claude", "claude", `true`, true, ""},
		{"disarm with null", "codex", `null`, false, ""},
		{"disarm with false", "claude", `false`, false, ""},
		{"wrong type", "codex", `"yes"`, false, "must be a boolean or null"},
		{"unsupported provider", "gemini", `true`, false, "only supported for Claude and Codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &coreauth.Auth{Provider: test.provider, Metadata: map[string]any{manualResetArmedKey: true}}
			err := applyAuthFileFieldsPatch(auth, map[string]json.RawMessage{manualResetArmedKey: json.RawMessage(test.body)}, authFileRequestRetryPatch{})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || manualResetArmed(auth) != test.wantArmed {
				t.Fatalf("armed = %t, err = %v", manualResetArmed(auth), err)
			}
		})
	}
	nested := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{}}
	if err := applyAuthFileFieldsPatch(nested, map[string]json.RawMessage{manualResetArmedKey + ".x": json.RawMessage(`true`)}, authFileRequestRetryPatch{}); err == nil {
		t.Fatal("nested manual_reset_armed path was accepted")
	}
}

type armingUpstream struct {
	mu       sync.Mutex
	usage    string
	grants   string
	credits  string
	result   string
	claims   []string
	postURLs []string
	onRead   func()
	// claimStatus overrides the claim's HTTP status; zero means 200.
	claimStatus int
}

func (u *armingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		u.claims = append(u.claims, string(body))
		u.postURLs = append(u.postURLs, "https://"+r.Host+r.URL.Path)
		if u.claimStatus != 0 {
			w.WriteHeader(u.claimStatus)
		}
		_, _ = w.Write([]byte(u.result))
	case r.URL.Path == "/api/oauth/profile":
		_, _ = w.Write([]byte(`{"organization":{"uuid":"11111111-2222-3333-4444-555555555555"}}`))
	case r.URL.Path == "/api/oauth/usage":
		if u.onRead != nil {
			// Released while serving so the hook can take manager locks.
			u.mu.Unlock()
			u.onRead()
			u.mu.Lock()
		}
		_, _ = w.Write([]byte(u.grants))
	case strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits"):
		_, _ = w.Write([]byte(u.credits))
	default:
		_, _ = w.Write([]byte(u.usage))
	}
}

// failingSaveStore accepts registration but fails every later save, like a
// read-only or full auth directory.
type failingSaveStore struct{ saves int }

func (s *failingSaveStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *failingSaveStore) Delete(context.Context, string) error           { return nil }
func (s *failingSaveStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.saves++
	if s.saves > 1 {
		return "", fmt.Errorf("disk full")
	}
	return auth.ID, nil
}

func newArmingHandler(t *testing.T, provider string, attributes map[string]string, apiKeys ...string) (*Handler, *coreauth.Auth, *armingUpstream) {
	return newArmingHandlerWithStore(t, nil, provider, attributes, apiKeys...)
}

func newArmingHandlerWithStore(t *testing.T, store coreauth.Store, provider string, attributes map[string]string, apiKeys ...string) (*Handler, *coreauth.Auth, *armingUpstream) {
	t.Helper()
	upstream := &armingUpstream{}
	newGSRProbeFixture(t, upstream)
	manager := coreauth.NewManager(store, nil, nil)
	attrs := map[string]string{"api_key": "upstream-token"}
	for key, value := range attributes {
		attrs[key] = value
	}
	auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "arming-" + provider,
		Provider:   provider,
		Attributes: attrs,
		Metadata:   map[string]any{manualResetArmedKey: true},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	cfg := &config.Config{}
	for _, key := range apiKeys {
		cfg.APIKeys = append(cfg.APIKeys, config.APIKeyEntry{Key: key})
	}
	return &Handler{authManager: manager, cfg: cfg}, auth, upstream
}

func (h *Handler) armedForTest(t *testing.T, id string) bool {
	t.Helper()
	auth, ok := h.authManager.GetByID(id)
	return ok && manualResetArmed(auth)
}

func claudeArmingStatus(weeklyUsed int, atLimit bool, endsAt time.Time) string {
	return claudeArmingWindows(40, weeklyUsed, atLimit, endsAt)
}

func claudeArmingWindows(fiveHourUsed, weeklyUsed int, atLimit bool, endsAt time.Time) string {
	resetsAt := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{"five_hour":{"utilization":%d,"resets_at":%q},"seven_day":{"utilization":%d,"resets_at":%q},`+
		`"cedar_ember":{"eligible":true,"at_limit":%t,"grants":[{"id":"grant-1","resets_total":1,"resets_left":1,"usable_now":true,`+
		`"clears":["five_hour","seven_day"],"ends_at":%q}]}}`, fiveHourUsed, resetsAt, weeklyUsed, resetsAt, atLimit, endsAt.UTC().Format(time.RFC3339))
}

func TestArmedResetIgnoresSpentFiveHourLimit(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingWindows(100, 40, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 || !h.armedForTest(t, auth.ID) {
		t.Fatalf("claims=%v armed=%t", upstream.claims, h.armedForTest(t, auth.ID))
	}
}

func TestArmedClaudeResetClaimsWhenExhaustedAndDisarms(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`

	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 1 || upstream.postURLs[0] != claudeResetTestURL {
		t.Fatalf("claims = %v to %v", upstream.claims, upstream.postURLs)
	}
	claim := upstream.claims[0]
	if ojson.Get(claim, "program").String() != "cedar_ember" || ojson.Get(claim, "grant_id").String() != "grant-1" || !validClaudeResetID(ojson.Get(claim, "request_id").String(), 64, true) {
		t.Fatalf("claim body = %s", claim)
	}
	if h.armedForTest(t, auth.ID) {
		t.Fatal("credential still armed after spending its reset")
	}
}

func TestArmedClaudeResetWaitsWhileHealthy(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingStatus(40, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 || !h.armedForTest(t, auth.ID) {
		t.Fatalf("healthy credential: claims=%v armed=%t", upstream.claims, h.armedForTest(t, auth.ID))
	}
}

// One-shot means one claim: a refusal, an ambiguous gateway error or a missing
// result all leave the credential disarmed, so CPA never sends a second claim.
func TestArmedResetStaysDisarmedWhateverTheAnswer(t *testing.T) {
	for _, test := range []struct {
		result string
		status int
	}{{`{"result":"cooldown"}`, 0}, {`{"unexpected":true}`, 0}, {`not json`, 0}, {`bad gateway`, http.StatusBadGateway}} {
		t.Run(test.result, func(t *testing.T) {
			h, auth, upstream := newArmingHandler(t, "claude", nil)
			upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
			upstream.result = test.result
			upstream.claimStatus = test.status
			h.checkArmedManualReset(context.Background(), auth)
			h.checkArmedManualReset(context.Background(), auth)
			if len(upstream.claims) != 1 || h.armedForTest(t, auth.ID) {
				t.Fatalf("claims=%d armed=%t", len(upstream.claims), h.armedForTest(t, auth.ID))
			}
		})
	}
}

func TestArmedResetSendsNothingWhenDisarmedDuringItsReads(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	upstream.onRead = func() {
		// A disarm PATCH that lands while the check is reading usage.
		_, _, _ = h.authManager.Modify(context.Background(), auth.ID, 0, func(current *coreauth.Auth) error {
			delete(current.Metadata, manualResetArmedKey)
			return nil
		})
	}
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 {
		t.Fatalf("claimed after a disarm: %v", upstream.claims)
	}
}

func TestArmedResetSendsNothingWhenTheDisarmCannotBeSaved(t *testing.T) {
	h, auth, upstream := newArmingHandlerWithStore(t, &failingSaveStore{}, "claude", nil)
	upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 {
		t.Fatalf("claimed although the arm could not be cleared on disk: %v", upstream.claims)
	}
}

func TestArmedResetWaitsForAnInFlightPanelClaim(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	unlock := h.lockManualResetClaim(auth.ID)
	done := make(chan struct{})
	go func() {
		h.checkArmedManualReset(context.Background(), auth)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("armed check ran while a panel claim held the credential")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	<-done
}

func TestArmedResetTreatsOnlyUnbypassableReserveAsBlocked(t *testing.T) {
	const liveKey = "live-client-key"
	for _, test := range []struct {
		name      string
		bypass    []string
		wantClaim bool
	}{
		{"no bypass list", nil, true},
		{"stale bypass key", []string{config.APIKeyID("deleted-key")}, true},
		{"live bypass key", []string{config.APIKeyID(liveKey)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			attributes := map[string]string{coreauth.AttributeQuotaReserve: `{"seven-day":30}`}
			if test.bypass != nil {
				raw, _ := json.Marshal(test.bypass)
				attributes[coreauth.AttributeQuotaReserveBypassKeys] = string(raw)
			}
			h, auth, upstream := newArmingHandler(t, "claude", attributes, liveKey)
			// 80% used leaves 20%, inside the 30% reserve but not spent. The grant
			// does not require a limit, so only the reserve can trigger it.
			upstream.grants = strings.Replace(claudeArmingStatus(80, false, time.Now().Add(72*time.Hour)), `"usable_now":true`, `"usable_now":true,"use_requires_limit":false`, 1)
			upstream.result = `{"result":"reset"}`
			h.checkArmedManualReset(context.Background(), auth)
			// A spent reset refills the bar and releases the hold.
			if _, held := h.authManager.QuotaReserveState(auth.ID); held == test.wantClaim {
				t.Fatalf("reserve held = %t after check", held)
			}
			if claimed := len(upstream.claims) == 1; claimed != test.wantClaim {
				t.Fatalf("claimed = %t, want %t", claimed, test.wantClaim)
			}
			if bypassable := h.quotaReserveBypassable(auth); bypassable == test.wantClaim {
				t.Fatalf("bypassable = %t", bypassable)
			}
		})
	}
}

func TestArmedCodexResetSpendsExpiringCredit(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "codex", nil)
	reset := time.Now().Add(3 * time.Hour).Unix()
	upstream.usage = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":%d}}}`, reset)
	upstream.credits = fmt.Sprintf(`{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":%q}]}`, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	upstream.result = `{"code":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 1 || upstream.postURLs[0] != codexConsumeURL || ojson.Get(upstream.claims[0], "redeem_request_id").String() == "" {
		t.Fatalf("claims = %v to %v", upstream.claims, upstream.postURLs)
	}
	if h.armedForTest(t, auth.ID) {
		t.Fatal("credential still armed after consuming its credit")
	}
}

func TestArmedCodexResetKeepsDistantCreditWhileHealthy(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "codex", nil)
	reset := time.Now().Add(3 * time.Hour).Unix()
	upstream.usage = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":%d}}}`, reset)
	upstream.credits = fmt.Sprintf(`{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":%q}]}`, time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
	upstream.result = `{"code":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 || !h.armedForTest(t, auth.ID) {
		t.Fatalf("claims=%v armed=%t", upstream.claims, h.armedForTest(t, auth.ID))
	}
}

func TestPatchManualResetArmedRequiresV8(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{ID: "arm.json", FileName: "arm.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/arm.json"}, Metadata: map[string]any{"type": "codex"}}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	patch := func(v8 bool, body string) int {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/credentials/fields", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Set(ConfigV8ContextKey, v8)
		h.PatchAuthFileFields(ctx)
		return rec.Code
	}
	if code := patch(false, `{"name":"arm.json","manual_reset_armed":true}`); code != http.StatusBadRequest {
		t.Fatalf("v0 arm status = %d", code)
	}
	if code := patch(true, `{"name":"arm.json","manual_reset_armed":true}`); code != http.StatusOK || !h.armedForTest(t, "arm.json") {
		t.Fatalf("v8 arm status = %d armed=%t", code, h.armedForTest(t, "arm.json"))
	}
	// Disarming stays possible everywhere, so an arm can always be withdrawn.
	if code := patch(false, `{"name":"arm.json","manual_reset_armed":null}`); code != http.StatusOK || h.armedForTest(t, "arm.json") {
		t.Fatalf("v0 disarm status = %d", code)
	}
}

func TestArmedResetSendsNothingWhenDisabledDuringItsReads(t *testing.T) {
	h, auth, upstream := newArmingHandler(t, "claude", nil)
	upstream.grants = claudeArmingStatus(100, true, time.Now().Add(72*time.Hour))
	upstream.result = `{"result":"reset"}`
	upstream.onRead = func() {
		_, _, _ = h.authManager.Modify(context.Background(), auth.ID, 0, func(current *coreauth.Auth) error {
			current.Disabled = true
			current.Status = coreauth.StatusDisabled
			return nil
		})
	}
	h.checkArmedManualReset(context.Background(), auth)
	if len(upstream.claims) != 0 {
		t.Fatalf("claimed for a disabled credential: %v", upstream.claims)
	}
}

func TestPatchDisarmFailsWhenTheStoreCannotSaveIt(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(&failingSaveStore{}, nil, nil)
	record := &coreauth.Auth{ID: "arm.json", FileName: "arm.json", Provider: "codex", Attributes: map[string]string{"path": "/tmp/arm.json"}, Metadata: map[string]any{"type": "codex", manualResetArmedKey: true}}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/credentials/fields", strings.NewReader(`{"name":"arm.json","manual_reset_armed":null}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(ConfigV8ContextKey, true)
	h.PatchAuthFileFields(ctx)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unsaved disarm answered %d %s", rec.Code, rec.Body.String())
	}
}

func TestArmedCodexResetFiresOnlyForTheWeeklyLimit(t *testing.T) {
	for _, test := range []struct {
		name             string
		fiveHour, weekly int
		wantClaim        bool
	}{
		{"weekly spent", 30, 100, true},
		{"five-hour spent", 100, 30, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, auth, upstream := newArmingHandler(t, "codex", nil)
			reset := time.Now().Add(3 * time.Hour).Unix()
			upstream.usage = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000,"reset_at":%d},`+
				`"secondary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`, test.fiveHour, reset, test.weekly, reset)
			upstream.credits = fmt.Sprintf(`{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":%q}]}`, time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
			upstream.result = `{"code":"reset"}`
			h.checkArmedManualReset(context.Background(), auth)
			if claimed := len(upstream.claims) == 1; claimed != test.wantClaim {
				t.Fatalf("claimed = %t, want %t", claimed, test.wantClaim)
			}
			if armed := h.armedForTest(t, auth.ID); armed == test.wantClaim {
				t.Fatalf("armed = %t after check", armed)
			}
		})
	}
}

func TestArmedResetIgnoresUnbypassableFiveHourReserve(t *testing.T) {
	attributes := map[string]string{coreauth.AttributeQuotaReserve: `{"five-hour":30}`}
	h, auth, upstream := newArmingHandler(t, "claude", attributes)
	// 80% used leaves 20%, inside the 30% reserve: held, nobody can bypass, but it is not the weekly bar.
	upstream.grants = strings.Replace(claudeArmingWindows(80, 40, false, time.Now().Add(72*time.Hour)), `"usable_now":true`, `"usable_now":true,"use_requires_limit":false`, 1)
	upstream.result = `{"result":"reset"}`
	h.checkArmedManualReset(context.Background(), auth)
	if _, held := h.authManager.QuotaReserveState(auth.ID); !held {
		t.Fatal("five-hour reserve not held; the test no longer exercises a reserve hit")
	}
	if len(upstream.claims) != 0 || !h.armedForTest(t, auth.ID) {
		t.Fatalf("claims=%v armed=%t", upstream.claims, h.armedForTest(t, auth.ID))
	}
}
