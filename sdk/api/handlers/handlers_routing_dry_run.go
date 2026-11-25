package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

// RoutingDryRunResult is the projected local inference response. Null fields
// indicate that a value does not apply or CPA cannot predict it.
type RoutingDryRunResult struct {
	Body       *string                  `json:"body"`
	RetryAfter *int64                   `json:"retry_after"`
	Credential *RoutingDryRunCredential `json:"credential"`
	Status     *int                     `json:"status"`
	Success    *bool                    `json:"success"`
	ErrorClass *string                  `json:"error_class"`
}

type RoutingDryRunCredential struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ProjectRoutingDryRunResult formats a local projection as an OpenAI-compatible
// inference response. Candidate labels should be resolved before calling it.
func (h *BaseAPIHandler) ProjectRoutingDryRunResult(req routing.DryRunRequest, projection *coreauth.DryRunProjection) RoutingDryRunResult {
	trace := projection.Trace
	if trace.DecidedBy == routing.DecidedByHome {
		return RoutingDryRunResult{}
	}
	allowImageModel := strings.EqualFold(strings.TrimSpace(req.SourceFormat), "openai-image")
	_, _, requestError := h.getRequestDetailsWithOptions(strings.TrimSpace(req.Model), allowImageModel, false)
	msg := requestError
	class := ""
	if msg == nil && projection.Error != nil {
		err := projection.Error
		if code, _, _, ok := coreauth.AuthUnavailableDetail(err); ok {
			class = code
		} else {
			var authErr *coreauth.Error
			if errors.As(err, &authErr) && authErr != nil {
				class = authErr.Code
			}
		}
		msg = executionErrorMessage(enrichAuthSelectionError(err, projection.Providers, projection.Model))
	}
	if msg != nil {
		return routingDryRunFailure(msg, class)
	}
	predicted := trace.PickPredicted && trace.Pick >= 0 && trace.Pick < len(trace.Candidates)
	if !predicted && (trace.StrategyStep == nil || trace.StrategyStep.Fallback != routing.FallbackMissingRecoveryDataRefreshPending) {
		return RoutingDryRunResult{}
	}
	body, status, success := "success", http.StatusOK, true
	result := RoutingDryRunResult{Body: &body, Status: &status, Success: &success}
	if predicted {
		pick := trace.Candidates[trace.Pick]
		result.Credential = &RoutingDryRunCredential{ID: pick.ID, Label: pick.Label}
	}
	return result
}

func routingDryRunFailure(msg *interfaces.ErrorMessage, class string) RoutingDryRunResult {
	status, success := msg.StatusCode, false
	body := string(BuildErrorResponseBodyWithError(status, msg.Error.Error(), msg.Error))
	if class == "" {
		class = ojson.Get(body, "error.code").String()
	}
	if class == "" {
		class = ojson.Get(body, "error.type").String()
	}
	result := RoutingDryRunResult{Body: &body, Status: &status, Success: &success}
	if class != "" {
		result.ErrorClass = &class
	}
	if seconds, err := strconv.ParseInt(coreauth.SafeResponseHeaders(msg.Error).Get("Retry-After"), 10, 64); err == nil {
		result.RetryAfter = &seconds
	}
	return result
}
