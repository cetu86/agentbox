package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultNetworkIsOpen(t *testing.T) {
	if got := defaultConfig("/project").Network; got != "open" {
		t.Fatalf("default network = %q, want %q", got, "open")
	}
}

func TestLoadConfigDefaultsMissingNetworkToOpen(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentbox")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"project":"/project","snapshot_dir":"/snapshots"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != "open" {
		t.Fatalf("network = %q, want %q", cfg.Network, "open")
	}
}
