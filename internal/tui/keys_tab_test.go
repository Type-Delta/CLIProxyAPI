package tui

import (
	"encoding/json"
	"errors"
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
		apiKeys: []string{limitedKey, unlimitedKey},
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
		apiKeys: []string{key},
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

func TestKeysTabRefreshCancelsResetConfirmation(t *testing.T) {
	key := "limited-secret-key"
	m := newKeysTabModel(nil)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{
		apiKeys: []string{key},
		limits:  map[string]APIKeyLimit{key: {MaxRequests: 10}},
	})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})

	m, _ = m.Update(keysDataMsg{
		apiKeys: []string{"different-secret-key"},
		limits:  map[string]APIKeyLimit{},
	})

	if m.resetConfirm != -1 {
		t.Fatalf("resetConfirm after refresh = %d, want -1", m.resetConfirm)
	}
}
