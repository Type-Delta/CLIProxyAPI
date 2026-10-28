package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRoutingRuleValidationPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "multiple condition keys",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        model: {is: gpt-5}\n        client: {is: codex}\n",
			want: "routing.rules[0].when:",
		},
		{
			name: "multiple leaf operators",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        model: {is: gpt-5, match: gpt-*}\n",
			want: "routing.rules[0].when.model:",
		},
		{
			name: "empty in",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        client: {in: []}\n",
			want: "routing.rules[0].when.client.in:",
		},
		{
			name: "missing header name",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        header: {is: infra}\n",
			want: "routing.rules[0].when.header.name:",
		},
		{
			name: "invalid regex",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        model: {regex: '['}\n",
			want: "routing.rules[0].when.model.regex:",
		},
		{
			name: "unknown field",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a]\n      when:\n        model: {is: gpt-5, typo: x}\n",
			want: "routing.rules[0].when.model.typo:",
		},
		{
			name: "empty target",
			raw:  "routing:\n  rules:\n    - route-to: [auth-a, ' ']\n",
			want: "routing.rules[0].route-to[1]:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseConfigBytes() error = %v, want path containing %q", err, tc.want)
			}
		})
	}
}

func TestRoutingRulesLoadPathValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("routing:\n  rules:\n    - route-to: []\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, optional := range []bool{false, true} {
		if _, err := LoadConfigOptional(path, optional); err == nil || !strings.Contains(err.Error(), "routing.rules[0].route-to:") {
			t.Fatalf("LoadConfigOptional(optional=%t) error = %v, want route-to validation path", optional, err)
		}
	}
}

func TestRoutingRuleLimits(t *testing.T) {
	longRegex := strings.Repeat("界", 513)
	deep := RoutingCondition{Model: &RoutingMatch{Is: "model"}}
	for range 8 {
		deep = RoutingCondition{Not: &deep}
	}
	tooManyNodes := RoutingCondition{All: make([]RoutingCondition, 64)}
	for index := range tooManyNodes.All {
		tooManyNodes.All[index] = RoutingCondition{Client: &RoutingMatch{Is: "codex"}}
	}
	tooManyTargets := make([]string, 65)
	for index := range tooManyTargets {
		tooManyTargets[index] = "auth"
	}
	tooManyRules := make([]RoutingRule, 257)
	for index := range tooManyRules {
		tooManyRules[index] = RoutingRule{RouteTo: []string{"auth"}}
	}
	for _, tc := range []struct {
		name  string
		rules []RoutingRule
		want  string
	}{
		{
			name:  "regex length",
			rules: []RoutingRule{{When: &RoutingCondition{Model: &RoutingMatch{Regex: longRegex}}, RouteTo: []string{"auth"}}},
			want:  "routing.rules[0].when.model.regex:",
		},
		{
			name:  "depth",
			rules: []RoutingRule{{When: &deep, RouteTo: []string{"auth"}}},
			want:  "condition depth must not exceed 8",
		},
		{
			name:  "node count",
			rules: []RoutingRule{{When: &tooManyNodes, RouteTo: []string{"auth"}}},
			want:  "condition nodes",
		},
		{
			name:  "target count",
			rules: []RoutingRule{{RouteTo: tooManyTargets}},
			want:  "routing.rules[0].route-to:",
		},
		{
			name:  "rule count",
			rules: tooManyRules,
			want:  "routing.rules:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateRoutingRules(tc.rules); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRoutingRules() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRoutingRuleJSONUsesKebabCaseAndFlattensHeaderMatch(t *testing.T) {
	rule := RoutingRule{
		Name: "example",
		When: &RoutingCondition{Header: &RoutingHeaderMatch{
			Name:         "x-team",
			RoutingMatch: RoutingMatch{Is: "infra"},
		}},
		RouteTo: []string{"auth-a"},
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)
	for _, field := range []string{`"route-to"`, `"header"`, `"name":"x-team"`, `"is":"infra"`} {
		if !strings.Contains(got, field) {
			t.Fatalf("JSON %s missing %s", got, field)
		}
	}
	if strings.Contains(got, "route_to") || strings.Contains(got, "api_key") {
		t.Fatalf("JSON uses non-contract field spelling: %s", got)
	}
	var decoded RoutingRule
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.When == nil || decoded.When.Header == nil || decoded.When.Header.Is != "infra" {
		t.Fatalf("decoded header match = %#v", decoded.When)
	}
}

func TestRoutingConditionMarshalingPreservesEmptyGroups(t *testing.T) {
	condition := RoutingCondition{All: []RoutingCondition{}}
	encodedJSON, err := json.Marshal(condition)
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedJSON) != `{"all":[]}` {
		t.Fatalf("JSON empty group = %s", encodedJSON)
	}
	encodedYAML, err := yaml.Marshal(condition)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedYAML), "all: []") {
		t.Fatalf("YAML lost empty group: %s", encodedYAML)
	}
	if err = ValidateRoutingRules([]RoutingRule{{When: &condition, RouteTo: []string{"auth-a"}}}); err != nil {
		t.Fatalf("empty all group should be a valid single node: %v", err)
	}
}

func TestRoutingRulesCloneAndPreserveCommentsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{name: "legacy", prefix: ""},
		{name: "v8", prefix: "config-version: 8\nserver:\n  port: 8317\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := tc.prefix + "routing:\n  rules:\n    - name: first\n      when:\n        all:\n          - model: {match: 'claude-opus-*'}\n          - not:\n              header: {name: x-team, is: infra}\n      route-to: [auth-a]\n    - name: second\n      when:\n        client: {in: [codex, pi]}\n      route-to: [auth-b]\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}

			clone := cfg.CloneForRuntime()
			cfg.Routing.Rules[0].When.All[0].Model.Match = "mutated"
			if clone.Routing.Rules[0].When.All[0].Model.Match != "claude-opus-*" {
				t.Fatal("CloneForRuntime shared a nested routing rule pointer")
			}
			cfg = clone

			cfg.Routing.Rules = append(cfg.Routing.Rules, RoutingRule{
				Name: "added",
				When: &RoutingCondition{Any: []RoutingCondition{
					{Model: &RoutingMatch{Is: "gpt-5"}},
					{APIKey: &RoutingMatch{In: []string{"research"}}},
				}},
				RouteTo: []string{"auth-c"},
			})
			cfg = saveAndReloadRules(t, path, cfg)
			if len(cfg.Routing.Rules) != 3 || cfg.Routing.Rules[2].When.Any[1].APIKey.In[0] != "research" {
				t.Fatal("added routing rule or nested condition did not survive save")
			}

			cfg.Routing.Rules = []RoutingRule{cfg.Routing.Rules[2], cfg.Routing.Rules[0], cfg.Routing.Rules[1]}
			cfg = saveAndReloadRules(t, path, cfg)
			if got := routingRuleNames(cfg.Routing.Rules); !reflect.DeepEqual(got, []string{"added", "first", "second"}) {
				t.Fatalf("rule order after save = %v", got)
			}

			cfg.Routing.Rules[1].When.All = []RoutingCondition{
				cfg.Routing.Rules[1].When.All[1],
				cfg.Routing.Rules[1].When.All[0],
			}
			cfg = saveAndReloadRules(t, path, cfg)
			if cfg.Routing.Rules[1].When.All[0].Not == nil || cfg.Routing.Rules[1].When.All[1].Model == nil {
				t.Fatalf("nested condition order after save = %#v", cfg.Routing.Rules[1].When.All)
			}

			cfg.Routing.Rules[1].When.All = cfg.Routing.Rules[1].When.All[1:]
			cfg.Routing.Rules = cfg.Routing.Rules[:2]
			cfg = saveAndReloadRules(t, path, cfg)
			if got := routingRuleNames(cfg.Routing.Rules); !reflect.DeepEqual(got, []string{"added", "first"}) {
				t.Fatalf("rule order after deletion = %v", got)
			}
			if len(cfg.Routing.Rules[1].When.All) != 1 || cfg.Routing.Rules[1].When.All[0].Model == nil {
				t.Fatalf("nested condition deletion did not survive save: %#v", cfg.Routing.Rules[1].When)
			}

			cfg.Routing.Rules = cfg.Routing.Rules[:1]
			cfg.Routing.Rules[0].When.Any = cfg.Routing.Rules[0].When.Any[:1]
			cfg = saveAndReloadRules(t, path, cfg)
			if len(cfg.Routing.Rules) != 1 || len(cfg.Routing.Rules[0].When.Any) != 1 {
				t.Fatalf("rule/nested condition deletion did not survive save: %#v", cfg.Routing.Rules)
			}
		})
	}
}

func saveAndReloadRules(t *testing.T, path string, cfg *Config) *Config {
	t.Helper()
	if err := cfg.ValidateRoutingRules(); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

func routingRuleNames(rules []RoutingRule) []string {
	names := make([]string, len(rules))
	for i, rule := range rules {
		names[i] = rule.Name
	}
	return names
}
