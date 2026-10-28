package clickhouse_test

import (
	"context"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestAgentSecurity(t *testing.T) {
	h := chtest.Handle(t)
	defer h.CloseAgent()
	ctx := context.Background()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().Exec(ctx, "CREATE TABLE `"+db.Database()+"`.sample (x UInt8) ENGINE=Memory"); err != nil {
		t.Fatal(err)
	}
	c, err := h.Agent(ctx, clickhouse.AgentLimits{MaxExecutionTime: time.Second * 30, MaxResultRows: 100, MaxMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	same, err := h.Agent(ctx, clickhouse.AgentLimits{MaxExecutionTime: 30 * time.Second, MaxResultRows: 100, MaxMemoryBytes: 1 << 30})
	if err != nil || same != c {
		t.Fatalf("idempotent provision: %v", err)
	}
	if err = db.Conn().Exec(ctx, "CREATE TABLE default.cpa_agent_test_protected (x UInt8) ENGINE=Memory"); err != nil {
		t.Fatal(err)
	}
	defer db.Conn().Exec(ctx, "DROP TABLE default.cpa_agent_test_protected")
	var visible uint64
	if err = c.QueryRow(ctx, "SELECT count() FROM system.tables WHERE database='default' AND name='cpa_agent_test_protected'").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("system tables leaked other database: count=%d err=%v", visible, err)
	}
	if err = c.Exec(ctx, "SELECT * FROM sample"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"INSERT INTO sample VALUES (1)", "CREATE TABLE evil (x UInt8) ENGINE=Memory", "CREATE TEMPORARY TABLE evil (x UInt8)", "SET max_execution_time=1000", "SELECT * FROM system.users", "SELECT * FROM default.cpa_agent_test_protected", "KILL QUERY WHERE 1", "SELECT * FROM url('http://127.0.0.1:1', 'CSV', 'x String')", "SELECT * FROM file('cpa-agent-test.txt', 'LineAsString')", "SELECT * FROM remote('127.0.0.1', 'default', 'sample')", "SELECT * FROM s3('http://127.0.0.1:1/a', 'CSV', 'x String')", "SELECT * FROM mysql('127.0.0.1:1', 'a', 'b', 'c', 'd')", "SELECT * FROM postgresql('127.0.0.1:1', 'a', 'b', 'c', 'd')", "SELECT * FROM executable('cat', 'LineAsString', 'x String')", "SELECT * FROM input('x UInt8')", "SELECT * FROM cluster('x', 'default', 'sample')"} {
		t.Run(sql, func(t *testing.T) {
			if err := c.Exec(ctx, sql); err == nil {
				t.Errorf("unexpectedly allowed %s", sql)
			} else {
				if strings.Contains(sql, "file(") && !strings.Contains(err.Error(), "READ ON FILE") {
					t.Errorf("file privilege was not checked: %v", err)
				}
				t.Log(err)
			}
		})
	}
}

func TestAgentCancellation(t *testing.T) {
	h := chtest.Handle(t)
	defer h.CloseAgent()
	ctx := context.Background()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := h.Agent(ctx, clickhouse.AgentLimits{MaxExecutionTime: 30 * time.Second, MaxResultRows: 100, MaxMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queryCtx = ch.Context(queryCtx, ch.WithQueryID("cpa-cancel-"+db.Database()))
	done := make(chan error, 1)
	go func() {
		rows, e := conn.Query(queryCtx, "SELECT sleepEachRow(0.00003) FROM numbers(100000)")
		if e == nil {
			for rows.Next() {
			}
			e = rows.Err()
			_ = rows.Close()
		}
		done <- e
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count uint64
		err = db.Conn().QueryRow(ctx, "SELECT count() FROM system.processes WHERE query_id = ?", "cpa-cancel-"+db.Database()).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("query did not start")
		}
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("canceled query succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("query did not cancel")
	}
}
