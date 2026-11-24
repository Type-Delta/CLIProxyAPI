package helps

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type claudeDeviceProfileContextKey struct{}

// PrepareClaudeDeviceProfile resolves one request snapshot for billing and headers.
// Callers decide whether their wire policy enables stabilization before calling it.
func PrepareClaudeDeviceProfile(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config, confirmedNative bool) (context.Context, error) {
	if !confirmedNative {
		headers = nil
	}
	profile, errProfile := ResolveClaudeDeviceProfileRequired(ctx, auth, apiKey, headers, cfg)
	if errProfile != nil {
		return ctx, errProfile
	}
	return context.WithValue(ctx, claudeDeviceProfileContextKey{}, profile), nil
}

// ClaudeDeviceProfileFromContext returns the snapshot shared by this request's
// payload preparation and header emission, even if another request upgrades it.
func ClaudeDeviceProfileFromContext(ctx context.Context) (ClaudeDeviceProfile, bool) {
	profile, ok := ctx.Value(claudeDeviceProfileContextKey{}).(ClaudeDeviceProfile)
	return profile, ok
}
