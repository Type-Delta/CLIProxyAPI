package routing

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDecideConditionOperators(t *testing.T) {
	clientKeys := []config.APIKeyEntry{{Key: "secret-a", Label: "Research"}}
	cfg := &config.Config{}
	cfg.APIKeys = clientKeys
	cfg.Routing.Rules = []config.RoutingRule{
		{
			Name: "all operators",
			When: &config.RoutingCondition{All: []config.RoutingCondition{
				{Model: &config.RoutingMatch{Is: "CLAUDE-OPUS-4"}},
				{Model: &config.RoutingMatch{Match: "claude-opus-?"}},
				{Model: &config.RoutingMatch{Regex: "opus-[0-9]"}},
				{APIKey: &config.RoutingMatch{In: []string{"research"}}},
				{Client: &config.RoutingMatch{Is: "CODEX"}},
				{Header: &config.RoutingHeaderMatch{Name: "x-team", RoutingMatch: config.RoutingMatch{In: []string{"other", "INFRA"}}}},
			}},
			RouteTo: []string{"auth-a"},
		},
	}
	set, warnings, err := Compile(cfg)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compile() error=%v warnings=%v", err, warnings)
	}
	attrs := RequestAttrs{
		Model:      "claude-opus-4",
		KeyID:      config.APIKeyID("secret-a"),
		ClientType: "codex",
		Headers: http.Header{
			"X-Team": []string{"qa", "infra"},
		},
	}
	decision := set.Decide(attrs, []string{"auth-a", "auth-b"})
	if decision.Applied != 0 || len(decision.Allowed) != 1 || decision.Allowed[0] != "auth-a" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestDecideEmptyBooleanGroups(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{
		{Name: "empty all", When: &config.RoutingCondition{All: []config.RoutingCondition{}}, RouteTo: []string{"auth-a"}},
		{Name: "empty any", When: &config.RoutingCondition{Any: []config.RoutingCondition{}}, RouteTo: []string{"auth-b"}},
		{Name: "unconditional", RouteTo: []string{"auth-c"}},
	}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decision := set.Decide(RequestAttrs{}, []string{"auth-a", "auth-b", "auth-c"})
	if decision.Applied != 2 || len(decision.Matched) != 2 || decision.Matched[0].Index != 0 || decision.Matched[1].Index != 2 || decision.Matched[1].Condition != "always" {
		t.Fatalf("empty group decision = %#v", decision)
	}
}

func TestDecideGlobIsFullStringCaseInsensitiveAndSlashAware(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{{
		When:    &config.RoutingCondition{Model: &config.RoutingMatch{Match: "TEAM/*/Model-?"}},
		RouteTo: []string{"auth-a"},
	}}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{model: "team/x/y/model-A", want: true},
		{model: "prefix/team/x/model-A", want: false},
		{model: "team/x/model-AB", want: false},
	} {
		decision := set.Decide(RequestAttrs{Model: tc.model}, []string{"auth-a"})
		if got := decision.Applied >= 0; got != tc.want {
			t.Fatalf("model %q matched=%v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestDecideHeaderMissingAndNegation(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{
		{
			Name: "missing header is negated",
			When: &config.RoutingCondition{Not: &config.RoutingCondition{Header: &config.RoutingHeaderMatch{
				Name: "x-mode", RoutingMatch: config.RoutingMatch{Is: "blocked"},
			}}},
			RouteTo: []string{"auth-a"},
		},
	}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{name: "missing", headers: nil, want: true},
		{name: "nonmatching value", headers: http.Header{"X-Mode": []string{"ok"}}, want: true},
		{name: "matching value", headers: http.Header{"x-mode": []string{"ok", "blocked"}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := set.Decide(RequestAttrs{Headers: tc.headers}, []string{"auth-a"})
			if got := decision.Applied >= 0; got != tc.want {
				t.Fatalf("applied=%v, want %v; decision=%#v", got, tc.want, decision)
			}
		})
	}
}

func TestDecideReverseRulePrecedenceAndFallThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{
		{Name: "upper", RouteTo: []string{"auth-a"}},
		{Name: "middle", RouteTo: []string{"auth-b"}},
		{Name: "bottom", RouteTo: []string{"auth-blocked"}},
	}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decision := set.Decide(RequestAttrs{}, []string{"auth-a", "auth-b"})
	if decision.Applied != 1 || !equalStrings(decision.Allowed, []string{"auth-b"}) {
		t.Fatalf("decision = %#v", decision)
	}
	wantStatuses := []string{RuleOverridden, RuleApplied, RuleFellThrough}
	for index, want := range wantStatuses {
		if got := decision.Matched[index].Status; got != want {
			t.Fatalf("matched[%d].status = %q, want %q", index, got, want)
		}
	}

	decision = set.Decide(RequestAttrs{}, []string{"auth-a"})
	if decision.Applied != 0 || decision.Matched[0].Status != RuleApplied || decision.Matched[1].Status != RuleFellThrough || decision.Matched[2].Status != RuleFellThrough {
		t.Fatalf("fall-through decision = %#v", decision)
	}
	decision = set.Decide(RequestAttrs{}, []string{"elsewhere"})
	if decision.Applied != -1 || decision.Allowed != nil {
		t.Fatalf("empty intersections should preserve the full pool: %#v", decision)
	}
	for index := range decision.Matched {
		if decision.Matched[index].Status != RuleFellThrough {
			t.Fatalf("matched[%d].status = %q, want fell_through", index, decision.Matched[index].Status)
		}
	}
}

func TestCompileDisabledRulesKeepConfigIndexes(t *testing.T) {
	disabled := false
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{
		{Name: "disabled", Enabled: &disabled, RouteTo: []string{"auth-a"}},
		{Name: "enabled", RouteTo: []string{"auth-b"}},
	}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decision := set.Decide(RequestAttrs{}, []string{"auth-b"})
	if set.Len() != 1 || decision.Applied != 1 || decision.Matched[0].Index != 1 {
		t.Fatalf("len=%d decision=%#v", set.Len(), decision)
	}
}

func TestCompileAPIKeyLabelsAndWarnings(t *testing.T) {
	cfg := &config.Config{}
	cfg.APIKeys = []config.APIKeyEntry{
		{Key: "research-key", Label: "Research"},
		{Key: "personal-key", Label: "Personal"},
	}
	cfg.Routing.Rules = []config.RoutingRule{
		{Name: "in labels", When: &config.RoutingCondition{APIKey: &config.RoutingMatch{In: []string{"research", "missing"}}}, RouteTo: []string{"auth-a"}},
		{Name: "glob labels", When: &config.RoutingCondition{APIKey: &config.RoutingMatch{Match: "pers*"}}, RouteTo: []string{"auth-b"}},
		{Name: "regex labels", When: &config.RoutingCondition{APIKey: &config.RoutingMatch{Regex: "^Research$"}}, RouteTo: []string{"auth-c"}},
	}
	set, warnings, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `unknown API-key label "missing"`) {
		t.Fatalf("warnings = %v", warnings)
	}
	for _, tc := range []struct {
		key      string
		eligible []string
		applied  int
	}{
		{key: "research-key", eligible: []string{"auth-a"}, applied: 0},
		{key: "personal-key", eligible: []string{"auth-b"}, applied: 1},
		{key: "other", eligible: []string{"auth-a", "auth-b", "auth-c"}, applied: -1},
	} {
		decision := set.Decide(RequestAttrs{KeyID: config.APIKeyID(tc.key)}, tc.eligible)
		if decision.Applied != tc.applied {
			t.Fatalf("key %q applied rule %d, want %d (%#v)", tc.key, decision.Applied, tc.applied, decision)
		}
	}
	regexDecision := set.Decide(RequestAttrs{KeyID: config.APIKeyID("research-key")}, []string{"auth-c"})
	if regexDecision.Applied != 2 {
		t.Fatalf("regex label decision = %#v", regexDecision)
	}
}

func TestCompileUnknownAPIKeyLabelNeverMatches(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{
		{Name: "unknown label", When: &config.RoutingCondition{APIKey: &config.RoutingMatch{Is: "missing"}}, RouteTo: []string{"auth-a"}},
	}
	set, warnings, err := Compile(cfg)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("Compile() error=%v warnings=%v", err, warnings)
	}
	decision := set.Decide(RequestAttrs{KeyID: ""}, []string{"auth-a"})
	if decision.Applied != -1 || len(decision.Matched) != 0 {
		t.Fatalf("unknown API-key label matched: %#v", decision)
	}
}

func TestCompileReturnsStructuralErrorPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{{
		When: &config.RoutingCondition{All: []config.RoutingCondition{
			{Client: &config.RoutingMatch{Is: "codex"}},
			{Model: &config.RoutingMatch{Regex: "["}},
		}},
		RouteTo: []string{"auth-a"},
	}}
	if _, _, err := Compile(cfg); err == nil || !strings.Contains(err.Error(), "routing.rules[0].when.all[1].model.regex:") {
		t.Fatalf("Compile() error = %v, want nested regex path", err)
	}
}

func TestDecideAllowedIntersectionAndReturnedTargetsAreDetached(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{{Name: "all", RouteTo: []string{"auth-b", "auth-a"}}}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decision := set.Decide(RequestAttrs{}, []string{"auth-a", "auth-c", "auth-b"})
	if !equalStrings(decision.Allowed, []string{"auth-a", "auth-b"}) {
		t.Fatalf("allowed = %v", decision.Allowed)
	}
	decision.Matched[0].Targets[0] = "changed"
	next := set.Decide(RequestAttrs{}, []string{"auth-a", "auth-b"})
	if !equalStrings(next.Matched[0].Targets, []string{"auth-b", "auth-a"}) {
		t.Fatalf("caller modified compiled targets: %v", next.Matched[0].Targets)
	}
}

func TestRuleConditionRenderingIsBounded(t *testing.T) {
	long := strings.Repeat("x", 250)
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{{
		When:    &config.RoutingCondition{Model: &config.RoutingMatch{Match: long}},
		RouteTo: []string{"auth-a"},
	}}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decision := set.Decide(RequestAttrs{Model: long}, []string{"auth-a"})
	condition := []rune(decision.Matched[0].Condition)
	if len(condition) != 240 || condition[len(condition)-1] != '…' {
		t.Fatalf("rendered condition has %d runes and ends with %q", len(condition), condition[len(condition)-1])
	}
}

func TestRuleSetDecideConcurrent(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Rules = []config.RoutingRule{{
		When:    &config.RoutingCondition{Model: &config.RoutingMatch{Match: "claude-*"}},
		RouteTo: []string{"auth-a"},
	}}
	set, _, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				decision := set.Decide(RequestAttrs{Model: "claude-opus"}, []string{"auth-a"})
				if decision.Applied != 0 || len(decision.Allowed) != 1 {
					t.Errorf("concurrent decision = %#v", decision)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestRuleConditionRenderingParenthesizesOnlyNestedGroups(t *testing.T) {
	got := renderRuleCondition(&config.RoutingCondition{All: []config.RoutingCondition{
		{Model: &config.RoutingMatch{Match: "claude-opus-*"}},
		{Any: []config.RoutingCondition{
			{Client: &config.RoutingMatch{In: []string{"codex", "opencode"}}},
			{Not: &config.RoutingCondition{Any: []config.RoutingCondition{
				{Header: &config.RoutingHeaderMatch{Name: "x-team", RoutingMatch: config.RoutingMatch{Is: "infra"}}},
			}}},
		}},
		{Not: &config.RoutingCondition{Model: &config.RoutingMatch{Match: "*-mini"}}},
	}})
	want := `model match "claude-opus-*" and (client in [codex, opencode] or not (header x-team is "infra")) and not (model match "*-mini")`
	if got != want {
		t.Fatalf("rendered condition\n got: %s\nwant: %s", got, want)
	}
}

func TestRuleConditionRenderingTreatsEmptyGroupsAsGroups(t *testing.T) {
	cases := map[string]*config.RoutingCondition{
		"always":       {All: []config.RoutingCondition{}},
		"never":        {Any: []config.RoutingCondition{}},
		"not (always)": {Not: &config.RoutingCondition{All: []config.RoutingCondition{}}},
	}
	for want, condition := range cases {
		if got := renderRuleCondition(condition); got != want {
			t.Errorf("renderRuleCondition() = %q, want %q", got, want)
		}
	}
}
