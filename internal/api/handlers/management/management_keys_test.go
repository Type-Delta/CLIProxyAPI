package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func scopedManagementFixture(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# preserved\nserver: {port: 8317}\nmanagement:\n  allow-remote: true\n  keys:\n  - {id: owner, name: Owner, secret-key: owner-secret, scope: full-access}\n  - {id: viewer, name: Viewer, secret-key: viewer-secret, scope: read-only}\nplugins:\n  configs:\n    opaque: {custom: untouched}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	r := gin.New()
	v8 := r.Group("/v8/management", h.Middleware(), func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	v8.GET("/session", h.GetManagementSession)
	v8.GET("/management-keys", h.ListManagementKeys)
	v8.POST("/management-keys", h.CreateManagementKey)
	v8.PATCH("/management-keys/:id", h.PatchManagementKey)
	v8.DELETE("/management-keys/:id", h.DeleteManagementKey)
	v8.GET("/config", h.ConfigV8)
	v8.GET("/config/*path", h.ConfigV8)
	v8.PUT("/config/*path", h.ConfigV8)
	return h, r
}

func scopedRequest(r http.Handler, method, path, secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	return res
}

func TestManagementKeysCRUDAndSessions(t *testing.T) {
	h, r := scopedManagementFixture(t)
	for _, tc := range []struct{ secret, scope string }{{"owner-secret", "full-access"}, {"viewer-secret", "read-only"}} {
		res := scopedRequest(r, "GET", "/v8/management/session", tc.secret, "")
		if res.Code != 200 || res.Body.String() != `{"scope":"`+tc.scope+`"}` {
			t.Fatal(res.Code, res.Body)
		}
	}
	res := scopedRequest(r, "POST", "/v8/management/management-keys", "owner-secret", `{"name":"New dashboard","key":"new-secret","scope":"read-only"}`)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body)
	}
	var entry config.ManagementKey
	if json.Unmarshal(res.Body.Bytes(), &entry) != nil || entry.ID == "" || entry.Name != "New dashboard" || entry.Scope != "read-only" {
		t.Fatal(res.Body)
	}
	if strings.Contains(res.Body.String(), "secret") || strings.Contains(res.Body.String(), "$2") {
		t.Fatal("create leaked secret")
	}
	res = scopedRequest(r, "GET", "/v8/management/session", "new-secret", "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), "read-only") {
		t.Fatal("new key not immediately usable", res.Code, res.Body)
	}
	res = scopedRequest(r, "PATCH", "/v8/management/management-keys/"+entry.ID, "owner-secret", `{"name":"Admin","key":"rotated-secret","scope":"full-access"}`)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body)
	}
	if res = scopedRequest(r, "GET", "/v8/management/session", "new-secret", ""); res.Code != 401 {
		t.Fatal("old key remained usable", res.Code)
	}
	if res = scopedRequest(r, "GET", "/v8/management/session", "rotated-secret", ""); res.Code != 200 || !strings.Contains(res.Body.String(), "full-access") {
		t.Fatal(res.Code, res.Body)
	}
	for _, route := range []string{"/management-keys", "/config", "/config/management", "/config/management/keys"} {
		res = scopedRequest(r, "GET", "/v8/management"+route, "owner-secret", "")
		if res.Code != 200 || strings.Contains(res.Body.String(), "$2") || strings.Contains(res.Body.String(), "rotated-secret") {
			t.Fatalf("metadata leak %s: %d %s", route, res.Code, res.Body)
		}
	}
	if res = scopedRequest(r, "GET", "/v8/management/config/management/keys", "viewer-secret", ""); res.Code != 403 {
		t.Fatal(res.Code, res.Body)
	}
	saved, _ := os.ReadFile(h.configFilePath)
	if strings.Contains(string(saved), "rotated-secret") || !strings.Contains(string(saved), "# preserved") || !strings.Contains(string(saved), "untouched") {
		t.Fatal("bad persistence")
	}
	reloaded, err := config.LoadConfig(h.configFilePath)
	if err != nil || len(reloaded.RemoteManagement.Keys) != 3 {
		t.Fatal("reload", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(reloaded.RemoteManagement.Keys[2].SecretKey), []byte("rotated-secret")) != nil {
		t.Fatal("missing persisted hash")
	}
	res = scopedRequest(r, "DELETE", "/v8/management/management-keys/"+entry.ID, "owner-secret", "")
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body)
	}
	if res = scopedRequest(r, "GET", "/v8/management/session", "rotated-secret", ""); res.Code != 401 {
		t.Fatal("deleted key remained usable")
	}
}

func TestManagementKeysInvalidWritesAndNonHTTP(t *testing.T) {
	h, r := scopedManagementFixture(t)
	before, _ := os.ReadFile(h.configFilePath)
	for _, body := range []string{`{}`, `{"name":"","key":"x","scope":"read-only"}`, `{"name":"Bad","key":"x","scope":"admin"}`, `{"name":"Bad","key":"","scope":"full-access"}`} {
		res := scopedRequest(r, "POST", "/v8/management/management-keys", "owner-secret", body)
		if res.Code != 400 {
			t.Fatal(res.Code, res.Body)
		}
	}
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		path := "/v8/management/management-keys"
		if method == "PATCH" || method == "DELETE" {
			path += "/owner"
		}
		res := scopedRequest(r, method, path, "viewer-secret", `{"name":"Stolen","key":"x","scope":"full-access"}`)
		if res.Code != 403 {
			t.Fatal(method, res.Code, res.Body)
		}
	}
	after, _ := os.ReadFile(h.configFilePath)
	if string(before) != string(after) {
		t.Fatal("denied writes changed config")
	}
	if allowed, status, _ := h.AuthenticateManagementKey("127.0.0.1", true, "viewer-secret"); allowed || status != 403 {
		t.Fatal("read-only non-HTTP auth allowed")
	}
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "owner-secret"); !allowed {
		t.Fatal("full non-HTTP auth denied")
	}
	h.SetLocalPassword("local-password")
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "local-password"); !allowed {
		t.Fatal("local password denied")
	}
	h.envSecret = "environment-password"
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "environment-password"); !allowed {
		t.Fatal("environment password denied")
	}
	legacyHash, _ := bcrypt.GenerateFromPassword([]byte("legacy-password"), bcrypt.MinCost)
	h.cfg.RemoteManagement.SecretKey = string(legacyHash)
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "legacy-password"); !allowed {
		t.Fatal("legacy password denied")
	}
	h.cfg.RemoteManagement.Keys[1].Scope = "unknown"
	if _, allowed, _, _ := h.authenticateManagementScope("127.0.0.1", true, "viewer-secret"); allowed {
		t.Fatal("unknown scope authenticated")
	}
}

func TestReadOnlyPricingGETDoesNotRefresh(t *testing.T) {
	service := &catalogProvidersHandlerService{analyticsHandlerService: &analyticsHandlerService{}}
	h := &Handler{analytics: service}
	c, res := v2Request(http.MethodGet, "/v8/management/analytics/pricing/catalog-providers", "")
	c.Set(managementScopeContextKey, config.ManagementScopeReadOnly)
	h.GetAnalyticsPricingCatalogProviders(c)
	if res.Code != 200 || service.refreshed {
		t.Fatal("GET refreshed pricing", res.Code, res.Body)
	}
}

func TestReadOnlyTelemetrySQLSafety(t *testing.T) {
	handle := chtest.Handle(t)
	defer handle.CloseAgent()
	db, err := handle.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	table := "`" + db.Database() + "`.sample"
	if err := db.Conn().Exec(context.Background(), "CREATE TABLE "+table+" (value UInt8) ENGINE=Memory"); err != nil {
		t.Fatal(err)
	}
	h, r := scopedManagementFixture(t)
	h.SetClickHouse(handle)
	r.POST("/v8/management/telemetry/query", h.Middleware(), h.PostTelemetryQuery)
	query := func(sql string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		return scopedRequest(r, "POST", "/v8/management/telemetry/query", "viewer-secret", string(body))
	}
	if res := query("SELECT 42 AS value"); res.Code != 200 || !strings.Contains(res.Body.String(), `[[42]]`) {
		t.Fatal("safe SQL failed", res.Code, res.Body)
	}
	for _, sql := range []string{
		"INSERT INTO sample VALUES (1)", "DROP TABLE sample", "CREATE TABLE evil (x UInt8) ENGINE=Memory",
		"SET readonly=0", "SELECT 1 SETTINGS readonly=0", "SELECT * FROM system.users",
		"SELECT * FROM url('http://127.0.0.1:1', 'CSV', 'x String')",
		"SELECT * FROM file('config.yaml', 'LineAsString')",
		"SELECT * FROM remote('127.0.0.1', 'default', 'sample')", "SELECT 1; DROP TABLE sample",
	} {
		t.Run(sql, func(t *testing.T) {
			if res := query(sql); res.Code != 400 {
				t.Fatal("unsafe SQL accepted", res.Code, res.Body)
			}
		})
	}
	var count uint64
	if err := db.Conn().QueryRow(context.Background(), "SELECT count() FROM "+table).Scan(&count); err != nil || count != 0 {
		t.Fatal("SQL changed data", count, err)
	}
}

func TestReadOnlyAnalyticsExports(t *testing.T) {
	h, router := scopedManagementFixture(t)
	h.analytics = &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}
	router.POST("/v8/management/analytics/exports", h.Middleware(), h.CreateAnalyticsExport)
	for _, format := range []string{"csv", "json"} {
		body := `{"query":{"schema_version":2,"operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC"},"format":"` + format + `"}`
		res := scopedRequest(router, http.MethodPost, "/v8/management/analytics/exports", "viewer-secret", body)
		if res.Code != http.StatusOK || !strings.Contains(res.Header().Get("Content-Disposition"), "analytics-events."+format) {
			t.Fatalf("%s: %d %s", format, res.Code, res.Body)
		}
	}
}

func TestReadOnlyAnalyticsQueryRejectsSQL(t *testing.T) {
	h, r := scopedManagementFixture(t)
	r.POST("/v8/management/analytics/query", h.Middleware(), h.PostAnalyticsQuery)
	for _, body := range []string{`{"sql":"DROP TABLE sample"}`, `{"operation":"delete"}`, `{"operation":"summary","sql":"SELECT * FROM secrets"}`} {
		res := scopedRequest(r, "POST", "/v8/management/analytics/query", "viewer-secret", body)
		if res.Code != 400 {
			t.Fatal("invalid analytics operation accepted", res.Code, res.Body)
		}
	}
}

func TestManagementKeysRedactedConfigRoundTrip(t *testing.T) {
	h, r := scopedManagementFixture(t)
	res := scopedRequest(r, "GET", "/v8/management/config/management/keys", "owner-secret", "")
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body)
	}
	body := res.Body.String()
	res = scopedRequest(r, "PUT", "/v8/management/config/management/keys", "owner-secret", body)
	if res.Code != 200 {
		t.Fatal("redacted roundtrip rejected", res.Code, res.Body)
	}
	if res = scopedRequest(r, "GET", "/v8/management/session", "viewer-secret", ""); res.Code != 200 {
		t.Fatal("redacted roundtrip revoked key", res.Code, res.Body)
	}
	raw, _ := os.ReadFile(h.configFilePath)
	if strings.Contains(string(raw), "viewer-secret") {
		t.Fatal("plaintext persisted")
	}
}

func TestManagementKeysPreserveEntryComments(t *testing.T) {
	h, r := scopedManagementFixture(t)
	raw, _ := os.ReadFile(h.configFilePath)
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys := configV8Node(doc.Content[0], []string{"management", "keys"})
	keys.Content[1].HeadComment = "keep viewer comment"
	raw, _ = yaml.Marshal(&doc)
	if err := os.WriteFile(h.configFilePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	res := scopedRequest(r, "PATCH", "/v8/management/management-keys/viewer", "owner-secret", `{"name":"Observer"}`)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body)
	}
	after, _ := os.ReadFile(h.configFilePath)
	if !strings.Contains(string(after), "# keep viewer comment") {
		t.Fatal("entry comment lost")
	}
}

func TestManagementKeysConfigAliasesDoNotLeakHashes(t *testing.T) {
	h, r := scopedManagementFixture(t)
	hash := h.cfg.RemoteManagement.Keys[1].SecretKey
	raw := "shared: &keys [{id: viewer, name: Viewer, secret-key: '" + hash + "', scope: read-only}]\nmanagement: {keys: *keys}\n"
	if err := os.WriteFile(h.configFilePath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	res := scopedRequest(r, "GET", "/v8/management/config", "owner-secret", "")
	if res.Code != 200 || strings.Contains(res.Body.String(), hash) {
		t.Fatal("aliased hash exposed", res.Code, res.Body)
	}
}
