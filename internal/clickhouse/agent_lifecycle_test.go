package clickhouse_test

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"testing"
	"time"
)

func TestAgentSwapDrainsRows(t *testing.T) {
	h := chtest.Handle(t)
	ctx := context.Background()
	limits := clickhouse.AgentLimits{MaxExecutionTime: 30 * time.Second, MaxResultRows: 100, MaxMemoryBytes: 1 << 30}
	conn, err := h.Agent(ctx, limits)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "SELECT number FROM numbers(10)")
	if err != nil {
		t.Fatal(err)
	}
	swapped := make(chan error, 1)
	limits.MaxResultRows++
	go func() { _, err := h.Agent(ctx, limits); swapped <- err }()
	select {
	case err := <-swapped:
		t.Fatalf("swap closed connection before rows drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-swapped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("swap did not finish")
	}
}
func TestAgentResultBytesCannotBeRaised(t *testing.T) {
	h := chtest.Handle(t)
	c, err := h.Agent(context.Background(), clickhouse.AgentLimits{MaxExecutionTime: 30 * time.Second, MaxResultRows: 100, MaxMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	var limit uint64
	if err := c.QueryRow(context.Background(), "SELECT toUInt64(getSetting('max_result_bytes'))").Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != 64<<20 {
		t.Fatalf("result bytes default=%d", limit)
	}
	if err := c.Exec(context.Background(), "SELECT 1 SETTINGS max_result_bytes=1000000000"); err == nil {
		t.Fatal("max_result_bytes is not CONST")
	}
}
