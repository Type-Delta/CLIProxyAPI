package cliproxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// A restart must not open the listener while stored auths have no models;
// clients that list models in that window cache an empty provider catalog.
func TestServiceRun_RegistersStoredAuthModelsBeforeListening(t *testing.T) {
	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if errClose := ln.Close(); errClose != nil {
		t.Fatalf("failed to close listener: %v", errClose)
	}

	authDir := t.TempDir()
	fileName := "codex-startup-models.json"
	teamIDToken := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJ1c2VyQGV4YW1wbGUuY29tIiwgImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6IHsiY2hhdGdwdF9wbGFuX3R5cGUiOiAidGVhbSIsICJjaGF0Z3B0X2FjY291bnRfaWQiOiAiYWNjLTEyMyJ9fQ.sig"
	authJSON := fmt.Sprintf(`{"type":"codex","email":"user@example.com","access_token":"token","id_token":%q}`, teamIDToken)
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), []byte(authJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	cfgPath := filepath.Join(authDir, "config.yaml")
	if errWrite := os.WriteFile(cfgPath, []byte("{}"), 0o600); errWrite != nil {
		t.Fatalf("write dummy config: %v", errWrite)
	}

	reg := internalregistry.GetGlobalRegistry()
	reg.UnregisterClient(fileName)
	t.Cleanup(func() { reg.UnregisterClient(fileName) })

	// OnBeforeStart runs right before the listener opens and before the
	// watcher loads the auth directory.
	modelsBeforeStart := make(chan int, 1)
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{Host: "127.0.0.1", Port: port, AuthDir: authDir}).
		WithConfigPath(cfgPath).
		WithHooks(Hooks{OnBeforeStart: func(*config.Config) {
			modelsBeforeStart <- len(reg.GetModelsForClient(fileName))
		}}).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() failed: %v", errBuild)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	})

	select {
	case count := <-modelsBeforeStart:
		if count == 0 {
			t.Fatal("stored codex auth had no registered models when the listener opened")
		}
	case errRun := <-runDone:
		t.Fatalf("Run() returned before start: %v", errRun)
	case <-time.After(10 * time.Second):
		t.Fatal("service did not reach OnBeforeStart")
	}
	// Stop only after the listener is up, as the other Run tests do.
	for i := 0; i < 50; i++ {
		if conn, errDial := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); errDial == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("service did not start listening")
}
