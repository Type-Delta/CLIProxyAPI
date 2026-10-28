package api

import (
	"context"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestTelemetryQueryHTTPDisconnectCancelsClickHouse(t *testing.T) {
	h := chtest.Handle(t)
	defer h.CloseAgent()
	db, err := h.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	hashed, errHash := bcrypt.GenerateFromPassword([]byte("management-disconnect"), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	cfg.RemoteManagement.SecretKey = string(hashed)
	cfg.RemoteManagement.AllowRemote = true
	s := newTestServerWithConfig(t, cfg)
	s.mgmt.SetClickHouse(h)
	defer s.Stop(context.Background())
	live := httptest.NewServer(s.engine)
	defer live.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// sleepEachRow keeps a real query running long enough to observe cancellation.
	req, _ := http.NewRequestWithContext(ctx, "POST", live.URL+"/v8/management/telemetry/query", strings.NewReader(`{"sql":"SELECT sleepEachRow(0.00003) /* http-disconnect */ FROM numbers(100000)"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer management-disconnect")
	done := make(chan error, 1)
	go func() {
		res, e := http.DefaultClient.Do(req)
		if e == nil {
			data, readErr := io.ReadAll(res.Body)
			e = readErr
			if res.StatusCode != 200 {
				e = fmt.Errorf("HTTP %d: %s", res.StatusCode, data)
			}
			res.Body.Close()
		}
		done <- e
	}()
	var id string
	waitTelemetryCondition(t, func() bool {
		select {
		case e := <-done:
			t.Fatalf("query ended before cancellation: %v", e)
		default:
		}
		rows, e := db.Conn().Query(context.Background(), "SELECT query_id FROM system.processes WHERE current_database = ? AND query LIKE '%http-disconnect%' AND query NOT LIKE '%system.processes%'", db.Database())
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		if rows.Next() {
			if e = rows.Scan(&id); e != nil {
				t.Fatal(e)
			}
			return true
		}
		return false
	})
	cancel()
	if e := <-done; e == nil {
		t.Fatal("HTTP request completed instead of disconnecting")
	}
	waitTelemetryCondition(t, func() bool {
		var n uint64
		if e := db.Conn().QueryRow(context.Background(), "SELECT count() FROM system.processes WHERE query_id = ?", id).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n == 0
	})
	t.Logf("ClickHouse query %s disappeared from system.processes after HTTP disconnect", id)
}
