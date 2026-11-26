package helps

import (
	"context"
	"errors"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/antigravity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityWireModel(t *testing.T) {
	const base = "gemini-3.8-flash"
	const high = base + "-high"
	const low = base + "-low"
	const path = "request.generationConfig.thinkingConfig.thinkingLevel"
	info := &registry.ModelInfo{
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
		AntigravityThinking: &registry.AntigravityThinking{
			BaseID: base, DefaultLevel: "high", WireModels: map[string]string{"low": low, "high": high},
		},
	}
	for _, tc := range []struct {
		name, model, effort, wantID, wantEffort string
		baseOnWire, legacyCatalog               bool
	}{
		{"split to combined", base, "low", low, "LOW", false, false},
		{"combined preserved", high, "", high, "", false, false},
		{"split to split", base, "low", base, "LOW", true, false},
		{"combined to split", high, "", base, "HIGH", true, false},
		{"legacy catalog", high, "low", high, "low", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(`{"request":{"generationConfig":{}}}`)
			if tc.effort != "" {
				_ = doc.Set(path, tc.effort)
			}
			selected := info
			if tc.legacyCatalog {
				selected = nil
			} else if tc.baseOnWire {
				group := *info.AntigravityThinking
				group.WireModels = map[string]string{"low": base, "high": base}
				groupedInfo := *info
				groupedInfo.AntigravityThinking = &group
				selected = &groupedInfo
			}
			ctx := context.Background()
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity"}
			if selected != nil {
				selected.ID = tc.model
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, "antigravity", []*registry.ModelInfo{selected})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}
			req := cliproxyexecutor.Request{Model: tc.model, Payload: doc.Bytes()}
			model := SelectAntigravityRequestModel(ctx, nil, auth, tc.model, tc.model, "antigravity", req.Payload, opts, false)
			var err error
			if selected != nil {
				err = ApplyAntigravityRequestThinkingDoc(doc, req, opts, model)
			}
			got := model.ID
			if err != nil || got != tc.wantID || doc.Get(path).Str != tc.wantEffort {
				t.Fatalf("wire model=%q effort=%q error=%v", got, doc.Get(path).Str, err)
			}
		})
	}
}

func TestAntigravityThinkingUsesSelectedAccount(t *testing.T) {
	const model = "gemini-3.8-flash"
	reg := registry.GetGlobalRegistry()
	for _, item := range []struct{ id, level string }{{"agy-qa-low", "low"}, {"agy-qa-high", "high"}} {
		reg.RegisterClient(item.id, "antigravity", []*registry.ModelInfo{{
			ID: model, Type: "antigravity", Thinking: &registry.ThinkingSupport{Levels: []string{item.level}},
			AntigravityThinking: &registry.AntigravityThinking{
				BaseID: model, DefaultLevel: item.level, KnownLevels: []string{"low", "high"},
				WireModels: map[string]string{item.level: model + "-" + item.level},
			},
		}})
		t.Cleanup(func() { reg.UnregisterClient(item.id) })
	}
	req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"request":{"generationConfig":{"thinking_config":{"thinking_level":"LOW","include_thoughts":true}}}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}
	doc := ojson.NewDoc(req.Payload)
	low := SelectAntigravityRequestModel(t.Context(), nil, &cliproxyauth.Auth{ID: "agy-qa-low"}, model, model, "antigravity", req.Payload, opts, false)
	if err := ApplyAntigravityRequestThinkingDoc(doc, req, opts, low); err != nil {
		t.Fatal(err)
	}
	wire, err := low.ID, low.Err
	if err != nil || wire != model+"-low" || thinking.ExtractTranslatedReasoningEffort(doc.Bytes(), "antigravity") != "low" {
		t.Fatalf("wire=%q body=%s error=%v", wire, doc.String(), err)
	}
	if doc.Get("request.generationConfig.thinking_config").Exists() ||
		!doc.Get("request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("thinking aliases were not normalized: %s", doc.String())
	}
	high := SelectAntigravityRequestModel(t.Context(), nil, &cliproxyauth.Auth{ID: "agy-qa-high"}, model, model, "antigravity", req.Payload, opts, false)
	var unavailable *cliproxyauth.Error
	if err := ApplyAntigravityRequestThinkingDoc(ojson.NewDoc(req.Payload), req, opts, high); !errors.As(err, &unavailable) ||
		unavailable.Code != cliproxyauth.ErrorCodeAntigravityTargetUnavailable {
		t.Fatalf("high-only account returned %v", err)
	}
}
