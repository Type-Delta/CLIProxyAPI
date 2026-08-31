package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestGetAPIKeyEntriesDecodesLimits checks that the configured limit block is
// carried through for object entries and stays nil for bare strings.
func TestGetAPIKeyEntriesDecodesLimits(t *testing.T) {
	body := `{"api-keys":["a",null,{},{"key":"b","limits":{"max-requests":100,"max-tokens-m":1.5,"resets":"daily"}},{"key":"c"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, errWrite := w.Write([]byte(body)); errWrite != nil {
			t.Errorf("write response: %v", errWrite)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	entries, err := client.GetAPIKeyEntries()
	if err != nil {
		t.Fatalf("GetAPIKeyEntries() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Key != "a" || entries[0].Limits != nil {
		t.Fatalf("entry 0 = %#v", entries[0])
	}
	if entries[1].Key != "b" || entries[1].Limits == nil {
		t.Fatalf("entry 1 = %#v", entries[1])
	}
	if got := *entries[1].Limits; got.MaxRequests != 100 || got.MaxTokensM != 1.5 || got.Resets != "daily" {
		t.Fatalf("entry 1 limits = %#v", got)
	}
	if entries[2].Key != "c" || entries[2].Limits != nil {
		t.Fatalf("entry 2 = %#v", entries[2])
	}
}

func TestDeleteAPIKeyUsesRevisionWithoutRawKeyURL(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"api-keys":["raw-key"],"config_revision":"rev-current"}`))
		case http.MethodDelete:
			requested = true
			if got := r.URL.RawQuery; got != "index=0" {
				t.Errorf("DELETE query = %q, want index only", got)
			}
			if got := r.Header.Get("If-Match"); got != `"rev-current"` {
				t.Errorf("If-Match = %q", got)
			}
			if got := r.Header.Get("X-CPA-API-Key-Contract"); got != "1" {
				t.Errorf("contract header = %q", got)
			}
			w.Header().Set("X-CPA-Config-Revision", "rev-next")
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	if _, err := client.GetAPIKeyEntries(); err != nil {
		t.Fatalf("GetAPIKeyEntries() error = %v", err)
	}
	if err := client.DeleteAPIKey(0); err != nil {
		t.Fatalf("DeleteAPIKey() error = %v", err)
	}
	if !requested {
		t.Fatal("DELETE request was not sent")
	}
	if got := client.currentAPIKeyRevision(); got != "rev-next" {
		t.Fatalf("stored revision = %q, want rev-next", got)
	}
}

func TestDeleteAPIKeyWithoutRevisionDoesNotSendRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	errDelete := client.DeleteAPIKey(0)
	if errDelete == nil || !strings.Contains(errDelete.Error(), "revision unavailable") {
		t.Fatalf("DeleteAPIKey() error = %v", errDelete)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

// TestAPIKeyPatchBodies pins the exact PATCH payloads for create, edit with
// limits and edit clearing limits.
func TestAPIKeyPatchBodies(t *testing.T) {
	tests := []struct {
		name string
		call func(c *Client) error
		want string
	}{
		{
			name: "create without limits omits the field",
			call: func(c *Client) error { return c.AddAPIKey("new-key", nil) },
			want: `{"new":"new-key"}`,
		},
		{
			name: "create with limits sends the object",
			call: func(c *Client) error {
				return c.AddAPIKey("new-key", &APIKeyLimitConfig{MaxRequests: 100, MaxTokensM: 1.5, Resets: "daily"})
			},
			want: `{"limits":{"max-requests":100,"max-tokens-m":1.5,"resets":"daily"},"new":"new-key"}`,
		},
		{
			name: "edit with limits sends index and object",
			call: func(c *Client) error {
				return c.EditAPIKey(2, "edited", &APIKeyLimitConfig{MaxTokensM: 0.5, Resets: "weekly"})
			},
			want: `{"index":2,"limits":{"max-tokens-m":0.5,"resets":"weekly"},"new":"edited"}`,
		},
		{
			name: "edit clearing limits sends explicit null",
			call: func(c *Client) error { return c.EditAPIKey(0, "edited", nil) },
			want: `{"index":0,"limits":null,"new":"edited"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/v0/management/api-keys" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				raw, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read body: %v", errRead)
				}
				got = strings.TrimSpace(string(raw))
			}))
			defer server.Close()

			client := &Client{baseURL: server.URL, http: server.Client()}
			if err := tt.call(client); err != nil {
				t.Fatalf("request error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("body = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestAPIKeyPatchSurfacesServerError checks that the server message replaces the
// raw JSON payload in the returned error.
func TestAPIKeyPatchSurfacesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if _, errWrite := w.Write([]byte(`{"error":"invalid resets value: yearly"}`)); errWrite != nil {
			t.Errorf("write response: %v", errWrite)
		}
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	err := client.AddAPIKey("key", &APIKeyLimitConfig{Resets: "yearly"})
	if err == nil || err.Error() != "invalid resets value: yearly" {
		t.Fatalf("error = %v", err)
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
