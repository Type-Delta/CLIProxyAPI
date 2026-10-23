package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
)

func TestManagementAssetRouteServesEmbeddedMascot(t *testing.T) {
	embedded, errAsset := managementasset.PanelAsset("mascots/claude.webp")
	if errAsset != nil {
		t.Skipf("embedded mascot unavailable: %v", errAsset)
	}
	server := newTestServer(t)

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/management-assets/mascots/claude.webp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type = %q, want image/webp", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Fatalf("Cache-Control = %q, want public, max-age=86400", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), embedded) {
		t.Fatalf("GET body = %d bytes, want %d", rr.Body.Len(), len(embedded))
	}

	headRR := httptest.NewRecorder()
	server.engine.ServeHTTP(headRR, httptest.NewRequest(http.MethodHead, "/management-assets/mascots/claude.webp", nil))
	if headRR.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", headRR.Code, http.StatusOK)
	}
	if headRR.Body.Len() != 0 {
		t.Fatalf("HEAD body = %d bytes, want 0", headRR.Body.Len())
	}
	if got := headRR.Header().Get("Content-Length"); got != fmt.Sprint(len(embedded)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", got, len(embedded))
	}
	if got := headRR.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("HEAD Content-Type = %q, want image/webp", got)
	}
}

func TestManagementAssetRouteRejectsTraversalAndMissingFiles(t *testing.T) {
	server := newTestServer(t)

	paths := []string{
		"/management-assets/mascots/../management.html",
		"/management-assets/%2e%2e/management.html",
		"/management-assets/mascots/%2e%2e/%2e%2e/management.html",
		"/management-assets/management.html",
		"/management-assets/mascots",
		"/management-assets/mascots/unknown.webp",
	}
	for _, requestPath := range paths {
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", requestPath, rr.Code, http.StatusNotFound)
		}
	}

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/management-assets/mascots/claude.webp", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestManagementAssetRouteRespectsControlPanelGate(t *testing.T) {
	server := newTestServer(t)
	server.cfg.RemoteManagement.DisableControlPanel = true

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/management-assets/mascots/claude.webp", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled panel status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestManagementAssetRouteHiddenByHomeMode(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Home.Enabled = true

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/management-assets/mascots/claude.webp", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("home mode status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestManagementControlPanelMissingBundleReturns503(t *testing.T) {
	original := resolveManagementPanelAsset
	resolveManagementPanelAsset = func(string) (managementasset.ResolvedAsset, error) {
		return managementasset.ResolvedAsset{}, fmt.Errorf("%w: test build", managementasset.ErrBundleMissing)
	}
	t.Cleanup(func() { resolveManagementPanelAsset = original })

	server := newTestServer(t)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/management.html", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scripts/build-management-center.sh") {
		t.Fatalf("body missing build instructions: %s", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestManagementAssetRouteNotBlockedByExampleAPIKeySafeMode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServerWithOptions(t, WithExampleAPIKeySafeMode())
	cfg := *server.cfg
	cfg.APIKeys = []proxyconfig.APIKeyEntry{{Key: "your-api-key-1"}}
	server.UpdateClients(&cfg)

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/management-assets/mascots/claude.webp", nil))
	if rr.Code == http.StatusForbidden {
		t.Fatalf("asset route blocked by safe mode: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "Example API key detected") {
		t.Fatalf("asset route served the safe-mode warning page")
	}
	if rr.Code != http.StatusOK && rr.Code != http.StatusNotFound {
		t.Fatalf("asset status = %d, want 200 with a mascot bundle or 404 without one", rr.Code)
	}
}
