// Package chtest provides isolated ClickHouse databases for integration tests.
package chtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
)

const image = "clickhouse/clickhouse-server:26.8.36.3"
const container = "cpa-test-clickhouse"

func Handle(t testing.TB) *clickhouse.Handle {
	t.Helper()
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		dsn = dockerDSN(t)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "cpa_test_" + hex.EncodeToString(random[:])
	h := clickhouse.NewHandle(clickhouse.Config{DSN: dsn, Database: database})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var db *clickhouse.DB
	var err error
	for {
		db, err = h.Get(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("clickhouse test connection: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := db.Conn().Exec(ctx, "DROP DATABASE IF EXISTS `"+database+"` SYNC"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		if err := h.Close(); err != nil {
			t.Errorf("close clickhouse: %v", err)
		}
	})
	return h
}
func New(t testing.TB) *clickhouse.DB {
	t.Helper()
	h := Handle(t)
	db, err := h.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func dockerDSN(t testing.TB) string {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("ClickHouse integration test requires Docker or CPA_TEST_CLICKHOUSE_DSN: %v", err)
	}
	// Docker enforces the unique container name, so simultaneous test binaries can race safely.
	if exec.Command("docker", "inspect", container).Run() != nil {
		_ = exec.Command("docker", "run", "-d", "--name", container, "-p", "127.0.0.1::9000", "-e", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1", image).Run()
	}
	for i := 0; i < 30; i++ {
		out, err := exec.Command("docker", "port", container, "9000/tcp").Output()
		if err == nil {
			addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
			_, port, e := net.SplitHostPort(addr)
			if e == nil {
				return (&url.URL{Scheme: "clickhouse", Host: net.JoinHostPort("127.0.0.1", port)}).String()
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("could not start or inspect Docker container %s", container)
	return fmt.Sprint("")
}
