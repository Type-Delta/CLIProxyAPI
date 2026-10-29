package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestTelemetryQueryAndSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	handle := chtest.Handle(t)
	db, err := handle.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for component, path := range map[string]string{"analytics": "../../../cpauk/chstore/migrations", "telemetry": "../../../telemetry/store/migrations"} {
		if _, err = db.Migrate(ctx, component, os.DirFS(path)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.ClickHouse = config.DefaultClickHouseConfig()
	h := NewHandler(cfg, "", nil)
	h.SetClickHouse(handle)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost {
			c.Request.Header.Set("Content-Type", "application/json")
			h.PostTelemetryQuery(c)
		} else {
			h.GetTelemetrySchema(c)
		}
		return w
	}
	w := run("POST", "/", `{"sql":"SELECT {value:UInt64} AS v, toDateTime('2020-01-01 00:00:00', 'UTC') AS t, [toUInt64(9007199254740992)] AS a", "params":{"value":9007199254740992}}`)
	if w.Code != 200 {
		t.Fatalf("query: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"9007199254740992"`) || !strings.Contains(w.Body.String(), "2020-01-01T00:00:00Z") {
		t.Fatal(w.Body)
	}
	w = run("POST", "/", `{"sql":"SELECT number FROM numbers(10)","max_rows":2}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"truncated":true`) {
		t.Fatal(w.Code, w.Body)
	}
	w = run("GET", "/", "")
	if w.Code != 200 {
		t.Fatalf("schema: %d %s", w.Code, w.Body)
	}
	var schema struct {
		Examples []struct{ Name, SQL string }
		Tables   []struct {
			Name, Comment string
			Columns       []struct{ Name, Comment string }
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	for _, table := range schema.Tables {
		if table.Name != "transcript_entries" && table.Name != "transcript_content" {
			continue
		}
		if !strings.Contains(table.Comment, "request_contexts") {
			t.Errorf("table %s missing per-session guidance: %s", table.Name, table.Comment)
		}
		for _, column := range table.Columns {
			if table.Name == "transcript_entries" && column.Name == "entry_id" || table.Name == "transcript_content" && column.Name == "text" {
				if !strings.Contains(column.Comment, "request_contexts") || strings.Contains(column.Comment, "Count distinct entry_id") {
					t.Errorf("column %s.%s has stale counting guidance: %s", table.Name, column.Name, column.Comment)
				}
			}
		}
	}
	if len(schema.Examples) < 5 {
		t.Fatal(w.Body)
	}
	for _, example := range schema.Examples {
		t.Run(example.Name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"sql": example.SQL, "params": map[string]string{"session": "xb12345", "skill": "AAA", "zone": "UTC"}})
			w = run("POST", "/", string(b))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	h.SetClickHouse(nil)
	w = run("POST", "/", `{ "sql": "SELECT 1" }`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Fatal(w.Code, w.Body)
	}
	h.SetClickHouse(handle)
	cfg.ClickHouse.AgentQuery.Enabled = false
	h.SetConfig(cfg)
	w = run("POST", "/", `{"sql":"SELECT 1"}`)
	if w.Code != 503 || !bytes.Contains(w.Body.Bytes(), []byte("disabled")) {
		t.Fatal(w.Code, w.Body)
	}
}
