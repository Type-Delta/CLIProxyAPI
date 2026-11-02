package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigV8WritesPreserveAPIKeyGroupNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	families := []string{"gemini", "interactions", "codex", "xai", "meta", "claude", "vertex", "openai-compatibility"}
	raw := "config-version: 8\napi-keys:\n"
	for _, family := range families {
		raw += fmt.Sprintf("  %s:\n    - name: %s-group\n      base-url: https://api.example.invalid\n      keys:\n        - api-key: dummy-first\n        - api-key: dummy-second\n", family, family)
	}
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: cfg, configFilePath: path}
			router := gin.New()
			router.Handle(method, "/v8/management/config", h.ConfigV8)
			body := `{"server":{"port":8318}}`
			if method == http.MethodPut {
				var tree map[string]any
				if err := yaml.Unmarshal([]byte(raw), &tree); err != nil {
					t.Fatal(err)
				}
				tree["server"] = map[string]any{"port": 8318}
				data, err := json.Marshal(tree)
				if err != nil {
					t.Fatal(err)
				}
				body = string(data)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v8/management/config", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.ValidateV8Config(saved); err != nil {
				t.Fatal(err)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(saved, &doc); err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				groups := configV8Node(doc.Content[0], []string{"api-keys", family})
				if groups == nil || len(groups.Content) != 1 {
					t.Fatalf("lost %s group", family)
				}
				group := groups.Content[0]
				if name := configV8Node(group, []string{"name"}); name == nil || name.Value != family+"-group" {
					t.Fatalf("lost %s name", family)
				}
				keys := configV8Node(group, []string{"keys"})
				if keys == nil || len(keys.Content) != 2 {
					t.Fatalf("lost %s keys", family)
				}
				for _, key := range keys.Content {
					if configV8Node(key, []string{"name"}) != nil {
						t.Fatalf("%s name leaked into key", family)
					}
				}
			}
			reloaded, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Port != 8318 {
				t.Fatalf("port=%d", reloaded.Port)
			}
			if reloaded.GeminiKey[0].Name != "gemini-group" || reloaded.CodexKey[0].Name != "codex-group" || reloaded.ClaudeKey[0].Name != "claude-group" {
				t.Fatal("group names lost on reload")
			}
		})
	}
}
