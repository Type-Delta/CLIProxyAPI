package helps

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeStableBillingValidatesIndependentFields(t *testing.T) {
	ua := "claude-cli/2.1.322 (external, sdk-ts)"
	for _, text := range []string{
		"x-anthropic-billing-header: cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_entrypoint=sdk-ts; cc_version=2.1.318.abc;",
		"x-anthropic-billing-header: cc_version=2.1.322.abc; cc_version=2.1.318.abc; cc_entrypoint=sdk-ts;",
	} {
		for _, field := range []string{`[{"text":%q}]`, `%q`} {
			body := []byte(`{"system":` + fmt.Sprintf(field, text) + `}`)
			if err := ValidateClaudeDeviceProfileBilling(body, ua); err == nil {
				t.Errorf("contradictory independent field was accepted: %s", body)
			}
		}

	}
}

func TestClaudeStablePersistentCustomFallbackCanLearnNative(t *testing.T) {
	for _, fallbackUA := range []string{"my-gateway/1.0", "claude-cli/2.1.280 (external, cli)"} {
		t.Run(fallbackUA, func(t *testing.T) {
			profileStore(t, true)
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: fallbackUA, PackageVersion: "custom-pkg", RuntimeVersion: "custom-runtime"}}
			first := resolveProfile(t, nil, cfg)
			if again := resolveProfile(t, nil, cfg); again != first {
				t.Fatalf("fallback changed on read: %+v", again)
			}
			native := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts)")
			if got := resolveProfile(t, native, cfg); got.UserAgent != native.Get("User-Agent") || got.fallback {
				t.Fatalf("custom fallback did not learn native tuple: %+v", got)
			}
		})
	}
}

func TestClaudeStableCustomFallbackPreservesKnownSoftwareVersions(t *testing.T) {
	profileStore(t, true)
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "my-gateway/1.0", PackageVersion: "0.130.0", RuntimeVersion: "v27.0.0"}}
	first := resolveProfile(t, nil, cfg)
	native := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts)")
	if got := resolveProfile(t, native, cfg); got != first {
		t.Fatalf("known SDK/runtime downgraded from custom fallback: %+v", got)
	}
	native.Set("X-Stainless-Package-Version", "0.131.0")
	native.Set("X-Stainless-Runtime-Version", "v27.1.0")
	if got := resolveProfile(t, native, cfg); got.UserAgent != native.Get("User-Agent") || got.fallback {
		t.Fatalf("eligible native tuple did not establish profile: %+v", got)
	}
}

func TestClaudeStableFallbackConfigUpgradesWithoutDowngrades(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[persistent], func(t *testing.T) {
			profileStore(t, persistent)
			initial := resolveProfile(t, nil, nil)
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "claude-cli/2.1.322 (external, cli)", PackageVersion: "0.128.0", RuntimeVersion: "v26.12.1", OS: "Linux", Arch: "x64"}}
			upgraded := resolveProfile(t, nil, cfg)
			if upgraded.UserAgent != cfg.ClaudeHeaderDefaults.UserAgent || upgraded.PackageVersion != "0.128.0" || upgraded.RuntimeVersion != "v26.12.1" || upgraded.OS != initial.OS || upgraded.Arch != initial.Arch {
				t.Fatalf("fallback config upgrade not coherent/device-pinned: %+v", upgraded)
			}
			if got := resolveProfile(t, nil, nil); got != upgraded {
				t.Fatalf("fallback config downgraded: %+v", got)
			}
		})
	}
}
