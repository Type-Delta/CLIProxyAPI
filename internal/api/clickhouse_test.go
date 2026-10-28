package api

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClickHouseRestartRequired(t *testing.T) {
	old := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	next := *old
	next.ClickHouse.DSN = "clickhouse://localhost:9000"
	next.ClickHouse.Database = "other"
	if got := markClickHouseRestartRequired(nil, old, &next); !reflect.DeepEqual(got, []string{"clickhouse.dsn", "clickhouse.database"}) {
		t.Fatalf("restart fields: %v", got)
	}
	next = *old
	next.ClickHouse.AgentQuery.MaxResultRows++
	if got := markClickHouseRestartRequired(nil, old, &next); len(got) != 0 {
		t.Fatalf("unexpected restart fields: %v", got)
	}
}
