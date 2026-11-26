package helps

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// AntigravityRequestModel holds the selected upstream model and the explicit
// thinking level used for the selection on this attempt.
type AntigravityRequestModel struct {
	cliproxyauth.AntigravityModelSelection
	Level thinking.ThinkingLevel
	group *registry.ModelInfo
	Err   error
}

// SelectAntigravityRequestModel reads caller intent before translation or static
// thinking validation can insert defaults or clamp an explicit level.
func SelectAntigravityRequestModel(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, baseModel, from string, source []byte, opts cliproxyexecutor.Options, countTokens bool) AntigravityRequestModel {
	if from == "" {
		from = "antigravity"
	}
	level := thinking.RequestedAntigravityThinkingLevel(source, from, model)
	selection := cliproxyauth.AntigravityModelSelection{ID: baseModel}
	if level != "" || !countTokens {
		selection = cliproxyauth.SelectAntigravityModel(ctx, auth, baseModel, string(level), cfg)
	}
	result := AntigravityRequestModel{AntigravityModelSelection: selection, Level: level}
	info := AntigravityGroupedModel(auth, baseModel, opts)
	if info == nil {
		return result
	}
	group := info.AntigravityThinking
	levelName := string(level)
	if levelName == "" || (level == thinking.LevelNone && group.WireModels[levelName] == "") {
		if strings.EqualFold(baseModel, group.BaseID) {
			levelName = group.DefaultLevel
		} else {
			levelName = strings.TrimPrefix(strings.ToLower(baseModel), group.BaseID+"-")
		}
	}
	wire := group.WireModels[levelName]
	if wire == "" {
		if thinking.HasLevel(group.KnownLevels, levelName) {
			result.Err = antigravityThinkingUnavailable(model, levelName)
		}
		return result
	}
	result.group = info
	result.Level = thinking.ThinkingLevel(levelName)
	// Prefer the catalog match for a published combined wire ID, including
	// its output cap and explicit search capability.
	if wire != group.BaseID && levelName != string(level) {
		selection = cliproxyauth.SelectAntigravityModel(ctx, auth, wire, levelName, cfg)
	}
	if selection.MaxOutputTokens > 0 && selection.ID == wire {
		result.AntigravityModelSelection = selection
		return result
	}
	result.AntigravityModelSelection = cliproxyauth.SelectAntigravityModel(ctx, auth, wire, "", cfg)
	return result
}

// AntigravityGroupedModel looks up the catalog published for the selected
// credential, including routes exposed under a prefix or alias.
func AntigravityGroupedModel(auth *cliproxyauth.Auth, model string, opts cliproxyexecutor.Options) *registry.ModelInfo {
	if auth == nil {
		return nil
	}
	requested, _ := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string)
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{model, thinking.ParseSuffix(requested).ModelName, auth.Prefix + "/" + model} {
		info := reg.ClientModelInfo(auth.ID, id)
		if info == nil || info.AntigravityThinking == nil {
			continue
		}
		group := info.AntigravityThinking
		modelID := strings.ToLower(model)
		level, legacyVariant := strings.CutPrefix(modelID, group.BaseID+"-")
		if modelID == group.BaseID || (legacyVariant && group.WireModels[level] != "") {
			info.ID = modelID
			return info
		}
	}
	return nil
}

func antigravityThinkingUnavailable(model, level string) error {
	return &cliproxyauth.Error{
		Code:       cliproxyauth.ErrorCodeAntigravityTargetUnavailable,
		Message:    fmt.Sprintf("Antigravity model %s does not offer thinking level %s on this credential", model, level),
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// TranslationModelInfo uses the selected snapshot's explicit search capability
// when changing models. A missing capability is not a hint to consult the registry.
func (model AntigravityRequestModel) TranslationModelInfo(req cliproxyexecutor.Request, baseModel string, from sdktranslator.Format) *registry.ModelInfo {
	if model.group != nil && model.MaxOutputTokens == 0 {
		return model.group
	}
	if model.ID == baseModel && model.MaxOutputTokens == 0 {
		if from == sdktranslator.FormatClaude {
			// Keep the legacy Claude capability lookup when no variant was selected.
			return nil
		}
		info, _ := cliproxyauth.ResolvedModelInfo(req)
		return info
	}
	return &registry.ModelInfo{ID: model.ID, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &model.WebSearch}}
}

// CapAntigravityMaxOutputTokensDoc preserves a smaller caller limit and keeps
// Claude thinking budgets below the effective output limit.
func CapAntigravityMaxOutputTokensDoc(doc *ojson.Doc, limit int, isClaudeModel bool) {
	if doc == nil || limit <= 0 {
		return
	}
	effectiveMaxOutputTokens := int64(limit)
	for _, path := range []string{
		"request.generationConfig.maxOutputTokens",
		"request.generationConfig.max_output_tokens",
	} {
		value := doc.Get(path)
		if value.Exists() && value.Type == ojson.Number && value.Int() > 0 && value.Int() < effectiveMaxOutputTokens {
			effectiveMaxOutputTokens = value.Int()
		}
	}
	_ = doc.Delete("request.generationConfig.max_output_tokens")
	_ = doc.Set("request.generationConfig.maxOutputTokens", effectiveMaxOutputTokens)
	if !isClaudeModel {
		return
	}
	for _, budgetPath := range []string{
		"request.generationConfig.thinkingConfig.thinkingBudget",
		"request.generationConfig.thinkingConfig.thinking_budget",
	} {
		budget := doc.Get(budgetPath)
		if budget.Exists() && budget.Int() >= effectiveMaxOutputTokens {
			_ = doc.Set(budgetPath, effectiveMaxOutputTokens-1)
		}
	}
}

// ApplyAntigravityRequestThinkingDoc formats a selected level with the canonical
// provider applier while leaving unmatched requests on the original path.
func ApplyAntigravityRequestThinkingDoc(doc *ojson.Doc, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model AntigravityRequestModel) error {
	from := opts.SourceFormat.String()
	if model.Err != nil {
		return model.Err
	}
	if model.group != nil && model.MaxOutputTokens == 0 {
		if err := applyPublishedAntigravityThinkingDoc(doc, req, opts, model.group); err != nil {
			return err
		}
		const path = "request.generationConfig.thinkingConfig"
		group := model.group.AntigravityThinking
		if model.Level != "" && (doc.Get(path+".thinkingLevel").Exists() || (model.ID == group.BaseID && !strings.EqualFold(group.BaseID, req.Model))) && !doc.Get(path+".thinkingBudget").Exists() {
			_ = doc.Set(path+".thinkingLevel", strings.ToUpper(string(model.Level)))
		}
		return nil
	}
	if model.MaxOutputTokens == 0 {
		return ApplyRequestThinkingDoc(doc, req, opts, from, "antigravity", "antigravity")
	}
	if from == "" {
		from = "antigravity"
	}
	level := string(model.Level)
	modelInfo := &registry.ModelInfo{ID: model.ID, Thinking: &registry.ThinkingSupport{Levels: []string{level}}}
	originalSource := opts.OriginalRequest
	if len(originalSource) == 0 {
		originalSource = req.Payload
	}
	summary := translatedRequestSummaryConfigDoc(sdktranslator.Default(), doc, req.Payload, originalSource, req.Model, from, "antigravity")
	_ = doc.Delete("request.generationConfig.thinking_config")
	return thinking.ApplyThinkingWithModelInfoAndSummaryDoc(doc, req.Payload, model.ID+"("+level+")", from, "antigravity", "antigravity", modelInfo, summary)
}

func applyPublishedAntigravityThinkingDoc(doc *ojson.Doc, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, info *registry.ModelInfo) error {
	from := opts.SourceFormat.String()
	if info == nil {
		return ApplyRequestThinkingDoc(doc, req, opts, from, "antigravity", "antigravity")
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	level := thinking.RequestedAntigravityThinkingLevel(req.Payload, from, req.Model)
	group := info.AntigravityThinking
	if level != "" && level != thinking.LevelNone && thinking.HasLevel(group.KnownLevels, string(level)) && group.WireModels[string(level)] == "" {
		return antigravityThinkingUnavailable(req.Model, string(level))
	}
	summary := translatedRequestSummaryConfigDoc(sdktranslator.Default(), doc, req.Payload, original, req.Model, from, "antigravity")
	if err := thinking.ApplyThinkingWithModelInfoAndSummaryDoc(doc, req.Payload, req.Model, from, "antigravity", "antigravity", info, summary); err != nil {
		return err
	}
	if level == thinking.LevelNone && group.WireModels[string(level)] != "" {
		const path = "request.generationConfig.thinkingConfig"
		_ = doc.Delete(path + ".thinkingBudget")
		_ = doc.Set(path+".thinkingLevel", "NONE")
	}
	return nil
}
