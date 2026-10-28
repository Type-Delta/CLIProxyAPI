package routing

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type matchOperation uint8

const (
	matchEqual matchOperation = iota + 1
	matchSet
	matchGlob
	matchRegex
)

type compiledMatch struct {
	operation matchOperation
	values    []string
	pattern   *regexp.Regexp
	never     bool
}

type conditionKind uint8

const (
	conditionAll conditionKind = iota + 1
	conditionAny
	conditionNot
	conditionModel
	conditionAPIKey
	conditionHeader
	conditionClient
)

type compiledCondition struct {
	kind     conditionKind
	children []compiledCondition
	child    *compiledCondition
	match    compiledMatch
	header   string
}

// RuleSet is a compiled, immutable set of routing rules. A nil RuleSet has no rules.
type RuleSet struct {
	rules []compiledRule
}

type compiledRule struct {
	index     int
	name      string
	condition *compiledCondition
	rendered  string
	targets   []string
}

type apiKeyLabel struct {
	label string
	id    string
}

// Compile validates and compiles cfg.Routing.Rules. Warnings report non-fatal issues
// such as unknown API-key labels; the error reports the first invalid field path.
func Compile(cfg *config.Config) (*RuleSet, []string, error) {
	set := &RuleSet{}
	if cfg == nil {
		return set, nil, nil
	}
	if err := config.ValidateRoutingRules(cfg.Routing.Rules); err != nil {
		return nil, nil, err
	}
	labels := make([]apiKeyLabel, 0, len(cfg.APIKeys))
	for _, entry := range cfg.APIKeys {
		if entry.Label == "" {
			continue
		}
		labels = append(labels, apiKeyLabel{label: entry.Label, id: config.APIKeyID(entry.Key)})
	}
	set.rules = make([]compiledRule, 0, len(cfg.Routing.Rules))
	warnings := make([]string, 0)
	for index, rule := range cfg.Routing.Rules {
		if rule.Enabled != nil && !*rule.Enabled {
			continue
		}
		compiled := compiledRule{
			index:    index,
			name:     rule.Name,
			rendered: renderRuleCondition(rule.When),
			targets:  append([]string(nil), rule.RouteTo...),
		}
		if rule.When != nil {
			condition, foundWarnings, err := compileCondition(rule.When, fmt.Sprintf("routing.rules[%d].when", index), labels)
			if err != nil {
				return nil, nil, err
			}
			compiled.condition = condition
			warnings = append(warnings, foundWarnings...)
		}
		set.rules = append(set.rules, compiled)
	}
	return set, warnings, nil
}

// Len returns the number of enabled compiled rules.
func (rs *RuleSet) Len() int {
	if rs == nil {
		return 0
	}
	return len(rs.rules)
}

// Decide evaluates the rules against attrs and narrows eligible, the Auth IDs that
// passed the routing gate across all priority tiers.
func (rs *RuleSet) Decide(attrs RequestAttrs, eligible []string) Decision {
	decision := Decision{Applied: -1}
	if rs == nil || len(rs.rules) == 0 {
		return decision
	}
	for index := range rs.rules {
		rule := &rs.rules[index]
		if rule.condition != nil && !matchesCondition(rule.condition, attrs) {
			continue
		}
		if decision.Matched == nil {
			decision.Matched = make([]RuleMatch, 0, len(rs.rules))
		}
		decision.Matched = append(decision.Matched, RuleMatch{
			Index:     rule.index,
			Name:      rule.name,
			Condition: rule.rendered,
			Targets:   append([]string(nil), rule.targets...),
		})
	}
	if len(decision.Matched) == 0 {
		return decision
	}

	appliedPosition := -1
	for index := len(decision.Matched) - 1; index >= 0; index-- {
		if hasEligibleTarget(decision.Matched[index].Targets, eligible) {
			appliedPosition = index
			decision.Applied = decision.Matched[index].Index
			break
		}
	}
	for index := range decision.Matched {
		switch {
		case index == appliedPosition:
			decision.Matched[index].Status = RuleApplied
		case appliedPosition >= 0 && index < appliedPosition:
			decision.Matched[index].Status = RuleOverridden
		default:
			decision.Matched[index].Status = RuleFellThrough
		}
	}
	if appliedPosition >= 0 {
		targets := decision.Matched[appliedPosition].Targets
		for _, id := range eligible {
			if containsString(targets, id) && !containsString(decision.Allowed, id) {
				decision.Allowed = append(decision.Allowed, id)
			}
		}
	}
	return decision
}

func compileCondition(condition *config.RoutingCondition, path string, labels []apiKeyLabel) (*compiledCondition, []string, error) {
	compiled := &compiledCondition{}
	warnings := make([]string, 0)
	switch {
	case condition.All != nil:
		compiled.kind = conditionAll
		compiled.children = make([]compiledCondition, 0, len(condition.All))
		for index := range condition.All {
			child, childWarnings, err := compileCondition(&condition.All[index], fmt.Sprintf("%s.all[%d]", path, index), labels)
			if err != nil {
				return nil, nil, err
			}
			compiled.children = append(compiled.children, *child)
			warnings = append(warnings, childWarnings...)
		}
	case condition.Any != nil:
		compiled.kind = conditionAny
		compiled.children = make([]compiledCondition, 0, len(condition.Any))
		for index := range condition.Any {
			child, childWarnings, err := compileCondition(&condition.Any[index], fmt.Sprintf("%s.any[%d]", path, index), labels)
			if err != nil {
				return nil, nil, err
			}
			compiled.children = append(compiled.children, *child)
			warnings = append(warnings, childWarnings...)
		}
	case condition.Not != nil:
		compiled.kind = conditionNot
		child, childWarnings, err := compileCondition(condition.Not, path+".not", labels)
		if err != nil {
			return nil, nil, err
		}
		compiled.child = child
		warnings = append(warnings, childWarnings...)
	case condition.Model != nil:
		compiled.kind = conditionModel
		compiled.match, warnings = compileMatch(condition.Model, path+".model", false, labels)
	case condition.APIKey != nil:
		compiled.kind = conditionAPIKey
		compiled.match, warnings = compileMatch(condition.APIKey, path+".api-key", true, labels)
	case condition.Header != nil:
		compiled.kind = conditionHeader
		compiled.header = condition.Header.Name
		compiled.match, warnings = compileMatch(&condition.Header.RoutingMatch, path+".header", false, labels)
	case condition.Client != nil:
		compiled.kind = conditionClient
		compiled.match, warnings = compileMatch(condition.Client, path+".client", false, labels)
	default:
		return nil, nil, fmt.Errorf("%s: condition node must contain exactly one key", path)
	}
	return compiled, warnings, nil
}

func compileMatch(match *config.RoutingMatch, path string, apiKey bool, labels []apiKeyLabel) (compiledMatch, []string) {
	if match.In != nil {
		if apiKey {
			values, warnings := resolveAPIKeyLabels(match.In, path+".in", labels)
			return compiledMatch{operation: matchSet, values: values, never: len(values) == 0}, warnings
		}
		return compiledMatch{operation: matchSet, values: append([]string(nil), match.In...)}, nil
	}
	if match.Is != "" {
		if apiKey {
			values, warnings := resolveAPIKeyLabels([]string{match.Is}, path+".is", labels)
			return compiledMatch{operation: matchSet, values: values, never: len(values) == 0}, warnings
		}
		return compiledMatch{operation: matchEqual, values: []string{match.Is}}, nil
	}
	if match.Match != "" {
		pattern := compileGlob(match.Match)
		if !apiKey {
			return compiledMatch{operation: matchGlob, pattern: pattern}, nil
		}
		values := make([]string, 0)
		for _, label := range labels {
			if pattern.MatchString(label.label) && !containsString(values, label.id) {
				values = append(values, label.id)
			}
		}
		if len(values) == 0 {
			return compiledMatch{operation: matchSet, never: true}, []string{fmt.Sprintf("%s.match: pattern %q matched no configured API-key labels", path, match.Match)}
		}
		return compiledMatch{operation: matchSet, values: values}, nil
	}
	if match.Regex != "" {
		pattern, _ := regexp.Compile(match.Regex) // config validation compiled it already
		if !apiKey {
			return compiledMatch{operation: matchRegex, pattern: pattern}, nil
		}
		values := make([]string, 0)
		for _, label := range labels {
			if pattern.MatchString(label.label) && !containsString(values, label.id) {
				values = append(values, label.id)
			}
		}
		if len(values) == 0 {
			return compiledMatch{operation: matchSet, never: true}, []string{fmt.Sprintf("%s.regex: pattern %q matched no configured API-key labels", path, match.Regex)}
		}
		return compiledMatch{operation: matchSet, values: values}, nil
	}
	return compiledMatch{never: true}, nil
}

func resolveAPIKeyLabels(labelsIn []string, path string, configured []apiKeyLabel) ([]string, []string) {
	values := make([]string, 0, len(labelsIn))
	warnings := make([]string, 0)
	for _, label := range labelsIn {
		found := false
		for _, entry := range configured {
			if !strings.EqualFold(label, entry.label) {
				continue
			}
			found = true
			if !containsString(values, entry.id) {
				values = append(values, entry.id)
			}
		}
		if !found {
			warnings = append(warnings, fmt.Sprintf("%s: unknown API-key label %q", path, label))
		}
	}
	return values, warnings
}

func compileGlob(glob string) *regexp.Regexp {
	var pattern strings.Builder
	pattern.Grow(len(glob)*2 + 12)
	pattern.WriteString("(?i)^")
	for _, char := range glob {
		switch char {
		case '*':
			pattern.WriteString("(?s:.*)")
		case '?':
			pattern.WriteString("(?s:.)")
		default:
			pattern.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	pattern.WriteByte('$')
	return regexp.MustCompile(pattern.String())
}

func matchesCondition(condition *compiledCondition, attrs RequestAttrs) bool {
	switch condition.kind {
	case conditionAll:
		for index := range condition.children {
			if !matchesCondition(&condition.children[index], attrs) {
				return false
			}
		}
		return true
	case conditionAny:
		for index := range condition.children {
			if matchesCondition(&condition.children[index], attrs) {
				return true
			}
		}
		return false
	case conditionNot:
		return !matchesCondition(condition.child, attrs)
	case conditionModel:
		return condition.match.matches(attrs.Model)
	case conditionAPIKey:
		return condition.match.matches(attrs.KeyID)
	case conditionClient:
		return condition.match.matches(attrs.ClientType)
	case conditionHeader:
		for name, values := range attrs.Headers {
			if !strings.EqualFold(name, condition.header) {
				continue
			}
			for _, value := range values {
				if condition.match.matches(value) {
					return true
				}
			}
		}
	}
	return false
}

func (match compiledMatch) matches(value string) bool {
	if match.never {
		return false
	}
	switch match.operation {
	case matchEqual:
		return len(match.values) > 0 && strings.EqualFold(value, match.values[0])
	case matchSet:
		for _, candidate := range match.values {
			if strings.EqualFold(value, candidate) {
				return true
			}
		}
		return false
	case matchGlob, matchRegex:
		return match.pattern != nil && match.pattern.MatchString(value)
	default:
		return false
	}
}

func hasEligibleTarget(targets, eligible []string) bool {
	for _, target := range targets {
		if containsString(eligible, target) {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func renderRuleCondition(condition *config.RoutingCondition) string {
	if condition == nil {
		return "always"
	}
	rendered := renderCondition(condition)
	if isConditionGroup(condition) {
		// The top-level group needs no parentheses; nested groups keep them.
		rendered = strings.TrimSuffix(strings.TrimPrefix(rendered, "("), ")")
	}
	return truncateCondition(rendered)
}

func isConditionGroup(condition *config.RoutingCondition) bool {
	return condition.All != nil || condition.Any != nil
}

func renderCondition(condition *config.RoutingCondition) string {
	switch {
	case condition.All != nil:
		return renderConditionGroup(condition.All, " and ")
	case condition.Any != nil:
		return renderConditionGroup(condition.Any, " or ")
	case condition.Not != nil:
		if isConditionGroup(condition.Not) {
			return "not " + renderCondition(condition.Not)
		}
		return "not (" + renderCondition(condition.Not) + ")"
	case condition.Model != nil:
		return renderMatchCondition("model", condition.Model)
	case condition.APIKey != nil:
		return renderMatchCondition("api-key", condition.APIKey)
	case condition.Header != nil:
		return renderMatchCondition("header "+renderListValue(condition.Header.Name), &condition.Header.RoutingMatch)
	case condition.Client != nil:
		return renderMatchCondition("client", condition.Client)
	default:
		return ""
	}
}

func renderConditionGroup(conditions []config.RoutingCondition, join string) string {
	if len(conditions) == 0 {
		if join == " and " {
			return "(always)"
		}
		return "(never)"
	}
	parts := make([]string, len(conditions))
	for index := range conditions {
		parts[index] = renderCondition(&conditions[index])
	}
	return "(" + strings.Join(parts, join) + ")"
}

func renderMatchCondition(field string, match *config.RoutingMatch) string {
	switch {
	case match.In != nil:
		values := make([]string, len(match.In))
		for index, value := range match.In {
			values[index] = renderListValue(value)
		}
		return field + " in [" + strings.Join(values, ", ") + "]"
	case match.Is != "":
		return field + " is " + strconv.Quote(match.Is)
	case match.Match != "":
		return field + " match " + strconv.Quote(match.Match)
	case match.Regex != "":
		return field + " regex " + strconv.Quote(match.Regex)
	default:
		return ""
	}
}

func renderListValue(value string) string {
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_-./:*?", char)) {
			return strconv.Quote(value)
		}
	}
	return value
}

func truncateCondition(value string) string {
	runes := []rune(value)
	if len(runes) <= 240 {
		return value
	}
	return string(runes[:239]) + "…"
}
