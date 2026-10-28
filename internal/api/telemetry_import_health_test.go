package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestTelemetryHealthLegacyImport(t *testing.T) {
	handle := chtest.Handle(t)
	db, err := handle.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.clickHouse.handle = handle
	read := func() map[string]json.RawMessage {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/v8/management/telemetry/health", nil)
		s.telemetryHealth(c)
		if w.Code != 200 {
			t.Fatalf("health: %d %s", w.Code, w.Body)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if _, present := read()["legacy_import"]; present {
		t.Fatal("missing job must omit legacy_import")
	}
	for _, state := range []string{"pending", "running", "done", "failed"} {
		detail := `{"files_seen":10,"imported":8,"skipped_unparseable":2,"records":9,"last":"file.log","last_at":"2026-10-28T00:00:00Z"}`
		if state == "failed" {
			detail = `{"files_seen":10,"imported":8,"skipped_unparseable":2,"records":9,"error":"permission denied"}`
		}
		if err := db.SetJob(context.Background(), store.LegacyJob, state, detail); err != nil {
			t.Fatal(err)
		}
		var status struct {
			State  string
			Counts map[string]int64
			Reason string
		}
		if err := json.Unmarshal(read()["legacy_import"], &status); err != nil {
			t.Fatal(err)
		}
		if status.State != state || status.Counts["files_seen"] != 10 || status.Counts["imported"] != 8 || status.Counts["skipped_unparseable"] != 2 || status.Counts["records"] != 9 || len(status.Counts) != 4 {
			t.Fatalf("state=%s status=%+v", state, status)
		}
		if state == "failed" && status.Reason != "permission denied" {
			t.Fatalf("failure reason=%q", status.Reason)
		}
		if state != "failed" && status.Reason != "" {
			t.Fatalf("unexpected reason=%q", status.Reason)
		}
	}
}
