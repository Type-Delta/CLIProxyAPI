package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// authUnavailableExecutorType marks records for requests that never reached an
// executor because auth selection found no usable credential.
const authUnavailableExecutorType = "auth-selection"

// reportAuthUnavailable publishes a zero-token failure record when auth
// selection fails (all credentials cooling down, none configured). Without it
// these requests leave no analytics trace even though the client saw an error.
func reportAuthUnavailable(ctx context.Context, err error, providers []string, model string) {
	code, message, status, ok := coreauth.AuthUnavailableDetail(err)
	if !ok {
		return
	}
	provider := "unknown"
	if len(providers) > 0 {
		provider = strings.TrimSpace(providers[0])
		if len(providers) > 1 {
			provider = "mixed"
		}
	}
	model = strings.TrimSpace(model)
	alias := strings.TrimSpace(coreusage.RequestedModelAliasFromContext(ctx))
	if alias == "" {
		alias = model
	}
	coreusage.PublishRecord(ctx, coreusage.Record{
		Provider:        provider,
		ExecutorType:    authUnavailableExecutorType,
		Model:           model,
		Alias:           alias,
		APIKey:          helps.APIKeyFromContext(ctx),
		ReasoningEffort: coreusage.ReasoningEffortFromContext(ctx),
		ServiceTier:     coreusage.ServiceTierFromContext(ctx),
		Generate:        coreusage.GenerateFlag(false),
		RequestedAt:     time.Now(),
		Failed:          true,
		Fail: coreusage.Failure{
			StatusCode: status,
			Body:       strings.TrimSpace(code + ": " + message),
		},
	})
}
