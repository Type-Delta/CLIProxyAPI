package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestRoutingGSRUsesRequestCredentialTrack(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, affinity := range []bool{false, true} {
			t.Run(fmt.Sprintf("mixed=%t/affinity=%t", mixed, affinity), func(t *testing.T) {
				const model = "gsr-siwc-track"
				now := time.Now()
				a := &Auth{ID: "siwc-track", Provider: "codex", Status: StatusActive, Metadata: map[string]any{
					"type": "codex", "access_token": "outer-valid", "expired": now.Add(time.Hour).Format(time.RFC3339), "use_siwc": true,
					"siwc": map[string]any{"access_token": "inner-expired", "expired": now.Add(-time.Hour).Format(time.RFC3339)},
				}}
				m := gsrFallbackTestManager(t, model, a)
				if affinity {
					s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: m.Selector(), TTL: time.Hour})
					t.Cleanup(s.Stop)
					m.SetSelector(s)
				}
				headers := http.Header{"User-Agent": {"codex_cli_rs/1.0"}, "X-Session-ID": {"siwc-track-session"}}
				dry, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model, Headers: headers})
				if err != nil || len(dry.Candidates) != 1 || dry.Candidates[0].Gate != "" {
					t.Fatalf("dry run must allow the outer credential: trace=%+v error=%v", dry, err)
				}
				ctx, holder := routing.WithTraceHolder(context.Background())
				// Exercise GSR's deferred affinity refresh under a routing rule as well.
				ctx = context.WithValue(ctx, routingPoolKey{}, &routingPool{decide: func(_ routing.RequestAttrs, ids []string) routing.Decision {
					return routing.Decision{Allowed: ids}
				}})
				opts := ex.Options{Headers: headers}
				var selected *Auth
				if mixed {
					selected, _, _, err = m.pickNextMixed(ctx, []string{"codex"}, model, opts, nil)
				} else {
					selected, _, err = m.pickNext(ctx, "codex", model, opts, nil)
				}
				if err != nil || selected == nil || selected.ID != a.ID {
					t.Fatalf("selection must allow the outer credential: auth=%+v error=%v", selected, err)
				}
				trace := holder.Last()
				if trace == nil || trace.Pick < 0 || trace.Candidates[trace.Pick].Gate != "" {
					t.Fatalf("live trace rejected the selected outer credential: %+v", trace)
				}
			})
		}
	}
}

func TestImageDryRunMatchesLiveCredentialTrack(t *testing.T) {
	for _, source := range []string{"", "openai", "openai-image"} {
		for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
			for _, stream := range []bool{false, true} {
				for _, outerValid := range []bool{true, false} {
					for _, mode := range []string{"plain", "rules", "affinity"} {
						t.Run(fmt.Sprintf("source=%s/path=%s/stream=%t/outerValid=%t/%s", source, path, stream, outerValid, mode), func(t *testing.T) {
							const model = "gpt-image-2.6"
							outerExpiry, innerExpiry := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
							if !outerValid {
								outerExpiry, innerExpiry = innerExpiry, outerExpiry
							}
							a := &Auth{ID: "image-siwc", Provider: "codex", Status: StatusActive, Metadata: map[string]any{
								"type": "codex", "access_token": "outer", "expired": outerExpiry.Format(time.RFC3339), "use_siwc": true,
								"siwc": map[string]any{"access_token": "inner", "expired": innerExpiry.Format(time.RFC3339)},
							}}
							m := gsrFallbackTestManager(t, model, a)
							m.SetSelector(&RoundRobinSelector{})
							if mode != "plain" {
								cfg := &internalconfig.Config{}
								cfg.Routing.Rules = []internalconfig.RoutingRule{{Name: "image rule", When: &internalconfig.RoutingCondition{Model: &internalconfig.RoutingMatch{Is: model}}, RouteTo: []string{a.ID}}}
								m.SetConfig(cfg)
							}
							if mode == "affinity" {
								selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: m.Selector(), TTL: time.Hour})
								t.Cleanup(selector.Stop)
								m.SetSelector(selector)
								selector.cache.Set("codex::header:image-session::"+canonicalModelKey(model), a.ID)
							}
							headers := http.Header{"User-Agent": {"Python-urllib/3.14"}, "X-Session-ID": {"image-session"}}
							dry, err := m.DryRunRoute(t.Context(), routing.DryRunRequest{Model: model, Headers: headers, SourceFormat: source})
							if err != nil {
								t.Fatal(err)
							}
							wantAllowed := outerValid == (source == "openai-image")
							wantGate := routing.GateUnauthorized
							if wantAllowed {
								wantGate = ""
							}
							if len(dry.Candidates) != 1 || dry.Candidates[0].Gate != wantGate || (dry.Pick >= 0) != wantAllowed {
								t.Fatalf("dry candidates=%+v pick=%d; want gate=%q allowed=%t", dry.Candidates, dry.Pick, wantGate, wantAllowed)
							}
							if wantAllowed && mode == "affinity" && dry.Affinity.State != routing.AffinityKept {
								t.Fatalf("dry affinity=%+v", dry.Affinity)
							}
							ctx, holder := routing.WithTraceHolder(t.Context())
							req := ex.Request{Model: model, Payload: []byte(`{"model":"gpt-image-2.6","prompt":"cat"}`)}
							opts := ex.Options{SourceFormat: sdktranslator.FromString(source), Headers: headers, Metadata: map[string]any{"request_path": path}}
							var payload []byte
							if stream {
								result, errStream := m.ExecuteStream(ctx, []string{"codex"}, req, opts)
								err = errStream
								if result != nil {
									for chunk := range result.Chunks {
										if chunk.Err != nil {
											t.Fatal(chunk.Err)
										}
										payload = append(payload, chunk.Payload...)
									}
								}
							} else {
								response, errExecute := m.Execute(ctx, []string{"codex"}, req, opts)
								err = errExecute
								payload = response.Payload
							}
							if (err == nil) != wantAllowed {
								t.Fatalf("live error=%v; want allowed=%t", err, wantAllowed)
							}
							live := holder.Last()
							if live == nil || len(live.Candidates) != 1 || live.Candidates[0].Gate != wantGate || (live.Pick >= 0) != wantAllowed {
								t.Fatalf("live trace=%+v; dry=%+v", live, dry)
							}
							if wantAllowed && string(payload) != a.ID {
								t.Fatalf("live payload=%q", payload)
							}
							if wantAllowed && mode == "rules" && (dry.Rules == nil || dry.Rules.Applied != 0 || live.Rules == nil || live.Rules.Applied != 0) {
								t.Fatalf("rule not applied: dry=%+v live=%+v", dry.Rules, live.Rules)
							}
						})
					}
				}
			}
		}
	}
}
