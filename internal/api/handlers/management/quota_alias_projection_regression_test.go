package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestFreshFableQuotaProjectsAliasAvailability(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		for _, prefix := range []string{"", "team"} {
			for _, suffix := range []string{"", "(high)"} {
				t.Run(fmt.Sprintf("api-key=%t/prefix=%s/suffix=%s", apiKey, prefix, suffix), func(t *testing.T) {
					fixture := newGSRProbeFixture(t)
					var attributes map[string]string
					if apiKey {
						attributes = map[string]string{"api_key": "alias-test-key"}
					}
					ctx := context.Background()
					h, auth := gsrProbeHandler(t, "claude", attributes)
					auth.Prefix = prefix
					if _, err := h.authManager.Update(ctx, auth); err != nil {
						t.Fatal(err)
					}
					if apiKey {
						h.authManager.SetConfig(&internalconfig.Config{ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "alias-test-key", Prefix: prefix, Models: []internalconfig.ClaudeModel{{Name: "claude-fable-5-1", Alias: "family-only"}}}}})
					} else {
						h.authManager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "claude-fable-5-1", Alias: "family-only"}}})
					}
					route := "family-only"
					if prefix != "" {
						route = prefix + "/" + route
					}
					requested := route + suffix
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: route}})
					t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
					h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "claude"})
					for _, percent := range []int{100, 20} {
						fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"limits":[{"kind":"weekly_scoped","percent":%d,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, percent, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
						quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "", true)
						_, errExecute := h.authManager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: requested}, cliproxyexecutor.Options{})
						exhausted := percent == 100
						if (errExecute != nil) != exhausted {
							t.Fatalf("utilization=%d execution error=%v", percent, errExecute)
						}
						if suspended := reg.IsModelSuspendedForClient(auth.ID, route); suspended != exhausted {
							t.Fatalf("utilization=%d registry suspended=%t, want=%t", percent, suspended, exhausted)
						}
						wantCount := 1
						if exhausted {
							wantCount = 0
						}
						if count := reg.GetModelCount(route); count != wantCount {
							t.Fatalf("utilization=%d registry count=%d, want=%d", percent, count, wantCount)
						}
					}
				})
			}
		}
	}
}
