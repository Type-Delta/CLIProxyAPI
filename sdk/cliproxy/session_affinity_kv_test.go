package cliproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestSessionAffinityKVSurvivesStoreRestart(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Errorf("close KV: %v", errShutdown)
		}
	})
	cfg := &internalconfig.Config{
		AuthDir: t.TempDir(),
		KVStore: internalconfig.KVStoreConfig{Path: filepath.Join(t.TempDir(), "kv.db")},
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	}
	svc := &Service{}
	svc.applyKVStoreConfig(cfg)
	first := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(first.Stop)
	opts := executor.Options{Headers: http.Header{"X-Session-Id": {"restart-session"}}}
	accountA := &coreauth.Auth{ID: "account-a", Provider: "codex"}
	accountB := &coreauth.Auth{ID: "account-b", Provider: "codex"}
	selected, errPick := first.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB})
	if errPick != nil || selected.ID != accountB.ID {
		t.Fatalf("initial selection = %v, %v; want account-b", selected, errPick)
	}
	// Reopen without stopping/flushing the selector, as after a process crash.
	if errShutdown := svc.shutdownKVStore(); errShutdown != nil {
		t.Fatal(errShutdown)
	}

	svc.applyKVStoreConfig(cfg)
	restarted := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(restarted.Stop)
	selected, errPick = restarted.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountA, accountB})
	if errPick != nil || selected.ID != accountB.ID {
		t.Fatalf("selection after KV reopen = %v, %v; want original account-b instead of fresh account-a", selected, errPick)
	}
}

func TestHistoryAffinityKVSurvivesStoreRestart(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	cfg := &internalconfig.Config{
		AuthDir: t.TempDir(),
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	}
	svc := &Service{}
	svc.applyKVStoreConfig(cfg)
	first := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(first.Stop)
	opts := executor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{executor.CallerScopeMetadataKey: "restart-test-caller"},
		OriginalRequest: []byte(`{"messages":[{"role":"system","content":"history affinity"},{"role":"user","content":"unique opening question"}]}`),
	}
	accountA := &coreauth.Auth{ID: "account-a", Provider: "codex"}
	accountB := &coreauth.Auth{ID: "account-b", Provider: "codex"}
	if _, errPick := first.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB}); errPick != nil {
		t.Fatal(errPick)
	}
	if errShutdown := svc.shutdownKVStore(); errShutdown != nil {
		t.Fatal(errShutdown)
	}
	svc.applyKVStoreConfig(cfg)
	restarted := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(restarted.Stop)
	opts.OriginalRequest = []byte(`{"messages":[{"role":"system","content":"history affinity"},{"role":"user","content":"unique opening question"},{"role":"assistant","content":"first answer"},{"role":"user","content":"continue"}]}`)
	opts.Metadata = map[string]any{executor.CallerScopeMetadataKey: "restart-test-caller"}
	selected, errPick := restarted.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountA, accountB})
	if errPick != nil || selected.ID != accountB.ID {
		t.Fatalf("history continuation after KV reopen = %v, %v; want account-b", selected, errPick)
	}
}

func TestSessionAffinityKVDisabledKeepsMemoryOnly(t *testing.T) {
	cfg := &internalconfig.Config{
		KVStore: internalconfig.KVStoreConfig{Disabled: true},
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	}
	first := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(first.Stop)
	opts := executor.Options{Headers: http.Header{"X-Session-Id": {"memory-only"}}}
	accountA := &coreauth.Auth{ID: "account-a", Provider: "codex"}
	accountB := &coreauth.Auth{ID: "account-b", Provider: "codex"}
	if _, errPick := first.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB}); errPick != nil {
		t.Fatal(errPick)
	}
	restarted := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(restarted.Stop)
	selected, errPick := restarted.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountA, accountB})
	if errPick != nil || selected.ID != accountA.ID {
		t.Fatalf("disabled KV restored a binding: %v, %v", selected, errPick)
	}
}

func TestSessionAffinityKVRoutingReloadKeepsBinding(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	cfg := &internalconfig.Config{
		AuthDir: t.TempDir(),
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	svc := &Service{coreManager: manager}
	svc.applyKVStoreConfig(cfg)
	if !svc.applyConfigRuntime(context.Background(), svc.commitConfigUpdate(cfg), false) {
		t.Fatal("initial routing config failed")
	}
	first := manager.Selector().(*coreauth.SessionAffinitySelector)
	opts := executor.Options{Headers: http.Header{"X-Session-Id": {"reload-session"}}}
	accountA := &coreauth.Auth{ID: "account-a", Provider: "codex"}
	accountB := &coreauth.Auth{ID: "account-b", Provider: "codex"}
	if _, errPick := first.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB}); errPick != nil {
		t.Fatal(errPick)
	}
	reloaded := cfg.CloneForRuntime()
	reloaded.Routing.SessionAffinityRebindOn = "quota"
	if !svc.applyConfigRuntime(context.Background(), svc.commitConfigUpdate(reloaded), false) {
		t.Fatal("routing reload failed")
	}
	current := manager.Selector().(*coreauth.SessionAffinitySelector)
	t.Cleanup(current.Stop)
	if first == current {
		t.Fatal("fixture did not replace the selector")
	}
	selected, errPick := current.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountA, accountB})
	if errPick != nil || selected.ID != accountB.ID {
		t.Fatalf("routing reload lost affinity: %v, %v", selected, errPick)
	}
}

func TestSessionAffinityKVPathChangeAfterSelectorReplacement(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	cfg := &internalconfig.Config{
		AuthDir: t.TempDir(),
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	}
	svc := &Service{}
	svc.applyKVStoreConfig(cfg)
	first := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(first.Stop)
	opts := executor.Options{Headers: http.Header{"X-Session-Id": {"path-change"}}}
	accountA := &coreauth.Auth{ID: "account-a", Provider: "codex"}
	accountB := &coreauth.Auth{ID: "account-b", Provider: "codex"}
	if _, errPick := first.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB}); errPick != nil {
		t.Fatal(errPick)
	}
	updated := cfg.CloneForRuntime()
	updated.KVStore.Path = filepath.Join(t.TempDir(), "different.db")
	replacement := newRoutingSelector(normalizedRoutingRuntimeState(updated)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(replacement.Stop)
	// Requests can reach the replacement before config application opens the new KV.
	if _, errPick := replacement.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB}); errPick != nil {
		t.Fatal(errPick)
	}
	svc.applyKVStoreConfig(updated)
	selected, errPick := replacement.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountA, accountB})
	if errPick != nil || selected.ID != accountA.ID {
		t.Fatalf("new KV reused the closed previous database: %v, %v", selected, errPick)
	}
	restarted := newRoutingSelector(normalizedRoutingRuntimeState(updated)).(*coreauth.SessionAffinitySelector)
	t.Cleanup(restarted.Stop)
	selected, errPick = restarted.Pick(context.Background(), "codex", "gpt-test", opts, []*coreauth.Auth{accountB, accountA})
	if errPick != nil || selected.ID != accountA.ID {
		t.Fatalf("replacement failed to write the new KV: %v, %v", selected, errPick)
	}
}
