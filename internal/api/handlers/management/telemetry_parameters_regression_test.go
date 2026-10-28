package management

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestTelemetryStructuredParameters(t *testing.T) {
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	h := NewHandler(cfg, "", nil)
	h.SetClickHouse(chtest.Handle(t))
	special := "quote'slash\\\n\t"
	for _, tc := range []struct {
		sql    string
		params map[string]any
		want   any
	}{
		{"SELECT isNull({n:Nullable(String)})", map[string]any{"n": nil}, float64(1)},
		{"SELECT {a:Array(String)}", map[string]any{"a": []string{"hello", special}}, []any{"hello", special}},
		{"SELECT {m:Map(String,UInt64)}['a']", map[string]any{"m": map[string]any{"a": 2}}, float64(2)},
		{"SELECT {m:Map(String,Array(Nullable(String)))}['a']", map[string]any{"m": map[string]any{"a": []any{special, nil, "NULL"}}}, []any{special, nil, "NULL"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"sql": tc.sql, "params": tc.params})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.PostTelemetryQuery(c)
			if w.Code != 200 {
				t.Fatalf("structured parameter rejected: %d %s", w.Code, w.Body)
			}
			var got struct{ Rows [][]any }
			if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Rows) != 1 || len(got.Rows[0]) != 1 || !reflect.DeepEqual(got.Rows[0][0], tc.want) {
				t.Fatalf("parameter value changed: got=%s want=%#v", w.Body, tc.want)
			}
		})
	}
}
