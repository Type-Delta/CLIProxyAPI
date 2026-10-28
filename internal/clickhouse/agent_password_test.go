package clickhouse_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestAgentPasswordSharedHandles(t *testing.T) {
	root := chtest.Handle(t)
	db, err := root.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		b, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Fatal(e)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(b), "\n")[0])
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	user := "admin_" + db.Database()
	ctx := context.Background()
	if err = db.Conn().Exec(ctx, "CREATE USER `"+user+"` IDENTIFIED BY 'test-only-main-password'"); err != nil {
		t.Fatal(err)
	}
	defer db.Conn().Exec(ctx, "DROP USER IF EXISTS `"+user+"`")
	if err = db.Conn().Exec(ctx, "GRANT CURRENT GRANTS ON *.* TO `"+user+"` WITH GRANT OPTION"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(user, "test-only-main-password")
	q := u.Query()
	q.Del("username")
	q.Del("password")
	u.RawQuery = q.Encode()
	cfg := clickhouse.Config{DSN: u.String(), Database: db.Database()}
	first := clickhouse.NewHandle(cfg)
	defer first.Close()
	defer first.CloseAgent()
	limits := clickhouse.AgentLimits{MaxExecutionTime: 30 * time.Second, MaxResultRows: 100, MaxMemoryBytes: 1 << 30}
	c, err := first.Agent(ctx, limits)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(db.Database()))
	agentUser := "cpa_agent_" + hex.EncodeToString(digest[:8])
	var before, after string
	if err = db.Conn().QueryRow(ctx, "SELECT toString(auth_params) FROM system.users WHERE name = ?", agentUser).Scan(&before); err != nil {
		t.Fatal(err)
	}
	second := clickhouse.NewHandle(cfg)
	defer second.Close()
	defer second.CloseAgent()
	if _, err = second.Agent(ctx, limits); err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT toString(auth_params) FROM system.users WHERE name = ?", agentUser).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("second process rotated the shared account password")
	}
	if err = c.Ping(ctx); err != nil {
		t.Fatalf("second process invalidated the first password: %v", err)
	}
}
