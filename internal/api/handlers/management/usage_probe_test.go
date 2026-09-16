package management

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func usageProbeTestPtr[T any](value T) *T {
	return &value
}

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

func TestParseOpenCodeGoQuota(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    []model.ProviderQuotaWindow
		wantErr bool
	}{
		{
			name: "valid windows",
			body: `{"usage":{"rolling":{"status":"ok","percent":64,"limit":100,"used":64,"remaining":36,"resetsAt":"2026-09-16T13:45:31.545Z"},"weekly":{"status":"ok","percent":65,"resetsAt":"2026-09-21T00:00:00.545Z"},"monthly":{"status":"ok","percent":32,"resetsAt":"2026-10-16T10:26:10.545+07:00"}}}`,
			want: []model.ProviderQuotaWindow{
				{
					Label:     "rolling",
					Limit:     usageProbeTestPtr(int64(100)),
					Used:      usageProbeTestPtr(int64(64)),
					Remaining: usageProbeTestPtr(int64(36)),
					Percent:   usageProbeTestPtr(float64(64)),
					ResetsAt:  usageProbeTestPtr(time.Date(2026, 9, 16, 13, 45, 31, 545000000, time.UTC)),
				},
				{
					Label:    "weekly",
					Percent:  usageProbeTestPtr(float64(65)),
					ResetsAt: usageProbeTestPtr(time.Date(2026, 9, 21, 0, 0, 0, 545000000, time.UTC)),
				},
				{
					Label:    "monthly",
					Percent:  usageProbeTestPtr(float64(32)),
					ResetsAt: usageProbeTestPtr(time.Date(2026, 10, 16, 3, 26, 10, 545000000, time.UTC)),
				},
			},
		},
		{
			name: "optional values and missing reset",
			body: `{"usage":{"weekly":{"percent":12,"limit":1000,"used":120,"remaining":880}}}`,
			want: []model.ProviderQuotaWindow{
				{
					Label:     "weekly",
					Limit:     usageProbeTestPtr(int64(1000)),
					Used:      usageProbeTestPtr(int64(120)),
					Remaining: usageProbeTestPtr(int64(880)),
					Percent:   usageProbeTestPtr(float64(12)),
				},
			},
		},
		{
			name: "malformed windows skipped",
			body: `{"usage":{"rolling":{"percent":64,"resetsAt":"2026-09-16T13:45:31.545Z"},"weekly":{"percent":"bad","resetsAt":"2026-09-21T00:00:00.545Z"},"monthly":{"percent":32,"resetsAt":"invalid"}}}`,
			want: []model.ProviderQuotaWindow{
				{
					Label:    "rolling",
					Percent:  usageProbeTestPtr(float64(64)),
					ResetsAt: usageProbeTestPtr(time.Date(2026, 9, 16, 13, 45, 31, 545000000, time.UTC)),
				},
			},
		},
		{
			name: "out of range percent skipped",
			body: `{"usage":{"weekly":{"percent":101,"resetsAt":"2026-09-21T00:00:00Z"}}}`,
			want: []model.ProviderQuotaWindow{},
		},
		{
			name: "missing usage",
			body: `{}`,
		},
		{
			name: "malformed usage",
			body: `{"usage":"invalid"}`,
		},
		{
			name:    "invalid JSON",
			body:    `{"usage":`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errParse := parseOpenCodeGoQuota([]byte(test.body))
			if test.wantErr {
				if errParse == nil {
					t.Fatal("parse error = nil, want error")
				}
				return
			}
			if errParse != nil {
				t.Fatalf("parse error = %v", errParse)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("windows = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProviderQuotaFromWindowsHeadline(t *testing.T) {
	rolling := model.ProviderQuotaWindow{Label: "rolling", Percent: usageProbeTestPtr(float64(64))}
	weekly := model.ProviderQuotaWindow{Label: "weekly", Limit: usageProbeTestPtr(int64(1000)), Percent: usageProbeTestPtr(float64(65))}
	monthly := model.ProviderQuotaWindow{Label: "monthly", Percent: usageProbeTestPtr(float64(32))}
	tests := []struct {
		name       string
		windows    []model.ProviderQuotaWindow
		wantWindow *model.ProviderQuotaWindow
	}{
		{name: "prefers weekly", windows: []model.ProviderQuotaWindow{rolling, weekly, monthly}, wantWindow: &weekly},
		{name: "falls back to first", windows: []model.ProviderQuotaWindow{rolling, monthly}, wantWindow: &rolling},
		{name: "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quota := providerQuotaFromWindows(test.windows)
			if !reflect.DeepEqual(quota.Windows, test.windows) {
				t.Fatalf("windows = %#v, want %#v", quota.Windows, test.windows)
			}
			if test.wantWindow == nil {
				if quota.Limit != nil || quota.Used != nil || quota.Remaining != nil || quota.ResetsAt != nil {
					t.Fatalf("headline values = %+v, want nil", quota)
				}
				return
			}
			if quota.Limit != test.wantWindow.Limit || quota.Used != test.wantWindow.Used || quota.Remaining != test.wantWindow.Remaining || quota.ResetsAt != test.wantWindow.ResetsAt {
				t.Fatalf("headline values = %+v, want %+v", quota, test.wantWindow)
			}
		})
	}
}

func TestClassifyUsageProbeQuotaRequest(t *testing.T) {
	tests := []struct {
		name   string
		probe  string
		method string
		target string
		want   quotaRequestKind
	}{
		{name: "zai GET", probe: "zai", method: http.MethodGet, target: zaiUsageQuotaURL, want: quotaRequestCacheable},
		{name: "zai POST", probe: "zai", method: http.MethodPost, target: zaiUsageQuotaURL, want: quotaRequestNotCacheable},
		{name: "zai query", probe: "zai", method: http.MethodGet, target: zaiUsageQuotaURL + "?extra=1", want: quotaRequestNotCacheable},
		{name: "OpenCode Go GET", probe: "opencode-go", method: http.MethodGet, target: opencodeGoUsageQuotaURL, want: quotaRequestCacheable},
		{name: "OpenCode Go POST", probe: "opencode-go", method: http.MethodPost, target: opencodeGoUsageQuotaURL, want: quotaRequestNotCacheable},
		{name: "OpenCode Go query", probe: "opencode-go", method: http.MethodGet, target: opencodeGoUsageQuotaURL + "?extra=1", want: quotaRequestNotCacheable},
		{name: "wrong probe", probe: "zai", method: http.MethodGet, target: opencodeGoUsageQuotaURL, want: quotaRequestNotCacheable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, errParse := url.Parse(test.target)
			if errParse != nil {
				t.Fatal(errParse)
			}
			auth := &coreauth.Auth{Provider: "openai-compatible-probe", Attributes: map[string]string{"usage_probe": test.probe}}
			if got := classifyQuotaRequest(auth, test.method, target, ""); got != test.want {
				t.Fatalf("request kind = %d, want %d", got, test.want)
			}
		})
	}
}

func TestProbeUsageRejectsUnknownProbe(t *testing.T) {
	auth := &coreauth.Auth{Attributes: map[string]string{"usage_probe": "unknown"}}
	_, errProbe := probeUsage(context.Background(), &Handler{}, auth)
	if errProbe == nil || !strings.Contains(errProbe.Error(), `unsupported usage probe "unknown"`) {
		t.Fatalf("probe error = %v", errProbe)
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
