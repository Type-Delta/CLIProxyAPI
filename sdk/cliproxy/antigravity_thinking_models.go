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
	blocked := make(map[string]struct{})
	for _, baseID := range slices.Sorted(maps.Keys(hints.ThinkingGroups)) {
		levels := hints.ThinkingGroups[baseID]
		variants := effortVariantIDs(baseID, levels)
		baseTemplate := byID[baseID]
		template := baseTemplate
		var availableTemplate bool
		for _, id := range variants {
			variant := byID[id]
			if variant == nil {
				continue
			}
			if template == nil {
				template = variant
			}
			if len(applyExcludedModels([]*ModelInfo{variant}, excluded)) != 0 {
				availableTemplate = true
			}
		}
		if template == nil || len(levels) == 0 {
			continue
		}
		// Some families appear in the management catalog only as effort variants.
		// Excluding every listed variant also disables the generated family routes.
		if len(applyExcludedModels([]*ModelInfo{{ID: baseID}}, excluded)) == 0 || (baseTemplate == nil && !availableTemplate) {
			blocked[baseID] = struct{}{}
			for _, id := range variants {
				blocked[id] = struct{}{}
			}
			continue
		}
		_, baseOnWire := hints.ModelIDs[baseID]
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
	return slices.DeleteFunc(models, func(model *ModelInfo) bool {
		_, excluded := blocked[model.ID]
		return excluded
	})
}

func effortVariantIDs(baseID string, levels []string) []string {
	ids := make([]string, len(levels))
	for i, level := range levels {
		ids[i] = baseID + "-" + level
	}
	return ids
}
