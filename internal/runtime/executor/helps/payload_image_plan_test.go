package helps

import (
	"encoding/json"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type observingPayloadValue struct {
	observe func()
}

func (v observingPayloadValue) MarshalJSON() ([]byte, error) {
	v.observe()
	return json.Marshal("retained")
}

func TestPayloadImageRulesEvaluateBeforeBusinessWrites(t *testing.T) {
	original := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"first"},{"role":"user","content":"second"}],"tools":[{"type":"function","name":"lookup"}]}`)
	doc := ojson.NewDoc(original)
	_ = doc.SetRaw("tools.-1", `{"type":"image_generation","output_format":"png"}`)
	calls := 0
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{
			{Models: []config.PayloadModelRule{{Name: "*", Exist: []string{"tools.1.type"}}}, Params: map[string]any{"reasoning.effort": "medium"}},
			{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"reasoning.effort": "medium"}}, Exist: []string{"tools.1.type"}}}, Params: map[string]any{"metadata.plan_probe": observingPayloadValue{observe: func() {
				calls++
				if doc.Get("reasoning.effort").Exists() {
					t.Errorf("user rule already changed the business payload during evaluation; automatic tool still present=%v", doc.Get("tools.1.type").String() == "image_generation")
				}
			}}}},
		},
		Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"metadata.plan_probe": "retained"}}}}, Params: []string{"input.0"}}},
	}}
	before := PayloadConfigEvaluationCount()
	touched := ApplyPayloadConfigWithTrackedPathsForExecutorDoc(cfg, "codex", "gpt-6-astra", "codex", "responses", "", doc, original, "gpt-6-astra", "/v1/responses", nil, "reasoning", "metadata", "input")
	if count := PayloadConfigEvaluationCount() - before; count != 1 {
		t.Fatalf("rule evaluations=%d, want 1", count)
	}
	if calls != 1 {
		t.Fatalf("value marshaled %d times, want 1", calls)
	}
	if got := doc.Get("metadata.plan_probe").String(); got != "retained" {
		t.Fatalf("dependent rule lost: %q", got)
	}
	if doc.Get("tools.#").Int() != 1 || doc.Get("tools.0.type").String() != "function" {
		t.Fatalf("automatic image was not removed: %s", doc.Get("tools").Raw)
	}
	if doc.Get("input.#").Int() != 1 || doc.Get("input.0.content").String() != "second" {
		t.Fatalf("filter replayed or lost: %s", doc.Get("input").Raw)
	}
	for _, path := range []string{"reasoning", "metadata", "input"} {
		if !touched[path] {
			t.Errorf("missing tracked path %q", path)
		}
	}
}

func TestPayloadImagePlanCleansBeforeCommit(t *testing.T) {
	doc := ojson.NewDocString(`{"model":"gpt-6-astra","tools":[{"type":"image_generation"}]}`)
	imageIndex := 0
	plan := newPayloadRulePlan(doc, &imageIndex)
	if err := plan.Set("reasoning.effort", "medium"); err != nil {
		t.Fatal(err)
	}
	if !codexConfiguredPayloadIncompatibleWithAutoImageGenDoc(plan, "", "gpt-6-astra", nil, nil) {
		t.Fatal("policy did not read the evaluated effects")
	}
	if doc.Get("reasoning").Exists() {
		t.Fatal("evaluation changed the business document")
	}
	if !retractAutoInjectedImageGenerationToolAtIndexDoc(doc, "tools", 0, false) {
		t.Fatal("missing cleanup")
	}
	if doc.Get("tools").Exists() || doc.Get("reasoning").Exists() {
		t.Fatal("cleanup did not precede the rule writes")
	}
	if !plan.apply(doc, "tools", true) {
		t.Fatal("missing final operation commit")
	}
	if doc.Get("reasoning.effort").String() != "medium" || doc.Get("tools").Exists() {
		t.Fatalf("unexpected committed document: %s", doc.String())
	}
}

func TestPayloadImagePlanRetainsSequentialEffectsAndBindings(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "*"}}
	for _, tc := range []struct {
		name    string
		payload config.PayloadConfig
		tools   int64
		marker  string
		inputs  int64
	}{
		{
			name: "defaults and dependent raw append",
			payload: config.PayloadConfig{
				Default:     []config.PayloadRule{{Models: models, Params: map[string]any{"metadata.phase": "default"}}},
				DefaultRaw:  []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"metadata.phase": "default"}}}}, Params: map[string]any{"metadata.raw": `{"ready":true}`}}},
				OverrideRaw: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"metadata.raw.ready": true}}}}, Params: map[string]any{"input.-1": `{"type":"configuration_update","reasoning":{"effort":"medium"}}`}}},
			}, tools: 2, inputs: 3,
		},
		{
			name: "cross root query reads preceding effects",
			payload: config.PayloadConfig{Override: []config.PayloadRule{
				{Models: models, Params: map[string]any{"reasoning.effort": "medium"}},
				{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"@this.reasoning.effort": "medium"}}, Exist: []string{"@this.tools"}}}, Params: map[string]any{"marker": "matched"}},
			}}, tools: 2, inputs: 2, marker: "matched",
		},
		{
			name: "escaped field and dependent match",
			payload: config.PayloadConfig{Override: []config.PayloadRule{
				{Models: models, Params: map[string]any{`meta\.phase`: "ready"}},
				{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{`meta\.phase`: "ready"}}}}, Params: map[string]any{"marker": "matched", "reasoning.effort": "medium"}},
			}}, tools: 2, inputs: 2, marker: "matched",
		},
		{
			name:    "indexes after removed automatic tool",
			payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"reasoning.effort": "medium", "tools.3": map[string]any{"type": "function", "name": "new"}}}}}, tools: 3, inputs: 2,
		},
		{
			name: "query filters bind before cleanup and do not repeat",
			payload: config.PayloadConfig{
				Override: []config.PayloadRule{{Models: models, Params: map[string]any{"reasoning.effort": "medium", "tools.3": map[string]any{"type": "function", "name": "new"}}}},
				Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{`tools.#(name=="first")#`, `input.0`}}},
			}, tools: 2, inputs: 1,
		},
		{
			name:    "writes to the automatic tool claim it",
			payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"reasoning.effort": "medium", "tools.2.output_format": "webp"}}}}, tools: 3, inputs: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(`{"model":"gpt-6-astra","tools":[{"type":"function","name":"first"},{"type":"function","name":"second"}],"input":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`)
			doc := ojson.NewDoc(original)
			_ = doc.SetRaw("tools.-1", `{"type":"image_generation","output_format":"png"}`)
			before := PayloadConfigEvaluationCount()
			ApplyPayloadConfigWithTrackedPathsForExecutorDoc(&config.Config{Payload: tc.payload}, "codex", "gpt-6-astra", "codex", "responses", "", doc, original, "gpt-6-astra", "/v1/responses", nil)
			if PayloadConfigEvaluationCount()-before != 1 {
				t.Fatal("rules evaluated more than once")
			}
			if doc.Get("tools.#").Int() != tc.tools || doc.Get("input.#").Int() != tc.inputs {
				t.Fatalf("changed array bindings: %s", doc.String())
			}
			if tc.marker != "" && doc.Get("marker").String() != tc.marker {
				t.Fatalf("lost dependent match: %s", doc.String())
			}
			for _, tool := range doc.Get("tools").Array() {
				if !tool.IsObject() {
					t.Fatalf("cleanup left an index hole: %s", doc.Get("tools").Raw)
				}
			}
			if tc.name == "writes to the automatic tool claim it" {
				if doc.Get("tools.2.output_format").String() != "webp" {
					t.Fatal("explicit tool override lost")
				}
			} else {
				for _, tool := range doc.Get("tools").Array() {
					if tool.Get("type").String() == "image_generation" {
						t.Fatal("incompatible automatic image remained")
					}
				}
			}
			if tc.name == "query filters bind before cleanup and do not repeat" && doc.Get("tools.1.name").String() != "new" {
				t.Fatal("tool filter was retargeted")
			}
		})
	}
}

func TestPayloadImageFilterIsFinalEvenWhenItEmptiesTools(t *testing.T) {
	original := []byte(`{"model":"gpt-6-astra","tools":[{"type":"function","name":"first"}]}`)
	doc := ojson.NewDoc(original)
	_ = doc.SetRaw("tools.-1", `{"type":"image_generation"}`)
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"reasoning.effort": "medium"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"tools.0"}}},
	}}
	ApplyPayloadConfigWithTrackedPathsForExecutorDoc(cfg, "codex", "gpt-6-astra", "codex", "responses", "", doc, original, "gpt-6-astra", "/v1/responses", nil)
	if tools := doc.Get("tools"); !tools.IsArray() || len(tools.Array()) != 0 {
		t.Fatalf("post-rule cleanup rewrote the filter's empty array: %s", tools.Raw)
	}
}

func TestPayloadImagePlanNestedRoot(t *testing.T) {
	for _, replaceParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "projected reasoning", true: "explicit parent replaces tools"}[replaceParent], func(t *testing.T) {
			original := []byte(`{"request":{"model":"gpt-6-astra","tools":[{"type":"function","name":"caller"}]}}`)
			doc := ojson.NewDoc(original)
			_ = doc.SetRaw("request.tools.-1", `{"type":"image_generation"}`)
			params := map[string]any{"reasoning.effort": "medium"}
			if replaceParent {
				params = map[string]any{"": map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "medium"}, "tools": []any{map[string]any{"type": "image_generation", "output_format": "webp"}}}}
			}
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: params}}}}
			ApplyPayloadConfigWithTrackedPathsForExecutorDoc(cfg, "codex", "gpt-6-astra", "codex", "responses", "request", doc, original, "gpt-6-astra", "/v1/responses", nil)
			if doc.Get("request.reasoning.effort").String() != "medium" {
				t.Fatal("lost configured reasoning")
			}
			wantType := "function"
			if replaceParent {
				wantType = "image_generation"
			}
			if doc.Get("request.tools.#").Int() != 1 || doc.Get("request.tools.0.type").String() != wantType {
				t.Fatalf("nested cleanup overwrote rules: %s", doc.String())
			}
			if replaceParent && doc.Get("request.tools.0.output_format").String() != "webp" {
				t.Fatal("parent replacement lost explicit image")
			}
		})
	}
}
