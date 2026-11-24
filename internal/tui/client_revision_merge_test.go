package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func revisionedKeyClient(t *testing.T, entries []config.APIKeyEntry) (*Client, *Client, *atomic.Int32) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\napi-keys: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := management.NewHandler(&config.Config{SDKConfig: config.SDKConfig{APIKeys: entries}}, path, nil)
	router := gin.New()
	router.GET("/v8/management/api-keys", h.GetAPIKeys)
	router.PATCH("/v8/management/api-keys", h.PatchAPIKeys)
	router.DELETE("/v8/management/api-keys", h.DeleteAPIKeys)
	var legacyCalls atomic.Int32
	router.Any("/v0/*path", func(c *gin.Context) { legacyCalls.Add(1); c.Status(http.StatusNotFound) })
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return NewClientWithBaseURL(server.URL, ""), NewClientWithBaseURL(server.URL, ""), &legacyCalls
}

func TestClientV8RevisionedStructuredKeys(t *testing.T) {
	c, _, legacy := revisionedKeyClient(t, []config.APIKeyEntry{
		{Key: ""}, {Key: "duplicate", Label: "first", Limits: &config.KeyLimits{MaxRequests: 12}},
		{Key: "duplicate", Label: "second", Limits: &config.KeyLimits{MaxTokensM: 2, Resets: "daily"}},
	})
	entries, err := c.GetAPIKeyEntries()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	if entries[1].Index != 2 || entries[1].Revision == "" {
		t.Fatalf("lost source index/revision: %+v", entries[1])
	}
	if err := c.EditAPIKeyLabel(2, "second renamed"); err != nil {
		t.Fatal(err)
	}
	if err := c.RotateAPIKey(2, "rotated"); err != nil {
		t.Fatal(err)
	}
	entries, err = c.GetAPIKeyEntries()
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Label != "first" || entries[0].Limits.MaxRequests != 12 || entries[1].Key != "rotated" || entries[1].Label != "second renamed" || entries[1].Limits.MaxTokensM != 2 {
		t.Fatalf("structured metadata lost: %+v", entries)
	}
	if err := c.DeleteAPIKey(2); err != nil {
		t.Fatal(err)
	}
	if legacy.Load() != 0 {
		t.Fatalf("used deprecated routes %d times", legacy.Load())
	}
}

func TestClientV8EmptyKeysAndMissingRevision(t *testing.T) {
	c, _, _ := revisionedKeyClient(t, nil)
	if entries, err := c.GetAPIKeyEntries(); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if err := c.AddAPIKey("first", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := c.GetAPIKeyEntries()
	if err != nil || len(entries) != 1 || entries[0].Key != "first" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	c.setAPIKeyRevision("")
	if err := c.EditAPIKey(0, "replacement", nil); err == nil || !strings.Contains(err.Error(), "revision unavailable") {
		t.Fatalf("missing revision accepted: %v", err)
	}
}

func TestKeysTabV8RejectsStaleEditAfterBackgroundRefresh(t *testing.T) {
	c, other, legacy := revisionedKeyClient(t, []config.APIKeyEntry{{Key: "A"}, {Key: "B"}, {Key: "C"}})
	entries, err := c.GetAPIKeyEntries()
	if err != nil {
		t.Fatal(err)
	}
	m := newKeysTabModel(c)
	m.SetSize(160, 40)
	m, _ = m.Update(keysDataMsg{entries: entries})
	m.cursor = 1
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if _, err := other.GetAPIKeyEntries(); err != nil {
		t.Fatal(err)
	}
	if err := other.DeleteAPIKey(0); err != nil {
		t.Fatal(err)
	}
	refreshed, err := c.GetAPIKeyEntries()
	if err != nil {
		t.Fatal(err)
	}
	m, _ = m.Update(keysDataMsg{entries: refreshed})
	m.formInputs[keyFieldKey].SetValue("B-edited")
	_, submit := m.submitKeyForm()
	if submit == nil {
		t.Fatal("missing submit command")
	}
	result := submit().(keyActionMsg)
	if result.err == nil {
		t.Fatal("stale form edited a shifted row")
	}
	// A stale delete uses the same original revision and never retries another contract.
	if err := c.withAPIKeyRevision(entries[1].Revision).DeleteAPIKey(entries[1].Index); err == nil {
		t.Fatal("stale delete accepted")
	}
	after, err := c.GetAPIKeyEntries()
	if err != nil || len(after) != 2 || after[0].Key != "B" || after[1].Key != "C" {
		t.Fatalf("wrong row changed: %+v err=%v", after, err)
	}
	if legacy.Load() != 0 {
		t.Fatalf("conflict fell back to v0 %d times", legacy.Load())
	}
}

func TestKeyMutationBusiness404DoesNotRetryLegacy(t *testing.T) {
	var legacy atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/config":
			writeJSON(t, w, map[string]any{})
		case "/v8/management/api-key-limits/reset":
			http.Error(w, `{"error":"key not found"}`, 404)
		default:
			legacy.Add(1)
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	c := NewClientWithBaseURL(server.URL, "")
	if err := c.ResetAPIKeyLimitsByID("unknown"); err == nil {
		t.Fatal("business error lost")
	}
	if legacy.Load() != 0 {
		t.Fatalf("business 404 retried %d legacy calls", legacy.Load())
	}
}
