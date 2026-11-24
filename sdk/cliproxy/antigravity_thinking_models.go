package cliproxy

import (
	"maps"
	"slices"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// appendAntigravityThinkingModels gives known families both a base route and
// their legacy effort routes when the account advertises a usable wire model.
func appendAntigravityThinkingModels(models, templates []*ModelInfo, hints antigravityModelCapabilityHints, excluded []string) []*ModelInfo {
	if len(hints.ThinkingGroups) == 0 {
		return models
	}
	byID := make(map[string]*ModelInfo, len(templates))
	for _, model := range templates {
		if model != nil {
			byID[model.ID] = model
		}
	}
	registered := make(map[string]int, len(models))
	for i, model := range models {
		registered[model.ID] = i
	}
	for _, baseID := range slices.Sorted(maps.Keys(hints.ThinkingGroups)) {
		levels := hints.ThinkingGroups[baseID]
		template := byID[baseID]
		for _, level := range levels {
			if template == nil {
				template = byID[baseID+"-"+level]
			}
		}
		if template == nil || len(levels) == 0 {
			continue
		}
		_, baseOnWire := hints.ModelIDs[baseID]
		if baseOnWire && len(applyExcludedModels([]*ModelInfo{{ID: baseID}}, excluded)) == 0 {
			continue
		}
		group := &registry.AntigravityThinking{
			BaseID: baseID, KnownLevels: slices.Clone(levels), WireModels: make(map[string]string),
		}
		if template.Thinking != nil {
			group.KnownLevels = append(group.KnownLevels, template.Thinking.Levels...)
		}
		availableLevels := make([]string, 0, len(levels))
		for _, level := range levels {
			id := baseID + "-" + level
			_, variantOnWire := hints.ModelIDs[id]
			if (!baseOnWire && !variantOnWire) || len(applyExcludedModels([]*ModelInfo{{ID: id}}, excluded)) == 0 {
				continue
			}
			availableLevels = append(availableLevels, level)
			wireID := id
			if baseOnWire {
				wireID = baseID
			}
			group.WireModels[level] = wireID
		}
		if len(availableLevels) == 0 {
			continue
		}
		group.DefaultLevel = availableLevels[0]
		if slices.Contains(availableLevels, "medium") {
			group.DefaultLevel = "medium"
		}
		for _, id := range append([]string{baseID}, effortVariantIDs(baseID, availableLevels)...) {
			source := byID[id]
			if source == nil {
				source = template
			}
			model := cloneModelInfoForCatalogRoute(source)
			model.ID = id
			model.Name = rewriteModelInfoName(model.Name, source.ID, id)
			model.AntigravityThinking = group
			if model.Thinking != nil {
				thinking := *model.Thinking
				thinking.Levels = availableLevels
				model.Thinking = &thinking
			}
			if index, exists := registered[id]; exists {
				models[index] = &model
			} else {
				registered[id] = len(models)
				models = append(models, &model)
			}
		}
	}
	return models
}

func effortVariantIDs(baseID string, levels []string) []string {
	ids := make([]string, len(levels))
	for i, level := range levels {
		ids[i] = baseID + "-" + level
	}
	return ids
}
