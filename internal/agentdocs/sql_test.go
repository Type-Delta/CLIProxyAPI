package agentdocs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentdocs"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestSQLExamplesThroughAgentQuery(t *testing.T) {
	handle := chtest.Handle(t)
	db, err := handle.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for component, path := range map[string]string{"analytics": "../cpauk/chstore/migrations", "telemetry": "../telemetry/store/migrations"} {
		if _, err := db.Migrate(context.Background(), component, os.DirFS(path)); err != nil {
			t.Fatal(err)
		}
	}

	// Seed the documented incident window with canonical identities and near misses.
	ctx := context.Background()
	table := "`" + db.Database() + "`."
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	sessions := []string{"claude:xb12345", "claude:xb12345:agent:worker", "codex:xb12345", "header:xb12345", "session:xb12345"}
	decoys := []string{"claude:xb123450", "claude:other:xb12345", "claude:xb12345-extra", "claude:xb12345_agent:other", "claude:xb%_123", "claude:xbAA123"}
	for index, id := range append(append([]string{}, sessions...), decoys...) {
		requestID := "request-" + strconv.Itoa(index)
		if index == 0 {
			requestID = "REQUEST_LOG_ID"
		}
		if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"requests (request_id,proxy_request_id,started_at,path,model,stream,status,session_id,envelope_content_id) VALUES (?,?,?,?,?,?,?,?,?)", requestID, "proxy-"+strconv.Itoa(index), at, "/v1/messages", "m", index == 0, uint16(403), id, "envelope"); err != nil {
			t.Fatal(err)
		}
		if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"attempts (request_id,attempt_index,request_started_at,provider,status,response_body) VALUES (?,?,?,?,?,?)", requestID, uint32(1), at, "claude", uint16(403), `{"error":"forbidden"}`); err != nil {
			t.Fatal(err)
		}
	}
	// A successful non-streaming request supplies the comparison denominator.
	if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"requests (request_id,started_at,path,model,stream,status,session_id) VALUES (?,?,?,?,?,?,?)", "successful-request", at, "/v1/messages", "m", false, uint16(200), "claude:xb12345"); err != nil {
		t.Fatal(err)
	}
	// A repeated failing attempt must not count as another client request.
	if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"attempts (request_id,attempt_index,request_started_at,provider,status) VALUES (?,?,?,?,?)", "REQUEST_LOG_ID", uint32(2), at, "claude", uint16(403)); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, raw, text string }{
		{"envelope", `{"model":"m","stream":true,"tools":[{"name":"AAA"}]}`, ""},
		{"skill", `{"role":"user","content":"use AAA"}`, "use AAA"},
	} {
		if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"transcript_content (content_id,raw,text) VALUES (?,?,?)", row.id, row.raw, row.text); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"transcript_entries (entry_id,content_id,occurred_at,first_attempt_index) VALUES (?,?,?,?)", "entry-AAA", "skill", time.Now().UTC(), uint32(0)); err != nil {
		t.Fatal(err)
	}
	// Identical opening prompts share one entry across two sessions; history repeats
	// within a session and translated upstream views must not inflate the count.
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		request, session string
		attempt          uint32
		at               time.Time
	}{
		{"skill-one", "claude:skill-one", 0, now},
		{"skill-one-replay", "claude:skill-one", 0, now},
		{"skill-two", "claude:skill-two", 0, now},
		{"skill-two-translated", "claude:upstream-only", 1, now},
		{"skill-old", "claude:old", 0, monthStart.Add(-time.Hour)},
		{"skill-old-replay", "claude:old", 0, now},
	} {
		if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"request_contexts (request_id,session_id,attempt_index,started_at,entry_ids) VALUES (?,?,?,?,?)", row.request, row.session, row.attempt, row.at, []string{"entry-AAA"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Conn().Exec(ctx, "INSERT INTO "+table+"analytics_events (attempt_id,proxy_request_id,requested_at_ns,provider,credential_id,routing_trace_json,version) VALUES (?,?,?,?,?,?,?)", "attempt-1", "proxy-0", at.UnixNano(), "claude", "credential", `{"version":1}`, uint64(1)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	h := management.NewHandler(cfg, "", nil)
	h.SetClickHouse(handle)
	router := gin.New()
	router.POST("/v8/management/telemetry/query", h.PostTelemetryQuery)
	doc, err := agentdocs.Files.ReadFile("sql.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```sql\n(.*?)\n```").FindAllStringSubmatch(string(doc), -1)
	if len(blocks) == 0 {
		t.Fatal("no SQL examples found")
	}

	type queryResult struct {
		QueryID string `json:"query_id"`
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	}
	run := func(t *testing.T, sql string, params map[string]any) queryResult {
		t.Helper()
		body, err := json.Marshal(map[string]any{"sql": sql, "params": params, "max_rows": 100})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v8/management/telemetry/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("query failed: %d %s", w.Code, w.Body)
		}
		var result queryResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.QueryID == "" || len(result.Columns) == 0 || len(result.Rows) == 0 {
			t.Fatalf("expected seeded result: %s", w.Body)
		}
		return result
	}
	var selectedIDs []string
	for i, block := range blocks {
		t.Run("example_"+strconv.Itoa(i+1), func(t *testing.T) {
			params := map[string]any{}
			for _, line := range strings.Split(block[1], "\n") {
				if value, ok := strings.CutPrefix(line, "-- test-params: "); ok {
					if err := json.Unmarshal([]byte(value), &params); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, ok := params["ids"]; ok {
				if len(selectedIDs) != 2 {
					t.Fatal("resolver did not supply main and subagent IDs")
				}
				params["ids"] = selectedIDs
			}
			result := run(t, block[1], params)
			if _, ok := params["skill"]; ok {
				want := [][]any{{"claude:skill-one", float64(1)}, {"claude:skill-two", float64(1)}}
				if !reflect.DeepEqual(result.Rows, want) {
					t.Fatalf("shared AAA entry must count once in each session, excluding replay, old and upstream views: got %v want %v", result.Rows, want)
				}
			}
			if _, ok := params["s"]; ok {
				var got []string
				for _, row := range result.Rows {
					id := row[0].(string)
					got = append(got, id)
					if strings.HasPrefix(id, "claude:") {
						selectedIDs = append(selectedIDs, id)
					}
				}
				want := append([]string{}, sessions...)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("resolved %v, want %v without near-match decoys", got, want)
				}
				// SQL wildcards inside the supplied ID must remain literal.
				params["s"] = "xb%_123"
				literal := run(t, block[1], params)
				if len(literal.Rows) != 1 || literal.Rows[0][0] != "claude:xb%_123" {
					t.Fatalf("wildcard ID matched other sessions: %v", literal.Rows)
				}
			}
			for column, meta := range result.Columns {
				if meta.Name == "requests" {
					total := float64(0)
					for _, row := range result.Rows {
						total += row[column].(float64)
					}
					if len(result.Rows) != 3 || total != 3 {
						t.Fatalf("expected failure and success groups with three distinct requests: %v", result.Rows)
					}
					statuses := map[float64]bool{}
					for _, row := range result.Rows {
						statuses[row[3].(float64)] = true
					}
					if !statuses[200] || !statuses[403] {
						t.Fatalf("missing success/failure comparison: %v", result.Rows)
					}
				}
				if meta.Name == "client_envelope" {
					for _, row := range result.Rows {
						if !strings.Contains(row[column].(string), `"tools"`) {
							t.Fatalf("missing captured tools in envelope: %v", row)
						}
					}
				}
			}
		})
	}
	t.Logf("executed %d SQL examples through restricted account against seeded traffic", len(blocks))
}

// TestSQLSessionPrefixesMatchCode keeps the documented session namespaces in step with session extraction.
func TestSQLSessionPrefixesMatchCode(t *testing.T) {
	doc, err := agentdocs.Files.ReadFile("sql.md")
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`\['claude:'[^\]]*\]`).Find(doc)
	if list == nil {
		t.Fatal("sql.md session lookup prefix list not found")
	}
	var documented []string
	for _, match := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(list, -1) {
		documented = append(documented, string(match[1]))
	}
	want := append([]string(nil), session.CandidateSessionPrefixes...)
	sort.Strings(documented)
	sort.Strings(want)
	if !reflect.DeepEqual(documented, want) {
		t.Fatalf("sql.md session prefixes = %v, want session.CandidateSessionPrefixes %v", documented, want)
	}
}
