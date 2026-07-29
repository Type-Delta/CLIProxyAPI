package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGetAPIKeysMixedEntries covers the management payload once per-key usage
// limits exist: unlimited keys stay bare strings while limited keys become
// objects, and the TUI must read both forms.
func TestGetAPIKeysMixedEntries(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "all plain strings",
			body: `{"api-keys":["a","b"]}`,
			want: []string{"a", "b"},
		},
		{
			name: "mixed plain and limited",
			body: `{"api-keys":["a",{"key":"b","limits":{"max-requests":1000,"max-tokens-m":20,"resets":"weekly"}}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "all limited",
			body: `{"api-keys":[{"key":"a","limits":{"max-tokens-m":0.5}}]}`,
			want: []string{"a"},
		},
		{
			name: "empty list",
			body: `{"api-keys":[]}`,
			want: []string{},
		},
		{
			name: "null and keyless objects are skipped",
			body: `{"api-keys":["a",null,{},{"key":""},{"key":"  "},{"key":"b"}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "key whitespace is normalized",
			body: `{"api-keys":[" a ",{"key":" b "}]}`,
			want: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, errWrite := w.Write([]byte(tt.body)); errWrite != nil {
					t.Errorf("write response: %v", errWrite)
				}
			}))
			defer server.Close()

			client := &Client{baseURL: server.URL, http: server.Client()}
			got, err := client.GetAPIKeys()
			if err != nil {
				t.Fatalf("GetAPIKeys() error = %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("GetAPIKeys() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestAPIKeyLimitsAndReset(t *testing.T) {
	resetAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resetKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/api-key-limits":
			w.Header().Set("Content-Type", "application/json")
			if errWrite := json.NewEncoder(w).Encode(map[string]any{
				"api-key-limits": []map[string]any{{
					"key": "limited-key",
					"limits": map[string]any{
						"max_requests":  10,
						"requests_used": 3,
						"max_tokens":    100,
						"tokens_used":   25,
						"resets":        "daily",
						"reset_at":      resetAt,
					},
				}},
			}); errWrite != nil {
				t.Errorf("write limits response: %v", errWrite)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-key-limits/reset":
			var body struct {
				Key string `json:"key"`
			}
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode reset body: %v", errDecode)
			}
			resetKey = body.Key
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	limits, err := client.GetAPIKeyLimits()
	if err != nil {
		t.Fatalf("GetAPIKeyLimits() error = %v", err)
	}
	if len(limits) != 1 || limits[0].Key != "limited-key" || limits[0].Limits == nil {
		t.Fatalf("GetAPIKeyLimits() = %#v", limits)
	}
	got := limits[0].Limits
	if got.MaxRequests != 10 || got.RequestsUsed != 3 || got.MaxTokens != 100 || got.TokensUsed != 25 || got.Resets != "daily" || got.ResetAt == nil || !got.ResetAt.Equal(resetAt) {
		t.Fatalf("limits = %#v", got)
	}

	if errReset := client.ResetAPIKeyLimits("limited-key"); errReset != nil {
		t.Fatalf("ResetAPIKeyLimits() error = %v", errReset)
	}
	if resetKey != "limited-key" {
		t.Fatalf("reset key = %q, want limited-key", resetKey)
	}
}
