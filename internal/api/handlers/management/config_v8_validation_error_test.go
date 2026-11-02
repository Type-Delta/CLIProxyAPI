package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestConfigV8ValidationErrorsPointIntoTheBody keeps validation error lines
// meaningful: they name the field and, for a whole-document PUT, its line in
// the submitted body rather than in the server's re-encoded copy.
func TestConfigV8ValidationErrorsPointIntoTheBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config.yaml", h.ConfigV8)
	router.PATCH("/v8/management/config", h.ConfigV8)

	// Comments, blank lines, and four-space indentation all shift the re-encoded copy.
	head := "# header\n\n# more\nconfig-version: 8\n\n\nserver:\n    # note\n    host: 127.0.0.1\n\n"
	cases := []struct {
		name, method, url, body string
		status                  int
		want                    []string
	}{
		{"type error", http.MethodPut, "/v8/management/config.yaml", head + "    port: notaport\n",
			http.StatusUnprocessableEntity, []string{"line 11 (server.port): cannot unmarshal !!str `notaport` into int"}},
		{"two type errors", http.MethodPut, "/v8/management/config.yaml", head + "    port: notaport\naccess:\n    api-keys: hello\n",
			http.StatusUnprocessableEntity, []string{"line 11 (server.port): ", "line 13 (access.api-keys): "}},
		{"unknown field", http.MethodPut, "/v8/management/config.yaml", head + "    port: 8317\n    hots: 0.0.0.0\n",
			http.StatusBadRequest, []string{"line 12 (server.hots): field hots not found"}},
		{"patch names the field without a line", http.MethodPatch, "/v8/management/config", `{"server":{"port":"notaport"}}`,
			http.StatusUnprocessableEntity, []string{"server.port: cannot unmarshal"}},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body)))
		var got struct{ Error, Message string }
		if err = json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != tc.status || got.Error != "invalid_config" {
			t.Errorf("%s: status=%d body=%s", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(got.Message, want) {
				t.Errorf("%s: message %q does not contain %q", tc.name, got.Message, want)
			}
		}
		if tc.method == http.MethodPatch && strings.Contains(got.Message, "line ") {
			t.Errorf("%s: PATCH bodies must not report lines: %q", tc.name, got.Message)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("invalid config changed the file")
	}
}
