package synthesizer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSynthesizerSkipsRuntimeState(t *testing.T) {
	dir := t.TempDir()
	ctx := &SynthesisContext{AuthDir: dir, StrictFileParsing: true}
	for _, name := range []string{"kv.db", "kv.db.lock", "kv.db.lock.json", "kv.db-shm", "state/usage-limits.json"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"type":"codex","access_token":"state-must-not-route"}`)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if auths, err := SynthesizeAuthFile(ctx, path, body); err != nil || len(auths) != 0 {
			t.Fatalf("SynthesizeAuthFile(%q) = %v, %v; want no credentials", name, auths, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "credential.json"), []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	auths, err := NewFileSynthesizer().Synthesize(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "credential.json" {
		t.Fatalf("Synthesize() = %v, %v; want credential.json only", auths, err)
	}
}
