package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNamesTheFileItCantRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOUDEX_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"apiUrl":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "config.json")) || !strings.Contains(err.Error(), "fix it, or delete it") {
		t.Fatalf("got %v", err)
	}
}
