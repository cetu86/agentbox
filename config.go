package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config lives at $XDG_CONFIG_HOME/agentbox/config.json (see `agentbox init`).
type Config struct {
	Project     string   `json:"project"`      // btrfs subvolume holding your working dir
	SnapshotDir string   `json:"snapshot_dir"` // must be on the same btrfs filesystem
	Runtime     string   `json:"runtime"`      // "podman" or "docker"
	Image       string   `json:"image"`
	AgentCmd    []string `json:"agent_cmd"` // "{task}" is replaced with the task text
	ShmSize     string   `json:"shm_size"`  // chrome needs a big /dev/shm
	Mounts      []string `json:"mounts"`    // extra "host:container[:opts]" mounts
	Env         []string `json:"env"`       // host env vars passed through if set

	// Network: "open" (normal runtime networking), "none", or "proxy" (allowlisting squid).
	Network        string   `json:"network"`
	AllowedDomains []string `json:"allowed_domains"`
	ProxyImage     string   `json:"proxy_image"`
	EgressNetwork  string   `json:"egress_network"` // proxy's route out; default "podman"/"bridge"
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agentbox", "config.json"), nil
}

func defaultConfig(project string) *Config {
	return &Config{
		Project:     project,
		SnapshotDir: filepath.Join(filepath.Dir(project), ".agent-snaps"),
		Runtime:     "podman",
		Image:       "agentbox:latest",
		// Verify the flags with `copilot --help`; swap in claude, codex, etc. as you like.
		AgentCmd: []string{"copilot", "--allow-all-tools", "-i", "{task}"},
		ShmSize:  "1g",
		Env:      []string{"GH_TOKEN", "GITHUB_TOKEN"},
		Network:  "open",
		AllowedDomains: []string{
			".github.com", ".githubcopilot.com", ".githubusercontent.com",
			".npmjs.org", ".pypi.org", ".pythonhosted.org", ".golang.org",
		},
		ProxyImage: "docker.io/ubuntu/squid:latest",
	}
}

func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; run `agentbox init` in your project dir", p)
	}
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.Project == "" || cfg.SnapshotDir == "" {
		return nil, fmt.Errorf("%s: project and snapshot_dir are required", p)
	}
	if cfg.Network == "" {
		cfg.Network = "open"
	}
	if cfg.EgressNetwork == "" {
		cfg.EgressNetwork = "bridge"
		if cfg.Runtime == "podman" {
			cfg.EgressNetwork = "podman"
		}
	}
	return cfg, nil
}

func writeDefaultConfig(project string) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		return p, fmt.Errorf("%s already exists", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return p, err
	}
	data, _ := json.MarshalIndent(defaultConfig(project), "", "  ")
	return p, os.WriteFile(p, append(data, '\n'), 0o644)
}

func expandHome(s string) string {
	if strings.HasPrefix(s, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, s[2:])
		}
	}
	return s
}
