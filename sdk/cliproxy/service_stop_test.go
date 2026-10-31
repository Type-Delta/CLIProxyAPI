package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestServiceShutdown_ViolentShutdownImmediatelyClosesServer(t *testing.T) {
	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if errClose := ln.Close(); errClose != nil {
		t.Fatalf("failed to close listener: %v", errClose)
	}

	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: port,
	}

	server := api.NewServer(cfg, nil, nil, "")
	go func() {
		_ = server.Start()
	}()

	// Wait briefly for server to bind.
	var conn net.Conn
	var errDial error
	for i := 0; i < 20; i++ {
		conn, errDial = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if errDial == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if errDial != nil {
		t.Fatalf("server failed to start listening: %v", errDial)
	}
	defer func() {
		if conn != nil {
			if errCloseConn := conn.Close(); errCloseConn != nil {
				t.Logf("connection close error: %v", errCloseConn)
			}
		}
	}()

	service := &Service{
		server: server,
		cfg:    cfg,
	}

	stopStart := time.Now()
	errShutdown := service.Shutdown(context.Background())
	stopDuration := time.Since(stopStart)

	if stopDuration >= time.Second {
		t.Fatalf("service.Shutdown took %v, want immediate violent shutdown (<1s)", stopDuration)
	}
	if errShutdown != nil {
		t.Fatalf("service.Shutdown error = %v, want nil", errShutdown)
	}

	// Verify server port is closed.
	conn2, errDial2 := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errDial2 == nil {
		_ = conn2.Close()
		t.Fatal("expected server to be closed, but dial succeeded")
	}
}

func TestServiceRun_ClosesListenerAtExit(t *testing.T) {
	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if errClose := ln.Close(); errClose != nil {
		t.Fatalf("failed to close listener: %v", errClose)
	}

	authDir := t.TempDir()
	cfg := &config.Config{
		Host:    "127.0.0.1",
		Port:    port,
		AuthDir: authDir,
	}

	cfgPath := filepath.Join(authDir, "config.yaml")
	if errWrite := os.WriteFile(cfgPath, []byte("{}"), 0o644); errWrite != nil {
		t.Fatalf("failed to write dummy config: %v", errWrite)
	}

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		Build()
	if errBuild != nil {
		t.Fatalf("failed to build service: %v", errBuild)
	}

	runCtx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- service.Run(runCtx)
	}()

	// Wait for server to accept connections.
	var conn net.Conn
	var errDial error
	for i := 0; i < 50; i++ {
		conn, errDial = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if errDial == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if errDial != nil {
		cancel()
		t.Fatalf("failed to dial service server: %v", errDial)
	}
	if errCloseConn := conn.Close(); errCloseConn != nil {
		t.Logf("connection close error: %v", errCloseConn)
	}

	// Trigger stop.
	stopStart := time.Now()
	cancel()

	select {
	case errRun := <-runDone:
		stopDuration := time.Since(stopStart)
		if stopDuration >= 5*time.Second {
			t.Fatalf("service.Run stop took %v, want fast shutdown", stopDuration)
		}
		if errRun != nil && !errors.Is(errRun, context.Canceled) {
			t.Fatalf("unexpected service.Run error: %v", errRun)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for service.Run to exit")
	}

	// Verify server port is closed.
	connClosed, errDialClosed := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errDialClosed == nil {
		_ = connClosed.Close()
		t.Fatal("expected server to be closed after Run exit, but dial succeeded")
	}
}

// shutdownTokenProvider pauses Run after its deferred shutdown has been installed.
type shutdownTokenProvider struct {
	load func() error
}

func (p shutdownTokenProvider) Load(context.Context, *config.Config) (*TokenClientResult, error) {
	return nil, p.load()
}

type shutdownUsageObserver struct {
	entered, release, drained chan struct{}
}

func (o shutdownUsageObserver) HandleUsage(context.Context, usage.Record) {
	close(o.entered)
	<-o.release
	close(o.drained)
}

func TestServiceRun_ShutdownContextFreshAtExit(t *testing.T) {
	authDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		observer := shutdownUsageObserver{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		unregister, err := usage.RegisterPlugin(observer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unregister(context.Background()) }()
		usage.StartDefault(context.Background())
		defer usage.StopDefault()

		priorBudget, cancelPrior := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelPrior()
		exit := make(chan struct{})
		loaded := make(chan struct{})
		stopErr := errors.New("stop before listener startup")
		service := &Service{
			cfg: &config.Config{AuthDir: authDir},
			tokenProvider: shutdownTokenProvider{load: func() error {
				usage.PublishRecord(context.Background(), usage.Record{})
				close(loaded)
				<-exit
				return stopErr
			}},
		}
		// Cancel background catalog work while retaining the independent usage drain.
		runCtx, cancelRun := context.WithCancel(context.Background())
		cancelRun()
		runDone := make(chan error, 1)
		go func() { runDone <- service.Run(runCtx) }()
		<-loaded
		<-observer.entered
		<-priorBudget.Done()
		close(exit)
		synctest.Wait()

		// A fresh 30-second deadline must still permit a drain 29 seconds after exit,
		// even though a budget created at startup has already expired.
		<-time.After(29 * time.Second)
		synctest.Wait()
		select {
		case errRun := <-runDone:
			close(observer.release)
			t.Fatalf("Run returned before usage drained after the prior budget expired: %v", errRun)
		default:
		}
		close(observer.release)
		if errRun := <-runDone; !errors.Is(errRun, stopErr) {
			t.Fatalf("Run error = %v, want %v", errRun, stopErr)
		}
		select {
		case <-observer.drained:
		default:
			t.Fatal("Run returned without draining usage")
		}
	})
}
