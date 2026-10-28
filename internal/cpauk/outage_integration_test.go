package cpauk

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// faultProxy simulates loss of every database socket without disrupting the shared test DB.
type faultProxy struct {
	mu       sync.Mutex
	down     bool
	conns    []net.Conn
	listener net.Listener
	target   string
}

func (p *faultProxy) outage(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}
func (p *faultProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			p.mu.Lock()
			if p.down {
				p.mu.Unlock()
				_ = client.Close()
				return
			}
			upstream, err := net.Dial("tcp", p.target)
			if err != nil {
				p.mu.Unlock()
				_ = client.Close()
				return
			}
			p.conns = append(p.conns, client, upstream)
			p.mu.Unlock()
			defer client.Close()
			defer upstream.Close()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			<-done
		}()
	}
}
func TestAnalyticsSummaryRecoversAfterDatabaseOutage(t *testing.T) {
	ctx := context.Background()
	root := chtest.Handle(t)
	db, err := root.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		out, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Fatal(e)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &faultProxy{listener: listener, target: u.Host}
	go proxy.serve()
	defer func() { _ = listener.Close(); proxy.outage(true) }()
	u.Host = listener.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: db.Database()})
	defer h.Close()
	cfg := smallConfig()
	cfg.Path = filepath.Join(t.TempDir(), "analytics.db")
	s := New(ctx, cfg, func(ctx context.Context, cfg Config) (Backend, [32]byte, error) {
		st, err := chstore.Open(ctx, h, chstore.Config{Path: cfg.Path, MaxStorageBytes: 64 << 20})
		if err != nil {
			return nil, [32]byte{}, err
		}
		if err = chstore.RunLegacyImport(ctx, st, cfg.Path); err != nil {
			return nil, [32]byte{}, err
		}
		return st, st.IdentityKeyArray(), nil
	})
	defer s.Close(ctx)
	setupCtx, cancelSetup := context.WithTimeout(ctx, 10*time.Second)
	defer cancelSetup()
	if err := waitOutageFixtureReady(setupCtx, s.(*service)); err != nil {
		t.Fatalf("outage fixture startup: %v; health=%+v", err, s.Health())
	}
	rec := validServiceRecord()
	s.Observer().HandleUsage(ctx, rec)
	waitForService(t, func() bool { return s.Health().LastSuccessfulWrite != nil })
	proxy.outage(true)
	// An independent borrower probes during the outage, as telemetry does.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = h.Get(canceled)
	rec.RequestedAt = rec.RequestedAt.Add(time.Second)
	s.Observer().HandleUsage(ctx, rec)
	waitForService(t, func() bool { return s.Health().State == StateDegraded || s.Health().State == StateCircuitOpen })
	if _, err = s.Maintenance().Start(ctx, MaintenanceRequest{Kind: "backup"}); err == nil {
		t.Fatal("backup accepted while database down")
	}
	if s.(*service).maintenanceActive {
		t.Fatal("outage backup stranded maintenance")
	}
	proxy.outage(false)
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for s.Health().State != StateReady {
		select {
		case <-ticker.C:
		case <-deadline.C:
			svc := s.(*service)
			svc.mu.RLock()
			current, active := svc.collector, svc.maintenanceActive
			svc.mu.RUnlock()
			t.Fatalf("outage recovery: health=%+v capabilities=%+v collector_ready=%v stats=%+v maintenance_active=%v", s.Health(), s.Capabilities(), current.Ready(), current.Stats(), active)
		}
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationSummary, Start: rec.RequestedAt.Add(-time.Hour), End: rec.RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	summary, err := s.Reader().Summary(ctx, query)
	if err != nil || summary.UpstreamAttempts != 2 {
		t.Fatalf("summary after recovery: attempts=%d err=%v health=%+v", summary.UpstreamAttempts, err, s.Health())
	}
	t.Logf("same CPA service recovered without restart; summary attempts=%d, dropped=%d", summary.UpstreamAttempts, s.Health().Queue.Dropped)
}
