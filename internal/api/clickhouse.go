package api

import (
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
)

type clickHouseHandle struct{ handle *clickhouse.Handle }

func (s *Server) initClickHouse(cfg *config.Config) {
	s.clickHouse.handle = clickhouse.NewHandle(clickhouse.Config{DSN: cfg.ClickHouse.DSN, Database: cfg.ClickHouse.Database})
}
func (s *Server) ClickHouse() *clickhouse.Handle { return s.clickHouse.handle }
func (s *Server) closeClickHouse() error {
	if s.clickHouse.handle == nil {
		return nil
	}
	return errors.Join(s.clickHouse.handle.CloseAgent(), s.clickHouse.handle.Close())
}
func markClickHouseRestartRequired(service cpauk.Service, oldCfg, newCfg *config.Config) []string {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	var fields []string
	if oldCfg.ClickHouse.DSN != newCfg.ClickHouse.DSN {
		fields = append(fields, "clickhouse.dsn")
	}
	if oldCfg.ClickHouse.Database != newCfg.ClickHouse.Database {
		fields = append(fields, "clickhouse.database")
	}
	if marker, ok := service.(interface{ MarkRestartRequired([]string) }); ok && len(fields) > 0 {
		marker.MarkRestartRequired(fields)
	}
	return fields
}
