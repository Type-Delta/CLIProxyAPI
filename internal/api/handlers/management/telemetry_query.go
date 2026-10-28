package management

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	log "github.com/sirupsen/logrus"
)

//go:embed telemetry_query_examples.json
var telemetryExamples embed.FS

type telemetryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type telemetryResult struct {
	QueryID   string            `json:"query_id"`
	Columns   []telemetryColumn `json:"columns"`
	Rows      [][]any           `json:"rows"`
	RowCount  int               `json:"row_count"`
	Truncated bool              `json:"truncated"`
	ElapsedMS int64             `json:"elapsed_ms"`
	RowsRead  uint64            `json:"rows_read"`
	BytesRead uint64            `json:"bytes_read"`
}

func (h *Handler) telemetryConnection(ctx context.Context) (driver.Conn, string, int, error) {
	h.mu.Lock()
	cfg := h.cfg.ClickHouse
	h.mu.Unlock()
	if !cfg.AgentQuery.Enabled {
		return nil, "", 0, fmt.Errorf("agent SQL queries are disabled")
	}
	if h.ClickHouse() == nil || !h.ClickHouse().Configured() {
		return nil, "", 0, fmt.Errorf("clickhouse is not configured")
	}
	db, err := h.ClickHouse().Get(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	c, err := h.ClickHouse().Agent(ctx, clickhouse.AgentLimits{MaxExecutionTime: cfg.AgentQuery.MaxExecutionTime, MaxResultRows: cfg.AgentQuery.MaxResultRows, MaxResultBytes: cfg.AgentQuery.MaxResultBytes, MaxMemoryBytes: cfg.AgentQuery.MaxMemoryBytes})
	return c, db.Database(), cfg.AgentQuery.MaxResultRows, err
}

func (h *Handler) PostTelemetryQuery(c *gin.Context) {
	var req struct {
		SQL     string                     `json:"sql"`
		Params  map[string]json.RawMessage `json:"params"`
		MaxRows int                        `json:"max_rows"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.SQL) == "" || req.MaxRows < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid query request"})
		return
	}
	conn, _, limit, err := h.telemetryConnection(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	if req.MaxRows > 0 && req.MaxRows < limit {
		limit = req.MaxRows
	}
	params := ch.Parameters{}
	for k, v := range req.Params {
		var x any
		decoder := json.NewDecoder(strings.NewReader(string(v)))
		decoder.UseNumber()
		if decoder.Decode(&x) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parameter"})
			return
		}
		params[k] = telemetryParameter(x, false)
	}
	id := uuid.NewString()
	start := time.Now()
	result := telemetryResult{QueryID: id, Columns: []telemetryColumn{}, Rows: [][]any{}}
	var rowsRead, bytesRead atomic.Uint64
	ctx := ch.Context(c.Request.Context(), ch.WithQueryID(id), ch.WithParameters(params), ch.WithProgress(func(p *ch.Progress) { rowsRead.Add(p.Rows); bytesRead.Add(p.Bytes) }))
	rows, err := conn.Query(ctx, req.SQL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "query_id": id})
		return
	}
	defer rows.Close()
	types := rows.ColumnTypes()
	for _, col := range types {
		result.Columns = append(result.Columns, telemetryColumn{Name: col.Name(), Type: col.DatabaseTypeName()})
	}
	for rows.Next() {
		if len(result.Rows) >= limit {
			result.Truncated = true
			break
		}
		args := make([]any, len(types))
		for i, col := range types {
			args[i] = reflect.New(col.ScanType()).Interface()
		}
		if err = rows.Scan(args...); err != nil {
			break
		}
		row := make([]any, len(args))
		for i, v := range args {
			row[i] = telemetryJSON(reflect.ValueOf(v))
		}
		result.Rows = append(result.Rows, row)
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "query_id": id})
		return
	}
	result.RowsRead, result.BytesRead = rowsRead.Load(), bytesRead.Load()
	result.RowCount = len(result.Rows)
	result.ElapsedMS = time.Since(start).Milliseconds()
	if result.RowCount >= limit {
		result.Truncated = true
	}
	log.WithFields(log.Fields{"query_id": id, "elapsed_ms": result.ElapsedMS, "row_count": result.RowCount, "rows_read": result.RowsRead}).Info("agent telemetry query")
	c.JSON(http.StatusOK, result)
}

func telemetryJSON(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Type() == reflect.TypeOf(big.Int{}) {
		n := v.Interface().(big.Int)
		return n.String()
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		return v.Interface().(time.Time).Format(time.RFC3339Nano)
	}
	if v.CanInterface() {
		if s, ok := v.Interface().(fmt.Stringer); ok {
			return s.String()
		}
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := v.Int()
		if n > 1<<53-1 || n < -(1<<53-1) {
			return strconv.FormatInt(n, 10)
		}
		return n
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n := v.Uint()
		if n > 1<<53-1 {
			return strconv.FormatUint(n, 10)
		}
		return n
	case reflect.Float32, reflect.Float64:
		if math.IsNaN(v.Float()) || math.IsInf(v.Float(), 0) {
			return nil
		}
		return v.Float()
	case reflect.Slice, reflect.Array:
		a := make([]any, v.Len())
		for i := range a {
			a[i] = telemetryJSON(v.Index(i))
		}
		return a
	case reflect.Map:
		m := map[string]any{}
		it := v.MapRange()
		for it.Next() {
			m[fmt.Sprint(it.Key().Interface())] = telemetryJSON(it.Value())
		}
		return m
	}
	if v.CanInterface() {
		return v.Interface()
	}
	return nil
}

func (h *Handler) GetTelemetrySchema(c *gin.Context) {
	conn, db, _, err := h.telemetryConnection(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	ctx := ch.Context(c.Request.Context(), ch.WithParameters(ch.Parameters{"db": db}))
	rows, err := conn.Query(ctx, "SELECT t.name, t.comment, c.name, c.type, c.comment FROM system.tables t INNER JOIN system.columns c ON t.database=c.database AND t.name=c.table WHERE t.database={db:String} ORDER BY t.name, c.position")
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	type col struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Comment string `json:"comment"`
	}
	type table struct {
		Name    string `json:"name"`
		Comment string `json:"comment"`
		Columns []col  `json:"columns"`
	}
	tables := []table{}
	for rows.Next() {
		var name, comment, cn, ct, cc string
		if err = rows.Scan(&name, &comment, &cn, &ct, &cc); err != nil {
			break
		}
		if len(tables) == 0 || tables[len(tables)-1].Name != name {
			tables = append(tables, table{Name: name, Comment: comment, Columns: []col{}})
		}
		i := len(tables) - 1
		tables[i].Columns = append(tables[i].Columns, col{cn, ct, cc})
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	examples, _ := telemetryExamples.ReadFile("telemetry_query_examples.json")
	c.JSON(http.StatusOK, gin.H{"database": db, "tables": tables, "examples": json.RawMessage(examples)})
}

// telemetryParameter encodes ClickHouse Values syntax for typed native parameters.
// Strings at the top level are already unquoted; nested strings need SQL quoting.
func telemetryParameter(value any, nested bool) string {
	switch v := value.(type) {
	case nil:
		if !nested {
			return "\\N"
		}
		return "NULL"
	case string:
		if !nested {
			return v
		}
		return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'", "\n", "\\n", "\r", "\\r", "\t", "\\t").Replace(v) + "'"
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = telemetryParameter(item, true)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, telemetryParameter(key, true)+":"+telemetryParameter(v[key], true))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return ""
}
