package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatInstructionRoleWrapper(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, source := range []string{"openai", "openai-response"} {
			for _, enabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/enabled=%t", source, stream, enabled), func(t *testing.T) {
					bodies := make(chan []byte, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Error(errRead)
							return
						}
						bodies <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"id\":\"r\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"r\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}))
					defer server.Close()
					cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", InstructionRoleWrapper: enabled}}}
					auth := &cliproxyauth.Auth{Provider: "fixture", Attributes: map[string]string{"compat_name": "fixture", "base_url": server.URL, "api_key": "test-key"}}
					payload := []byte(`{"messages":[{"role":"system","content":"base rule"},{"role":"developer","content":[{"type":"text","text":"mode PLAN"}]},{"role":"user","content":"inspect"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"inspect","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"file data"},{"role":"developer","content":"mode EDIT"},{"role":"user","content":"continue"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object","properties":{}}}}]}`)
					if source == "openai-response" {
						payload = []byte(`{"instructions":"base rule","input":[{"role":"developer","content":[{"type":"input_text","text":"mode PLAN"}]},{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"file data"},{"role":"developer","content":"mode EDIT"},{"role":"user","content":"continue"}],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{}}}]}`)
					}
					req := cliproxyexecutor.Request{Model: "test-model", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source), Stream: stream}
					executor := NewOpenAICompatExecutor("fixture", cfg)
					if stream {
						result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExec := executor.Execute(context.Background(), auth, req, opts); errExec != nil {
						t.Fatal(errExec)
					}
					body := <-bodies
					messages := ojson.GetBytes(body, "messages").Array()
					offset := 0
					if enabled {
						offset = 1
						if len(messages) != 8 {
							t.Fatalf("wrapped message count=%d, want 8: %s", len(messages), body)
						}
						if !strings.Contains(messages[0].Get("content").String(), "SYSTEM > DEVELOPER > USER") {
							t.Fatalf("missing hierarchy policy: %s", body)
						}
						for _, position := range []int{0, 1, 5} {
							wrapped := messages[position+1]
							if wrapped.Get("role").String() != "system" {
								t.Fatalf("position %d role=%q, want system", position, wrapped.Get("role").String())
							}
							record := ojson.Parse(wrapped.Get("content").String())
							role := "developer"
							if position == 0 {
								role = "system"
							}
							if record.Get("original_role").String() != role || record.Get("original_position").Int() != int64(position) {
								t.Fatalf("original role/position lost: %s", wrapped.Raw)
							}
							if position == 0 && record.Get("content").String() != "base rule" {
								t.Fatalf("system instruction lost: %s", record.Raw)
							}
							if position == 1 && record.Get("content.0.text").String() != "mode PLAN" {
								t.Fatalf("text-array instruction lost: %s", record.Raw)
							}
							if position == 5 && record.Get("content").String() != "mode EDIT" {
								t.Fatalf("session update lost: %s", record.Raw)
							}
						}
					} else if len(messages) != 7 || messages[1].Get("role").String() != "developer" || messages[5].Get("role").String() != "developer" {
						t.Fatalf("default changed roles: %s", body)
					}
					if messages[2+offset].Get("role").String() != "user" || messages[3+offset].Get("tool_calls.0.id").String() != "call_1" || messages[4+offset].Get("tool_call_id").String() != "call_1" || messages[4+offset].Get("content").String() != "file data" || messages[6+offset].Get("content").String() != "continue" {
						t.Fatalf("conversation/tool order changed: %s", body)
					}
				})
			}
		}
	}
}

func TestOpenAICompatInstructionRoleWrapperPayloadRulesRemainFinal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			bodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					return
				}
				bodies <- body
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"r\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer server.Close()
			models := []config.PayloadModelRule{{Name: "test-model", Protocol: "openai", Match: []map[string]any{{"messages.1.role": "system", "messages.2.role": "system"}}}}
			cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture", InstructionRoleWrapper: true}}, Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"messages.1.role": "developer"}}}, Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "test-model", Protocol: "openai"}}, Params: []string{"messages.0.content"}}}}}
			auth := &cliproxyauth.Auth{Provider: "fixture", Attributes: map[string]string{"compat_name": "fixture", "base_url": server.URL, "api_key": "test-key"}}
			req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"messages":[{"role":"system","content":"base"},{"role":"developer","content":"session"},{"role":"user","content":"hi"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
			executor := NewOpenAICompatExecutor("fixture", cfg)
			if stream {
				result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, errExec := executor.Execute(context.Background(), auth, req, opts); errExec != nil {
				t.Fatal(errExec)
			}
			body := <-bodies
			if ojson.GetBytes(body, "messages.#").Int() != 4 || ojson.GetBytes(body, "messages.1.role").String() != "developer" || ojson.GetBytes(body, "messages.0.content").Exists() {
				t.Fatalf("final payload rules did not observe prepared wrappers or were overwritten: %s", body)
			}
		})
	}
}

func TestOpenAICompatInstructionRoleWrapperCountTokens(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "fixture"}}}
	auth := &cliproxyauth.Auth{Provider: "fixture", Attributes: map[string]string{"compat_name": "fixture"}}
	req := cliproxyexecutor.Request{Model: "gpt-4o", Payload: []byte(`{"messages":[{"role":"developer","content":"session"},{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	executor := NewOpenAICompatExecutor("fixture", cfg)
	plain, errPlain := executor.CountTokens(context.Background(), auth, req, opts)
	if errPlain != nil {
		t.Fatal(errPlain)
	}
	cfg.OpenAICompatibility[0].InstructionRoleWrapper = true
	wrapped, errWrapped := executor.CountTokens(context.Background(), auth, req, opts)
	if errWrapped != nil {
		t.Fatal(errWrapped)
	}
	if ojson.GetBytes(wrapped.Payload, "usage.total_tokens").Int() <= ojson.GetBytes(plain.Payload, "usage.total_tokens").Int() {
		t.Fatalf("token count omitted wrapper overhead: plain=%s wrapped=%s", plain.Payload, wrapped.Payload)
	}
	cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-4o", Protocol: "openai"}}, Params: map[string]any{"messages": []map[string]string{{"role": "user", "content": "final"}}}}}
	finalWrapped, errFinal := executor.CountTokens(context.Background(), auth, req, opts)
	if errFinal != nil {
		t.Fatal(errFinal)
	}
	cfg.OpenAICompatibility[0].InstructionRoleWrapper = false
	finalPlain, errFinalPlain := executor.CountTokens(context.Background(), auth, req, opts)
	if errFinalPlain != nil {
		t.Fatal(errFinalPlain)
	}
	if string(finalPlain.Payload) != string(finalWrapped.Payload) {
		t.Fatal("token counting wrapped after final payload rules")
	}
}
