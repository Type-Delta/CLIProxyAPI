package management

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseZaiQuota(t *testing.T) {
	body := []byte(`{"code":200,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":2000,"currentValue":318,"remaining":1681,"percentage":15,"nextResetTime":1789113883259},{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":10000,"currentValue":3235,"remaining":6764,"percentage":32,"nextResetTime":1789615750984}],"level":"lite"},"success":true}`)
	windows, err := parseZaiQuota(body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("window count = %d, want 2", len(windows))
	}
	if windows[0].Label != "5h" || *windows[0].Limit != 2000 || *windows[0].Used != 318 || *windows[0].Remaining != 1681 || *windows[0].Percent != 15 {
		t.Fatalf("five-hour window = %+v", windows[0])
	}
	if windows[1].Label != "weekly" || *windows[1].Limit != 10000 || *windows[1].Used != 3235 || *windows[1].Remaining != 6764 || *windows[1].Percent != 32 {
		t.Fatalf("weekly window = %+v", windows[1])
	}
	for index, want := range []int64{1789113883259, 1789615750984} {
		if got := windows[index].ResetsAt.UnixMilli(); got != want || windows[index].ResetsAt.Location() != time.UTC {
			t.Fatalf("window %d reset = %v, want UTC millisecond %d", index, windows[index].ResetsAt, want)
		}
	}
}

func TestClassifyZaiQuotaRequest(t *testing.T) {
	target, err := url.Parse(zaiUsageQuotaURL)
	if err != nil {
		t.Fatal(err)
	}
	auth := &coreauth.Auth{Provider: "openai-compatible-zai", Attributes: map[string]string{"usage_probe": "zai"}}
	if got := classifyQuotaRequest(auth, http.MethodGet, target, ""); got != quotaRequestCacheable {
		t.Fatalf("request kind = %d, want cacheable", got)
	}
	if got := classifyQuotaRequest(auth, http.MethodPost, target, ""); got != quotaRequestNotCacheable {
		t.Fatalf("POST request kind = %d, want not cacheable", got)
	}
	other, err := url.Parse(zaiUsageQuotaURL + "?extra=1")
	if err != nil {
		t.Fatal(err)
	}
	if got := classifyQuotaRequest(auth, http.MethodGet, other, ""); got != quotaRequestNotCacheable {
		t.Fatalf("query request kind = %d, want not cacheable", got)
	}
}

func TestCredentialDisplayNamePrecedence(t *testing.T) {
	tests := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{name: "attribute label", auth: &coreauth.Auth{Provider: "codex", FileName: "/auth/account.json", Label: "codex-apikey", Attributes: map[string]string{"label": "Team A", "api_key": "secret-key"}}, want: "Team A"},
		{name: "filename", auth: &coreauth.Auth{Provider: "codex", FileName: `C:\\auth\\account.json`, Label: "codex-apikey", Attributes: map[string]string{"api_key": "secret-key", "auth_kind": "apikey"}}, want: "account.json"},
		{name: "oauth label", auth: &coreauth.Auth{Provider: "codex", Label: "Account", Attributes: map[string]string{"auth_kind": "oauth"}}, want: "Account"},
		{name: "custom api label", auth: &coreauth.Auth{Provider: "codex", Label: "Production", Attributes: map[string]string{"api_key": "secret-key", "auth_kind": "apikey"}}, want: "Production"},
		{name: "masked key", auth: &coreauth.Auth{Provider: "codex", Label: "codex-apikey", Attributes: map[string]string{"api_key": "secret-key", "auth_kind": "apikey"}}, want: "codex secr...-key"},
		{name: "id fallback", auth: &coreauth.Auth{Provider: "codex", Label: "codex-apikey", Attributes: map[string]string{"auth_kind": "apikey"}, ID: "credential-id"}, want: "credential-id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := credentialDisplayName(test.auth); got != test.want {
				t.Fatalf("display name = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildAuthFileEntryIncludesCredentialMetadata(t *testing.T) {
	auth := &coreauth.Auth{ID: "credential-id", Provider: "zai", FileName: "account.json", Attributes: map[string]string{
		"path": "account.json", "label": "Team A", "usage_probe": "zai", "pricing_catalog": "zai-coding-plan",
	}}
	entry := (&Handler{}).buildAuthFileEntry(auth)
	for key, want := range map[string]string{"display_name": "Team A", "usage_probe": "zai", "pricing_catalog": "zai-coding-plan"} {
		if got, ok := entry[key].(string); !ok || got != want {
			t.Fatalf("entry[%q] = %#v, want %q", key, entry[key], want)
		}
	}
	if strings.TrimSpace(entry["display_name"].(string)) == "" {
		t.Fatal("display name is empty")
	}
}
