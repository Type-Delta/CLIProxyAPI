package management

import "github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"

type clickHouseHandle struct{ handle *clickhouse.Handle }

func (h *Handler) SetClickHouse(handle *clickhouse.Handle) { h.clickHouse.handle = handle }
func (h *Handler) ClickHouse() *clickhouse.Handle          { return h.clickHouse.handle }
