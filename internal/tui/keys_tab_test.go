package tui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeysTabRendersLimitAndWarningWithoutKeyLeak(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	limitedKey := "limited-secret-key"
	unlimitedKey := "unlimited-secret-key"
	resetAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: limitedKey, Index: 0}, {Key: unlimitedKey, Index: 1}},
		limits: map[string]APIKeyLimit{
			limitedKey: {
				MaxRequests:  11,
				RequestsUsed: 7,
				MaxTokens:    17,
				TokensUsed:   13,
				Resets:       "daily",
				ResetAt:      &resetAt,
			},
		},
		limitsErr: errors.New("limits unavailable"),
	})

	content := m.renderContent()
	for _, want := range []string{
		maskKey(limitedKey),
		maskKey(unlimitedKey),
		"requests 7/11",
		"tokens 13/17",
		"daily",
		resetAt.Format(time.RFC3339),
		"Usage limit data unavailable: limits unavailable",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q: %s", want, content)
		}
	}
	if strings.Contains(content, limitedKey) || strings.Contains(content, unlimitedKey) {
		t.Fatalf("content exposes an API key: %s", content)
	}
}

func TestKeysTabResetsSelectedLimitedKeyAfterConfirmation(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "limited-secret-key"
	resetKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/management/api-key-limits/reset" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Key string `json:"key"`
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
			t.Errorf("decode reset body: %v", errDecode)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resetKey = body.Key
	}))
	defer server.Close()

	m := newKeysTabModel(&Client{baseURL: server.URL, http: server.Client()})
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: key, Index: 0}},
		limits:  map[string]APIKeyLimit{key: {MaxRequests: 10}},
	})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.resetConfirm != 0 {
		t.Fatalf("resetConfirm = %d, want 0", m.resetConfirm)
	}
	if !strings.Contains(m.renderContent(), "Reset usage for "+maskKey(key)+"? [y/n]") {
		t.Fatalf("reset confirmation not rendered: %s", m.renderContent())
	}

	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("reset command is nil")
	}
	msg := cmd()
	action, ok := msg.(keyActionMsg)
	if !ok || action.err != nil || action.action != T("key_usage_reset") {
		t.Fatalf("reset message = %#v", msg)
	}
	if resetKey != key {
		t.Fatalf("reset key = %q, want %q", resetKey, key)
	}

	m, fetchCmd := m.Update(msg)
	if fetchCmd == nil || !strings.Contains(m.status, T("key_usage_reset")) {
		t.Fatalf("reset status = %q, fetch command = %v", m.status, fetchCmd != nil)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.resetConfirm != -1 {
		t.Fatalf("resetConfirm after Esc = %d, want -1", m.resetConfirm)
	}
}

// TestKeysTabShowsConfiguredLimitsWithoutUsage covers a limited key the runtime
// tracker has never seen: it must not render as unlimited.
func TestKeysTabShowsConfiguredLimitsWithoutUsage(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "unused-limited-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{
			{Key: key, Index: 0, Limits: &APIKeyLimitConfig{MaxRequests: 50, MaxTokensM: 2, Resets: "daily"}},
			{Key: "plain-key", Index: 1},
		},
		limits: map[string]APIKeyLimit{},
	})

	content := m.renderContent()
	for _, want := range []string{"requests 0/50", "tokens 0/2000000", "daily", "on first use"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q: %s", want, content)
		}
	}
	// A cadence must not be printed alongside a "never" reset time.
	if strings.Contains(content, "daily • reset never") {
		t.Errorf("windowed key reported a reset of never: %s", content)
	}
	if m.effectiveLimit(m.entries[1]) != nil {
		t.Fatalf("plain key reported limits: %#v", m.effectiveLimit(m.entries[1]))
	}

	// Usage reset must be offered for keys known only from the configuration.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.resetConfirm != 0 {
		t.Fatalf("resetConfirm = %d, want 0", m.resetConfirm)
	}
}

// TestKeysTabMergesConfiguredLimitsWithUsage checks that recorded usage keeps
// its counters while the ceilings come from the configuration.
func TestKeysTabMergesConfiguredLimitsWithUsage(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "limited-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: key, Index: 0, Limits: &APIKeyLimitConfig{MaxRequests: 100, MaxTokensM: 1, Resets: "daily"}}},
		limits:  map[string]APIKeyLimit{key: {MaxRequests: 100, RequestsUsed: 12, MaxTokens: 1000000, TokensUsed: 400000, Resets: "daily"}},
	})

	content := m.renderContent()
	for _, want := range []string{"requests 12/100", "tokens 400000/1000000"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q: %s", want, content)
		}
	}
}

// TestKeysTabFormNavigationAndCadenceCycler exercises field movement and the
// left/right cadence selector.
func TestKeysTabFormNavigationAndCadenceCycler(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "limited-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: key, Index: 0, Limits: &APIKeyLimitConfig{MaxRequests: 100, MaxTokensM: 2, Resets: "daily"}}},
	})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !m.editing || m.adding {
		t.Fatalf("editing = %v, adding = %v", m.editing, m.adding)
	}
	if got := m.formInputs[keyFieldKey].Value(); got != key {
		t.Fatalf("key field = %q, want %q", got, key)
	}
	if got := m.formInputs[keyFieldMaxRequests].Value(); got != "100" {
		t.Fatalf("max requests field = %q, want 100", got)
	}
	if got := m.formInputs[keyFieldMaxTokens].Value(); got != "2" {
		t.Fatalf("max tokens field = %q, want 2", got)
	}
	if keyResetCadences[m.formCadence] != "daily" {
		t.Fatalf("cadence = %q, want daily", keyResetCadences[m.formCadence])
	}

	for i := 0; i < 3; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.formField != keyFieldResets {
		t.Fatalf("formField = %d, want %d", m.formField, keyFieldResets)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if keyResetCadences[m.formCadence] != "weekly" {
		t.Fatalf("cadence after right = %q, want weekly", keyResetCadences[m.formCadence])
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if keyResetCadences[m.formCadence] != "" {
		t.Fatalf("cadence after left = %q, want empty", keyResetCadences[m.formCadence])
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.formField != keyFieldMaxTokens {
		t.Fatalf("formField after shift+tab = %d", m.formField)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.editing || m.adding {
		t.Fatalf("form still open after esc")
	}
}

// TestKeysTabFormRejectsInvalidNumbers keeps the form open on local validation
// failures.
func TestKeysTabFormRejectsInvalidNumbers(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{entries: []APIKeyEntry{{Key: "key-one", Index: 0}}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})

	m.formInputs[keyFieldMaxRequests].SetValue("abc")
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !m.editing || m.formErr != T("key_form_invalid_number") {
		t.Fatalf("garbage input: cmd=%v editing=%v err=%q", cmd != nil, m.editing, m.formErr)
	}

	m.formInputs[keyFieldMaxRequests].SetValue("-5")
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !m.editing || m.formErr != T("key_form_negative") {
		t.Fatalf("negative input: cmd=%v editing=%v err=%q", cmd != nil, m.editing, m.formErr)
	}

	m.formInputs[keyFieldMaxRequests].SetValue("1")
	m.formInputs[keyFieldKey].SetValue("   ")
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.formErr != T("key_form_key_required") {
		t.Fatalf("empty key: cmd=%v err=%q", cmd != nil, m.formErr)
	}
}

// TestKeysTabFormLimitsPayload verifies which limit block the form produces.
func TestKeysTabFormLimitsPayload(t *testing.T) {
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)

	limits, err := m.formLimits()
	if err != nil || limits != nil {
		t.Fatalf("empty form limits = %#v, err = %v", limits, err)
	}

	m.formInputs[keyFieldMaxRequests].SetValue("0")
	m.formInputs[keyFieldMaxTokens].SetValue("0")
	limits, err = m.formLimits()
	if err != nil || limits != nil {
		t.Fatalf("zero form limits = %#v, err = %v", limits, err)
	}

	m.formInputs[keyFieldMaxTokens].SetValue("1.5")
	m.formCadence = cadenceIndex("monthly")
	limits, err = m.formLimits()
	if err != nil || limits == nil || limits.MaxRequests != 0 || limits.MaxTokensM != 1.5 || limits.Resets != "monthly" {
		t.Fatalf("form limits = %#v, err = %v", limits, err)
	}
}

func TestKeysTabRefreshCancelsResetConfirmation(t *testing.T) {
	key := "limited-secret-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: key, Index: 0}},
		limits:  map[string]APIKeyLimit{key: {MaxRequests: 10}},
	})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})

	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: "different-secret-key", Index: 0}},
		limits:  map[string]APIKeyLimit{},
	})

	if m.resetConfirm != -1 {
		t.Fatalf("resetConfirm after refresh = %d, want -1", m.resetConfirm)
	}
}

// TestKeysTabEditAndDeleteUseOriginalServerIndex covers a server list whose
// first entry is blank: the row shown as 1 is server index 1, and both the edit
// PATCH and the delete must address that index, not the compacted row.
func TestKeysTabEditAndDeleteUseOriginalServerIndex(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	patchBody := ""
	deleteQuery := ""
	deleteRevision := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/api-keys":
			if _, errWrite := w.Write([]byte(`{"api-keys":["",{"key":"real-key"}],"config_revision":"rev-current"}`)); errWrite != nil {
				t.Errorf("write api-keys: %v", errWrite)
			}
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/api-keys":
			raw, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read patch body: %v", errRead)
			}
			patchBody = strings.TrimSpace(string(raw))
		case r.Method == http.MethodDelete && r.URL.Path == "/v0/management/api-keys":
			deleteQuery = r.URL.RawQuery
			deleteRevision = r.Header.Get("If-Match")
			w.Header().Set("X-CPA-Config-Revision", "rev-after-delete")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	m := newKeysTabModel(client)
	m.SetSize(160, 40)
	msg, ok := m.fetchKeys().(keysDataMsg)
	if !ok || msg.err != nil {
		t.Fatalf("fetchKeys = %#v", msg)
	}
	m, _ = m.Update(msg)
	if len(m.entries) != 1 || m.entries[0].Key != "real-key" || m.entries[0].Index != 1 {
		t.Fatalf("entries = %#v", m.entries)
	}

	// Edit the only displayed row (row 0) and save.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m.formInputs[keyFieldKey].SetValue("real-key-v2")
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("edit command is nil")
	}
	if action, okAction := cmd().(keyActionMsg); !okAction || action.err != nil {
		t.Fatalf("edit result = %#v", action)
	}
	if patchBody != `{"index":1,"limits":null,"new":"real-key-v2"}` {
		t.Fatalf("patch body = %s, want index 1", patchBody)
	}

	// Delete the same row.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("delete command is nil")
	}
	if action, okAction := cmd().(keyActionMsg); !okAction || action.err != nil {
		t.Fatalf("delete result = %#v", action)
	}
	if deleteQuery != "index=1" {
		t.Fatalf("delete query = %q, want index=1", deleteQuery)
	}
	if deleteRevision != `"rev-current"` {
		t.Fatalf("delete If-Match = %q, want quoted revision", deleteRevision)
	}
}

// TestKeysTabDuplicateKeysKeepOwnConfiguredLimits checks that two entries
// sharing one key string do not collapse into a single limits display.
func TestKeysTabDuplicateKeysKeepOwnConfiguredLimits(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "duplicated-secret-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{
			{Key: key, Index: 0, Limits: &APIKeyLimitConfig{MaxRequests: 50, Resets: "daily"}},
			{Key: key, Index: 1, Limits: &APIKeyLimitConfig{MaxRequests: 900, Resets: "weekly"}},
		},
		limits: map[string]APIKeyLimit{},
	})

	content := m.renderContent()
	for _, want := range []string{"requests 0/50", "requests 0/900", "daily", "weekly"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q: %s", want, content)
		}
	}

	// Editing the second row must prefill that row's own limits.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if got := m.formInputs[keyFieldMaxRequests].Value(); got != "900" {
		t.Fatalf("max requests field = %q, want 900", got)
	}
}

// TestKeysTabFormRejectsNonFiniteTokenCap keeps the form open for NaN and the
// infinities, which strconv.ParseFloat otherwise accepts.
func TestKeysTabFormRejectsNonFiniteTokenCap(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	for _, value := range []string{"NaN", "Inf", "+Inf", "-Inf", "infinity"} {
		m := newKeysTabModel(nil)
		m.SetSize(160, 40)
		m, _ = m.Update(keysDataMsg{entries: []APIKeyEntry{{Key: "key-one", Index: 0}}})
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
		m.formInputs[keyFieldMaxTokens].SetValue(value)
		m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil || !m.editing || m.formErr != T("key_form_invalid_number") {
			t.Fatalf("%q: cmd=%v editing=%v err=%q", value, cmd != nil, m.editing, m.formErr)
		}
	}
}

// TestKeysTabFormRejectsCadenceWithoutCap covers the inert limit block: a reset
// cadence with no cap enforces nothing and the server discards it.
func TestKeysTabFormRejectsCadenceWithoutCap(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{entries: []APIKeyEntry{{Key: "key-one", Index: 0}}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m.formCadence = cadenceIndex("daily")
	m.formInputs[keyFieldMaxRequests].SetValue("")
	m.formInputs[keyFieldMaxTokens].SetValue("0")
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !m.editing || m.formErr != T("key_form_cadence_needs_cap") {
		t.Fatalf("cadence without cap: cmd=%v editing=%v err=%q", cmd != nil, m.editing, m.formErr)
	}

	// Blank caps with "never" still mean "no limits".
	m.formCadence = cadenceIndex("")
	limits, err := m.formLimits()
	if err != nil || limits != nil {
		t.Fatalf("never cadence limits = %#v, err = %v", limits, err)
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.editing {
		t.Fatalf("never cadence: cmd=%v editing=%v", cmd != nil, m.editing)
	}
}

// TestKeysTabRoundsConfiguredTokenCap pins the TUI to the backend rounding of
// max-tokens-m instead of truncating it.
func TestKeysTabRoundsConfiguredTokenCap(t *testing.T) {
	previousLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(previousLocale) })

	key := "rounded-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		entries: []APIKeyEntry{{Key: key, Index: 0, Limits: &APIKeyLimitConfig{MaxTokensM: 8.2}}},
		limits:  map[string]APIKeyLimit{},
	})

	limit := m.effectiveLimit(m.entries[0])
	if limit == nil || limit.MaxTokens != 8_200_000 {
		t.Fatalf("max tokens = %#v, want 8200000", limit)
	}
	if !strings.Contains(m.renderContent(), "tokens 0/8200000") {
		t.Fatalf("content missing rounded cap: %s", m.renderContent())
	}
}
