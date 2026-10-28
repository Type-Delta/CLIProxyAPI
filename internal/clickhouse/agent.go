package clickhouse

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	log "github.com/sirupsen/logrus"
)

// AgentLimits are enforced by ClickHouse, independently of the HTTP handler.
type AgentLimits struct {
	MaxExecutionTime time.Duration
	MaxResultRows    int
	MaxResultBytes   int64
	MaxMemoryBytes   int64
}
type agentState struct {
	mu     sync.Mutex
	conn   driver.Conn
	limits AgentLimits
	closed bool
}

// Agent returns a dedicated restricted connection. It never uses the privileged
// connection for agent queries. The password exists only in process memory.
func (h *Handle) Agent(ctx context.Context, limits AgentLimits) (driver.Conn, error) {
	if limits.MaxExecutionTime <= 0 || limits.MaxResultRows <= 0 || limits.MaxMemoryBytes <= 0 {
		return nil, fmt.Errorf("agent query limits must be positive")
	}
	if limits.MaxResultBytes <= 0 {
		limits.MaxResultBytes = 64 << 20
	}
	state := &h.agent
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, fmt.Errorf("clickhouse: agent handle closed")
	}
	if state.conn != nil && state.limits == limits {
		return state.conn, nil
	}
	db, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(db.Database()))
	suffix := hex.EncodeToString(digest[:8])
	user := "cpa_agent_" + suffix
	profile := "cpa_agent_profile_" + suffix
	opts, err := ch.ParseDSN(h.cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("invalid ClickHouse DSN")
	}
	password := ""
	if opts.Auth.Password != "" {
		mac := hmac.New(sha256.New, []byte(opts.Auth.Password))
		mac.Write([]byte("cpa-agent-password-v1\x00" + db.Database()))
		password = hex.EncodeToString(mac.Sum(nil))
	} else {
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return nil, err
		}
		password = hex.EncodeToString(random[:])
		log.Warn("ClickHouse DSN has no password; agent SQL multi-process database sharing is unsupported")
	}
	seconds := int64((limits.MaxExecutionTime + time.Second - 1) / time.Second)
	// Fixed constraints prevent queries and SET statements from relaxing limits.
	settings := fmt.Sprintf("readonly = 1 CONST, max_execution_time = %d CONST, max_result_rows = %d CONST, max_result_bytes = %d CONST, result_overflow_mode = 'break' CONST, max_memory_usage = %d CONST, max_rows_to_read = 100000000 CONST, max_bytes_to_read = 10737418240 CONST", seconds, limits.MaxResultRows, limits.MaxResultBytes, limits.MaxMemoryBytes)
	statements := []string{
		"CREATE SETTINGS PROFILE IF NOT EXISTS `" + profile + "`",
		"ALTER SETTINGS PROFILE `" + profile + "` SETTINGS " + settings,
		"CREATE USER IF NOT EXISTS `" + user + "` IDENTIFIED WITH sha256_password BY '" + password + "'",
		"ALTER USER `" + user + "` IDENTIFIED WITH sha256_password BY '" + password + "' SETTINGS PROFILE `" + profile + "`",
		"ALTER USER `" + user + "` DEFAULT ROLE NONE",
		"REVOKE ALL ON *.* FROM `" + user + "`",
		"GRANT SELECT ON `" + db.Database() + "`.* TO `" + user + "`",
	}
	for _, sql := range statements {
		if err = db.Conn().Exec(ctx, sql); err != nil {
			return nil, fmt.Errorf("provision restricted ClickHouse user: %w", err)
		}
	}
	opts.Auth.Username = user
	opts.Auth.Password = password
	opts.Auth.Database = db.Database()
	conn, err := ch.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open restricted ClickHouse connection: %w", err)
	}
	if err = conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("connect restricted ClickHouse user: %w", err)
	}
	previous := state.conn
	state.conn = &agentConnection{Conn: conn}
	state.limits = limits
	if previous != nil {
		_ = previous.Close()
	}
	return state.conn, nil
}

// CloseAgent releases the restricted connection when its owning handle closes.
func (h *Handle) CloseAgent() error {
	state := &h.agent
	state.mu.Lock()
	defer state.mu.Unlock()
	state.closed = true
	if state.conn == nil {
		return nil
	}
	conn := state.conn
	state.conn = nil
	return conn.Close()
}
