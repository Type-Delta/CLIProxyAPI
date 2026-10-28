package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxRoutingRules      = 256
	maxRoutingDepth      = 8
	maxRoutingNodes      = 64
	maxRoutingTargets    = 64
	maxRoutingRegexRunes = 512
)

// ValidateRoutingRules validates the routing rule tree held in a Config.
func (cfg *Config) ValidateRoutingRules() error {
	if cfg == nil {
		return nil
	}
	return ValidateRoutingRules(cfg.Routing.Rules)
}

func (condition RoutingCondition) MarshalJSON() ([]byte, error) {
	fields := make(map[string]any, 1)
	if condition.All != nil {
		fields["all"] = condition.All
	}
	if condition.Any != nil {
		fields["any"] = condition.Any
	}
	if condition.Not != nil {
		fields["not"] = condition.Not
	}
	if condition.Model != nil {
		fields["model"] = condition.Model
	}
	if condition.APIKey != nil {
		fields["api-key"] = condition.APIKey
	}
	if condition.Header != nil {
		fields["header"] = condition.Header
	}
	if condition.Client != nil {
		fields["client"] = condition.Client
	}
	return json.Marshal(fields)
}

func (condition RoutingCondition) MarshalYAML() (any, error) {
	fields := make(map[string]any, 1)
	if condition.All != nil {
		fields["all"] = condition.All
	}
	if condition.Any != nil {
		fields["any"] = condition.Any
	}
	if condition.Not != nil {
		fields["not"] = condition.Not
	}
	if condition.Model != nil {
		fields["model"] = condition.Model
	}
	if condition.APIKey != nil {
		fields["api-key"] = condition.APIKey
	}
	if condition.Header != nil {
		fields["header"] = condition.Header
	}
	if condition.Client != nil {
		fields["client"] = condition.Client
	}
	return fields, nil
}

// ValidateRoutingRules validates a rule slice and returns the first invalid path.
func ValidateRoutingRules(rules []RoutingRule) error {
	if len(rules) > maxRoutingRules {
		return routingRuleError("routing.rules", fmt.Sprintf("must contain at most %d rules", maxRoutingRules))
	}
	for index, rule := range rules {
		path := fmt.Sprintf("routing.rules[%d]", index)
		if rule.When != nil {
			if err := validateRoutingCondition(rule.When, path+".when", 1, new(int)); err != nil {
				return err
			}
		}
		if len(rule.RouteTo) < 1 || len(rule.RouteTo) > maxRoutingTargets {
			return routingRuleError(path+".route-to", "must contain 1 to 64 non-empty strings")
		}
		for targetIndex, target := range rule.RouteTo {
			if strings.TrimSpace(target) == "" {
				return routingRuleError(fmt.Sprintf("%s.route-to[%d]", path, targetIndex), "must be a non-empty string")
			}
		}
	}
	return nil
}

// ValidateRoutingRulesYAML validates routing.rules in a complete YAML or JSON config.
func ValidateRoutingRulesYAML(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		// The config loader owns syntax errors, including optional-config fallback.
		return nil
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil
	}
	root := expandConfigAliases(document.Content[0])
	routingNode := mappingValue(root, "routing")
	if routingNode == nil {
		return nil
	}
	rulesNode := mappingValue(routingNode, "rules")
	if rulesNode == nil {
		return nil
	}
	return validateRoutingRulesNode(rulesNode, "routing.rules")
}

// ValidateRoutingRulesJSON validates a rule array before JSON decoding can discard unknown fields.
func ValidateRoutingRulesJSON(data []byte) error {
	if !json.Valid(data) {
		return fmt.Errorf("routing.rules: invalid JSON")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("routing.rules: parse rules: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return fmt.Errorf("routing.rules: expected a sequence")
	}
	return validateRoutingRulesNode(document.Content[0], "routing.rules")
}

func validateRoutingCondition(condition *RoutingCondition, path string, depth int, count *int) error {
	if depth > maxRoutingDepth {
		return routingRuleError(path, fmt.Sprintf("condition depth must not exceed %d", maxRoutingDepth))
	}
	*count = *count + 1
	if *count > maxRoutingNodes {
		return routingRuleError(path, fmt.Sprintf("rule must not contain more than %d condition nodes", maxRoutingNodes))
	}
	if condition == nil {
		return routingRuleError(path, "condition node must contain exactly one key")
	}
	branches := 0
	if condition.All != nil {
		branches++
	}
	if condition.Any != nil {
		branches++
	}
	if condition.Not != nil {
		branches++
	}
	if condition.Model != nil {
		branches++
	}
	if condition.APIKey != nil {
		branches++
	}
	if condition.Header != nil {
		branches++
	}
	if condition.Client != nil {
		branches++
	}
	if branches != 1 {
		return routingRuleError(path, "condition node must contain exactly one key")
	}
	switch {
	case condition.All != nil:
		for index := range condition.All {
			if err := validateRoutingCondition(&condition.All[index], fmt.Sprintf("%s.all[%d]", path, index), depth+1, count); err != nil {
				return err
			}
		}
	case condition.Any != nil:
		for index := range condition.Any {
			if err := validateRoutingCondition(&condition.Any[index], fmt.Sprintf("%s.any[%d]", path, index), depth+1, count); err != nil {
				return err
			}
		}
	case condition.Not != nil:
		return validateRoutingCondition(condition.Not, path+".not", depth+1, count)
	case condition.Model != nil:
		return validateRoutingMatch(condition.Model, path+".model")
	case condition.APIKey != nil:
		return validateRoutingMatch(condition.APIKey, path+".api-key")
	case condition.Header != nil:
		if strings.TrimSpace(condition.Header.Name) == "" {
			return routingRuleError(path+".header.name", "header name is required")
		}
		return validateRoutingMatch(&condition.Header.RoutingMatch, path+".header")
	case condition.Client != nil:
		return validateRoutingMatch(condition.Client, path+".client")
	}
	return nil
}

func validateRoutingMatch(match *RoutingMatch, path string) error {
	if match == nil {
		return routingRuleError(path, "must contain exactly one match operator")
	}
	ops := 0
	if match.Is != "" {
		ops++
	}
	if match.Match != "" {
		ops++
	}
	if match.Regex != "" {
		ops++
	}
	if match.In != nil {
		ops++
		if len(match.In) == 0 {
			return routingRuleError(path+".in", "must be a non-empty sequence")
		}
	}
	if ops != 1 {
		return routingRuleError(path, "must contain exactly one match operator")
	}
	if match.Regex != "" {
		if utf8.RuneCountInString(match.Regex) > maxRoutingRegexRunes {
			return routingRuleError(path+".regex", "must be at most 512 characters")
		}
		if _, err := regexp.Compile(match.Regex); err != nil {
			return routingRuleError(path+".regex", "invalid RE2 expression")
		}
	}
	return nil
}

func validateRoutingRulesNode(node *yaml.Node, path string) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return routingRuleError(path, "must be a sequence")
	}
	if len(node.Content) > maxRoutingRules {
		return routingRuleError(path, fmt.Sprintf("must contain at most %d rules", maxRoutingRules))
	}
	for index, rule := range node.Content {
		rulePath := fmt.Sprintf("%s[%d]", path, index)
		if err := validateRoutingRuleNode(rule, rulePath); err != nil {
			return err
		}
	}
	return nil
}

func validateRoutingRuleNode(node *yaml.Node, path string) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return routingRuleError(path, "must be a mapping")
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	var when, targets *yaml.Node
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return routingRuleError(path, "field names must be strings")
		}
		fieldPath := path + "." + key.Value
		if _, ok := seen[key.Value]; ok {
			return routingRuleError(fieldPath, "duplicate field")
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "name":
			if !isStringNode(value) {
				return routingRuleError(fieldPath, "must be a string")
			}
		case "enabled":
			if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return routingRuleError(fieldPath, "must be a boolean")
			}
		case "when":
			when = value
		case "route-to":
			targets = value
		default:
			return routingRuleError(fieldPath, "unknown field")
		}
	}
	if when != nil {
		if err := validateRoutingConditionNode(when, path+".when", 1, new(int)); err != nil {
			return err
		}
	}
	if targets == nil || targets.Kind != yaml.SequenceNode {
		return routingRuleError(path+".route-to", "must be a sequence with 1 to 64 non-empty strings")
	}
	if len(targets.Content) < 1 || len(targets.Content) > maxRoutingTargets {
		return routingRuleError(path+".route-to", "must contain 1 to 64 non-empty strings")
	}
	for index, target := range targets.Content {
		if !isStringNode(target) || strings.TrimSpace(target.Value) == "" {
			return routingRuleError(fmt.Sprintf("%s.route-to[%d]", path, index), "must be a non-empty string")
		}
	}
	return nil
}

func validateRoutingConditionNode(node *yaml.Node, path string, depth int, count *int) error {
	if depth > maxRoutingDepth {
		return routingRuleError(path, fmt.Sprintf("condition depth must not exceed %d", maxRoutingDepth))
	}
	*count = *count + 1
	if *count > maxRoutingNodes {
		return routingRuleError(path, fmt.Sprintf("rule must not contain more than %d condition nodes", maxRoutingNodes))
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return routingRuleError(path, "condition node must be a mapping with exactly one key")
	}
	if len(node.Content) != 2 {
		return routingRuleError(path, "condition node must contain exactly one key")
	}
	key := node.Content[0]
	value := node.Content[1]
	if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
		return routingRuleError(path, "condition key must be a string")
	}
	fieldPath := path + "." + key.Value
	switch key.Value {
	case "all", "any":
		if value == nil || value.Kind != yaml.SequenceNode {
			return routingRuleError(fieldPath, "must be a sequence of conditions")
		}
		for index, child := range value.Content {
			if err := validateRoutingConditionNode(child, fmt.Sprintf("%s[%d]", fieldPath, index), depth+1, count); err != nil {
				return err
			}
		}
		return nil
	case "not":
		return validateRoutingConditionNode(value, fieldPath, depth+1, count)
	case "model", "api-key", "client":
		return validateRoutingMatchNode(value, fieldPath, false)
	case "header":
		return validateRoutingMatchNode(value, fieldPath, true)
	default:
		return routingRuleError(fieldPath, "unknown condition")
	}
}

func validateRoutingMatchNode(node *yaml.Node, path string, header bool) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return routingRuleError(path, "must be a mapping with exactly one operator")
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	ops := 0
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return routingRuleError(path, "field names must be strings")
		}
		fieldPath := path + "." + key.Value
		if _, ok := seen[key.Value]; ok {
			return routingRuleError(fieldPath, "duplicate field")
		}
		seen[key.Value] = struct{}{}
		if header && key.Value == "name" {
			if !isStringNode(value) || strings.TrimSpace(value.Value) == "" {
				return routingRuleError(fieldPath, "header name is required")
			}
			continue
		}
		switch key.Value {
		case "is", "match", "regex":
			ops++
			if !isStringNode(value) {
				return routingRuleError(fieldPath, "must be a string")
			}
			if key.Value == "regex" {
				if utf8.RuneCountInString(value.Value) > maxRoutingRegexRunes {
					return routingRuleError(fieldPath, "must be at most 512 characters")
				}
				if _, err := regexp.Compile(value.Value); err != nil {
					return routingRuleError(fieldPath, "invalid RE2 expression")
				}
			}
		case "in":
			ops++
			if value == nil || value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
				return routingRuleError(fieldPath, "must be a non-empty sequence")
			}
			for index, item := range value.Content {
				if !isStringNode(item) {
					return routingRuleError(fmt.Sprintf("%s[%d]", fieldPath, index), "must be a string")
				}
			}
		default:
			return routingRuleError(fieldPath, "unknown match operator")
		}
	}
	_, hasName := seen["name"]
	if header && !hasName {
		return routingRuleError(path+".name", "header name is required")
	}
	if ops != 1 {
		return routingRuleError(path, "must contain exactly one match operator")
	}
	return nil
}

func mappingValue(node *yaml.Node, name string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index] != nil && node.Content[index].Value == name {
			return node.Content[index+1]
		}
	}
	return nil
}

func isStringNode(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!str"
}

func routingRuleError(path, reason string) error {
	return fmt.Errorf("%s: %s", path, reason)
}
