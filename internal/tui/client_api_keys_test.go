package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
