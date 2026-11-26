package management

import (
	"github.com/MoeForge/moego/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8YAMLSyntaxErrorReportsLine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317, server-id: 161e28b2-e721-408e-aabd-8f72f0404164}\n"
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

	cases := []struct{ name, body, want string }{
		{"unclosed flow sequence", "config-version: 8\n# comment\n\nserver:\n  port: [8317\n", "parse config document 1: yaml: line 5: did not find expected ',' or ']'"},
		{"dedent mismatch", "config-version: 8\nserver:\n   host: a\n  port: 1\n", "parse config document 1: yaml: line 4: did not find expected key"},
		{"tab indentation", "config-version: 8\nserver:\n\tport: 1\n", "parse config document 1: yaml: line 3: found character that cannot start any token"},
		{"unclosed quote", "config-version: 8\nserver: \"oops\nother: 2\n", "parse config document 1: yaml: line 2: found unexpected end of stream"},
		{"one line", "server: [1, 2", "parse config document 1: yaml: line 1: did not find expected ',' or ']'"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v8/management/config.yaml", strings.NewReader(tc.body)))
		var got struct{ Error, Message string }
		if err = json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusBadRequest || got.Error != "invalid_body" || got.Message != tc.want {
			t.Errorf("%s: status=%d got %+v, want message %q", tc.name, recorder.Code, got, tc.want)
		}
	}
	// A later document uses the stream decoder's own document and line.
	later := raw + "---\nserver: [1, 2\n"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v8/management/config.yaml", strings.NewReader(later)))
	_, parserErr := config.DecodeFirstConfigDocument([]byte(later))
	var got struct{ Error, Message string }
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest || got.Error != "invalid_body" ||
		parserErr == nil || !strings.HasPrefix(got.Message, "parse config document 2: ") || got.Message != parserErr.Error() {
		t.Fatalf("later document diagnostic: %d %s", recorder.Code, recorder.Body)
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("invalid YAML changed the file")
	}
}
