package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

func TestTelemetryLifecycleDrainsAndStops(t *testing.T) {
	ctx := context.Background()
	h := chtest.Handle(t)
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	dir := t.TempDir()
	if err = outbox.Write(dir, &record.Request{RequestID: "lifecycle", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"live"}]}`)}); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandler(cfg, "", nil)}
	s.clickHouse.handle = h
	s.startTelemetry(dir)
	defer s.stopTelemetry()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("ingestion did not complete: %+v", s.TelemetryStatus())
		case <-ticker.C:
			paths, e := outbox.List(dir)
			if e != nil {
				t.Fatal(e)
			}
			if len(paths) == 0 {
				var n uint64
				if e = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE request_id='lifecycle'").Scan(&n); e != nil || n != 1 {
					t.Fatalf("request count %d: %v", n, e)
				}
				digest := sha256.Sum256([]byte(db.Database()))
				var users uint64
				if e = db.Conn().QueryRow(ctx, "SELECT count() FROM system.users WHERE name = ?", "cpa_agent_"+hex.EncodeToString(digest[:8])).Scan(&users); e != nil || users != 1 {
					t.Fatalf("startup agent user count %d: %v", users, e)
				}
				if e = outbox.Write(dir, &record.Request{RequestID: "shutdown-final", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"flush on shutdown"}]}`)}); e != nil {
					t.Fatal(e)
				}
				s.stopTelemetry()
				if e = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE request_id='shutdown-final'").Scan(&n); e != nil || n != 1 {
					t.Fatalf("shutdown record count %d: %v", n, e)
				}
				remaining, e := outbox.List(dir)
				if e != nil || len(remaining) != 0 {
					t.Fatalf("shutdown outbox: %v %v", remaining, e)
				}
				select {
				case <-s.telemetry.done:
				default:
					t.Fatal("telemetry did not stop")
				}
				return
			}
		}
	}
}
