package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config contains the settings for one project.
type Config struct {
	Project             string   `json:"project"`      // btrfs subvolume holding the working directory
	SnapshotDir         string   `json:"snapshot_dir"` // must be on the same btrfs filesystem
	Runtime             string   `json:"runtime"`      // "podman" or "docker"
	Image               string   `json:"image"`
	Toolchains          []string `json:"toolchains,omitempty"`
	CustomContainerfile string   `json:"custom_containerfile,omitempty"`
	AgentCmd            []string `json:"agent_cmd"` // "{task}" is replaced with the task text
	ShmSize             string   `json:"shm_size"`  // chrome needs a big /dev/shm
	Mounts              []string `json:"mounts"`    // extra "host:container[:opts]" mounts
	Env                 []string `json:"env"`       // host env vars passed through if set
	GitHubToken         string   `json:"github_token,omitempty"`
	MaxParallel         int      `json:"max_parallel"` // plan tasks running at once; default 3

	// Network: "open" (normal runtime networking), "none", or "proxy" (allowlisting squid).
	Network        string   `json:"network"`
	AllowedDomains []string `json:"allowed_domains"`
	ProxyImage     string   `json:"proxy_image"`
	EgressNetwork  string   `json:"egress_network"` // proxy's route out; default "podman"/"bridge"
}

func configPath(project string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	project, err = cleanProjectPath(project)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(project))
	name := hex.EncodeToString(hash[:]) + ".json"
	return filepath.Join(dir, "agentbox", "projects", name), nil
}

func defaultConfig(project string) *Config {
	return &Config{
		Project:     project,
		SnapshotDir: filepath.Join(filepath.Dir(project), ".agent-snaps"),
		Runtime:     "podman",
		Image:       "agentbox:latest",
		Toolchains:  []string{},
		// Verify the flags with `copilot --help`; swap in claude, codex, etc. as you like.
		AgentCmd: []string{"copilot", "--allow-all-tools", "-i", "{task}"},
		ShmSize:  "1g",
		Env:      []string{"GH_TOKEN", "GITHUB_TOKEN"},
		Network:  "open",
		AllowedDomains: []string{
			".github.com", ".githubcopilot.com", ".githubusercontent.com",
			".npmjs.org", ".pypi.org", ".pythonhosted.org", ".golang.org",
		},
		ProxyImage:  "docker.io/ubuntu/squid:latest",
		MaxParallel: defaultMaxParallel,
	}
}

func loadConfig() (*Config, error) {
	return loadConfigFor("")
}

// errNoProject is returned when no registered project contains the directory.
var errNoProject = errors.New("no project configured")

func loadConfigFor(project string) (*Config, error) {
	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		project = cwd
		project, err = cleanProjectPath(project)
		if err != nil {
			return nil, err
		}
		for dir := project; ; dir = filepath.Dir(dir) {
			cfg, err := readConfigFile(dir)
			if err == nil {
				return cfg, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return nil, fmt.Errorf("%w for %s; run `agentbox init` there or use --project DIR", errNoProject, project)
			}
		}
	}
	return readConfigFile(project)
}

func readConfigFile(project string) (*Config, error) {
	project, err := cleanProjectPath(project)
	if err != nil {
		return nil, err
	}
	p, err := configPath(project)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; run `agentbox init` in %s: %w", p, project, os.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := decodeConfigFile(p, data)
	if err != nil {
		return nil, err
	}
	if cfg.Project != project {
		return nil, fmt.Errorf("%s config is for project %s, not %s", p, cfg.Project, project)
	}
	return cfg, nil
}

func decodeConfigFile(path string, data []byte) (*Config, error) {
	cfg := &Config{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Project == "" || cfg.SnapshotDir == "" {
		return nil, fmt.Errorf("%s: project and snapshot_dir are required", path)
	}
	project, err := cleanProjectPath(cfg.Project)
	if err != nil {
		return nil, err
	}
	cfg.Project = project
	if !filepath.IsAbs(cfg.SnapshotDir) {
		cfg.SnapshotDir = filepath.Join(cfg.Project, cfg.SnapshotDir)
	}
	applyConfigDefaults(cfg)
	return cfg, nil
}

func applyConfigDefaults(cfg *Config) {
	defaults := defaultConfig(cfg.Project)
	if cfg.Runtime == "" {
		cfg.Runtime = defaults.Runtime
	}
	if cfg.Image == "" {
		cfg.Image = defaults.Image
	}
	if cfg.Toolchains == nil {
		cfg.Toolchains = defaults.Toolchains
	}
	if cfg.AgentCmd == nil {
		cfg.AgentCmd = defaults.AgentCmd
	}
	if cfg.ShmSize == "" {
		cfg.ShmSize = defaults.ShmSize
	}
	if cfg.Env == nil {
		cfg.Env = defaults.Env
	}
	if cfg.Network == "" {
		cfg.Network = defaults.Network
	}
	if cfg.AllowedDomains == nil {
		cfg.AllowedDomains = defaults.AllowedDomains
	}
	if cfg.ProxyImage == "" {
		cfg.ProxyImage = defaults.ProxyImage
	}
	if cfg.MaxParallel == 0 {
		cfg.MaxParallel = defaults.MaxParallel
	}
	if cfg.EgressNetwork == "" {
		cfg.EgressNetwork = "bridge"
		if cfg.Runtime == "podman" {
			cfg.EgressNetwork = "podman"
		}
	}
}

func cleanProjectPath(project string) (string, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

func registerProject(project string) (string, error) {
	project, err := cleanProjectPath(project)
	if err != nil {
		return "", err
	}
	p, err := configPath(project)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	var cfg *Config
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = defaultConfig(project)
	case err != nil:
		return p, err
	default:
		cfg, err = decodeConfigFile(p, data)
		if err != nil {
			return p, err
		}
	}
	if cfg.Project != project {
		return p, fmt.Errorf("%s config is for project %s, not %s", p, cfg.Project, project)
	}
	applyConfigDefaults(cfg)
	return saveConfig(cfg)
}

func saveConfig(cfg *Config) (string, error) {
	project, err := cleanProjectPath(cfg.Project)
	if err != nil {
		return "", err
	}
	cfg.Project = project
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = defaultConfig(project).SnapshotDir
	}
	if !filepath.IsAbs(cfg.SnapshotDir) {
		cfg.SnapshotDir = filepath.Join(project, cfg.SnapshotDir)
	}
	applyConfigDefaults(cfg)
	p, err := configPath(project)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	configDir := filepath.Dir(p)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(configDir, ".config.json-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", err
	}
	return p, nil
}

func validateConfigForRegistration(project string) error {
	project, err := cleanProjectPath(project)
	if err != nil {
		return err
	}
	p, err := configPath(project)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		cfg, err := decodeConfigFile(p, data)
		if err != nil {
			return err
		}
		if cfg.Project != project {
			return fmt.Errorf("%s config is for project %s, not %s", p, cfg.Project, project)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config.json-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func expandHome(s string) string {
	if strings.HasPrefix(s, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, s[2:])
		}
	}
	return s
}
