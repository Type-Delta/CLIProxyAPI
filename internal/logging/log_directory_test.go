package logging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestConfigureLogOutputCanonicalizesPathOutsideConfigDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	cwd, configDir, later := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{cwd, later} {
		if err := os.Mkdir(filepath.Join(dir, "logs"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("observability:\n  logs:\n    logging-to-file: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	originalOutput := log.StandardLogger().Out
	t.Cleanup(func() { closeLogOutputs(); log.SetOutput(originalOutput) })
	if err = ConfigureLogOutput(cfg); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, "logs")
	writerMu.RLock()
	filename := logWriter.Filename
	writerMu.RUnlock()
	if filename != filepath.Join(want, "main.log") {
		t.Fatalf("app logger retained a relative/config-based path: got %q want %q", filename, filepath.Join(want, "main.log"))
	}
	t.Chdir(later)
	shared, err := ConfiguredLogDirectory(cfg)
	if err != nil || shared != want {
		t.Fatalf("configured directory moved: %q %v", shared, err)
	}
	// Match SDK startup, which clones before calling the API constructor.
	runtimeCfg := cfg.CloneForRuntime()
	cloneDir, err := ConfiguredLogDirectory(runtimeCfg)
	if err != nil || cloneDir != want {
		t.Fatalf("runtime clone lost canonical directory: %q %v", cloneDir, err)
	}
	// The API's server construction fallback must not inherit another config's
	// configured process logger, even when the config values are identical.
	unrelated := &config.Config{LoggingToFile: cfg.LoggingToFile, AuthDir: cfg.AuthDir}
	independent, err := ConfiguredLogDirectory(unrelated)
	if err != nil || independent != filepath.Join(later, "logs") {
		t.Fatalf("unrelated config inherited log state: %q %v", independent, err)
	}
	// Reconfiguring a cloned runtime snapshot must preserve the target too.
	if err = ConfigureLogOutput(runtimeCfg); err != nil {
		t.Fatal(err)
	}
	writerMu.RLock()
	reloaded := logWriter.Filename
	writerMu.RUnlock()
	if reloaded != filepath.Join(want, "main.log") {
		t.Fatalf("reload moved app logger: %s", reloaded)
	}
	log.Info("canonical directory regression")
	if _, err = os.Stat(filepath.Join(want, "main.log")); err != nil {
		t.Fatalf("logger moved after cwd change: %v", err)
	}
	if _, err = os.Stat(filepath.Join(later, "logs", "main.log")); !os.IsNotExist(err) {
		t.Fatalf("logger wrote in changed cwd: %v", err)
	}
}

func TestConfiguredLogDirectoryWithoutGlobalConfiguration(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg := &config.Config{}
	got, err := ConfiguredLogDirectory(cfg)
	if err != nil || got != filepath.Join(dir, "logs") {
		t.Fatalf("unconfigured fallback: %q %v", got, err)
	}
	// Resolving a server's independent config must not configure global logging.
	writerMu.RLock()
	inherited := cfg.RuntimeLogDirectory != ""
	writerMu.RUnlock()
	if inherited {
		t.Fatal("fallback mutated global logger configuration")
	}
}
