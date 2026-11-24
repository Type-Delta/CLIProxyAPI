package chstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestProxyRequestIDWhereIsParameterized(t *testing.T) {
	q := model.Query{Operation: model.OperationEvents, Filters: map[string]json.RawMessage{"proxy_request_id": json.RawMessage(`["0123456789abcdef0123456789abcdef"]`)}}
	where, args, err := buildWhere(q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "proxy_request_id IN (?)") || len(args) != 3 || args[2] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("where=%s args=%v", where, args)
	}
}

func TestProxyRequestIDFilterClickHouse(t *testing.T) {
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	first, second := events[0], events[1]
	first.ProxyRequestID = "0123456789abcdef0123456789abcdef"
	second.ProxyRequestID = "fedcba9876543210fedcba9876543210"
	if err := s.WriteBatch(context.Background(), []model.Event{first, second}); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: model.QuerySchemaVersion, Operation: model.OperationEvents, Start: first.RequestedAt.Add(-time.Hour), End: first.RequestedAt.Add(24 * time.Hour), TimeZone: "UTC", Filters: map[string]json.RawMessage{"proxy_request_id": json.RawMessage(`"0123456789abcdef0123456789abcdef"`)}}
	page, err := s.Events(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ProxyRequestID != first.ProxyRequestID {
		t.Fatalf("page %+v", page)
	}
}
