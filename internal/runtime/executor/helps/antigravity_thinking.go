package helps

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

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

// ApplyAntigravityRequestThinkingDoc uses the selected account's capabilities
// for grouped models so another account's model registration cannot clamp effort.
func ApplyAntigravityRequestThinkingDoc(doc *ojson.Doc, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, info *registry.ModelInfo) error {
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
	summary := translatedRequestSummaryConfigDoc(doc, req.Payload, original, req.Model, from, "antigravity")
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

// AntigravityWireModel resolves the upstream ID and formats the selected
// effort before payload rules run. A combined ID supplies the implicit effort.
func AntigravityWireModel(doc *ojson.Doc, model string, info *registry.ModelInfo) (string, error) {
	if info == nil || info.AntigravityThinking == nil || info.Thinking == nil {
		return model, nil
	}
	group := info.AntigravityThinking
	const configPath = "request.generationConfig.thinkingConfig."
	level := strings.ToLower(doc.Get(configPath + "thinkingLevel").String())
	hasBudget := doc.Get(configPath + "thinkingBudget").Exists()
	if level == "" {
		level = strings.TrimPrefix(strings.ToLower(model), group.BaseID+"-")
		if strings.EqualFold(model, group.BaseID) {
			level = group.DefaultLevel
		}
	}
	wireID := group.WireModels[level]
	if wireID == "" {
		return "", antigravityThinkingUnavailable(model, level)
	}
	if !hasBudget && doc.Get(configPath+"thinkingLevel").Exists() {
		_ = doc.Set(configPath+"thinkingLevel", strings.ToUpper(level))
	} else if !hasBudget && wireID == group.BaseID {
		// A split-wire alias has to carry the effort that its model ID encoded.
		if !strings.EqualFold(model, group.BaseID) {
			_ = doc.Set(configPath+"thinkingLevel", strings.ToUpper(level))
		}
	}
	return wireID, nil
}

func antigravityThinkingUnavailable(model, level string) error {
	return &cliproxyauth.Error{
		Code:       cliproxyauth.ErrorCodeAntigravityTargetUnavailable,
		Message:    fmt.Sprintf("Antigravity model %s does not offer thinking level %s on this credential", model, level),
		HTTPStatus: http.StatusServiceUnavailable,
	}
}
