package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestDefaultNetworkIsOpen(t *testing.T) {
	if got := defaultConfig("/project").Network; got != "open" {
		t.Fatalf("default network = %q, want %q", got, "open")
	}
}

func TestDefaultToolchainsAreEmpty(t *testing.T) {
	if got := defaultConfig("/project").Toolchains; len(got) != 0 {
		t.Fatalf("default toolchains = %#v, want none", got)
	}
}

func TestLoadConfigDefaultsMissingNetworkToOpen(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, project, `{"project":%q,"snapshot_dir":"/snapshots"}`)

	cfg, err := loadConfigFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != "open" {
		t.Fatalf("network = %q, want %q", cfg.Network, "open")
	}
}

func TestProjectConfigsAreIndependent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	projectOne := filepath.Join(t.TempDir(), "project-one")
	projectTwo := filepath.Join(t.TempDir(), "project-two")
	for _, project := range []string{projectOne, projectTwo} {
		if err := os.Mkdir(project, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectConfig(t, projectOne, `{"project":%q,"snapshot_dir":"/snapshots-one","runtime":"docker"}`)

	pathOne, err := configPath(projectOne)
	if err != nil {
		t.Fatal(err)
	}
	pathTwo, err := registerProject(projectTwo)
	if err != nil {
		t.Fatal(err)
	}
	if pathOne == pathTwo {
		t.Fatal("distinct projects share the same config path")
	}

	cfgOne, err := loadConfigFor(projectOne)
	if err != nil {
		t.Fatal(err)
	}
	cfgTwo, err := loadConfigFor(projectTwo)
	if err != nil {
		t.Fatal(err)
	}
	if cfgOne.Runtime != "docker" {
		t.Fatalf("project one runtime = %q, want docker", cfgOne.Runtime)
	}
	if cfgTwo.Runtime != "podman" {
		t.Fatalf("project two runtime = %q, want podman", cfgTwo.Runtime)
	}
	if cfgTwo.SnapshotDir != filepath.Join(filepath.Dir(projectTwo), ".agent-snaps") {
		t.Fatalf("project two snapshot dir = %q", cfgTwo.SnapshotDir)
	}
}

func TestSaveConfigPersistsInteractiveUpdates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registerProject(project); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFor(project)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime = "docker"
	cfg.Network = "none"
	cfg.Toolchains = []string{"java", "go", "custom"}
	cfg.CustomContainerfile = "Containerfile.custom"
	cfg.AgentCmd = []string{"custom-agent", "--task", "{task}"}
	cfg.GitHubToken = "ghp-test-token"
	cfg.Env = []string{}
	cfg.AllowedDomains = []string{}
	path, err := saveConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	got, err := loadConfigFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "docker" || got.Network != "none" {
		t.Fatalf("saved runtime/network = %q/%q, want docker/none", got.Runtime, got.Network)
	}
	if !reflect.DeepEqual(got.Toolchains, []string{"java", "go", "custom"}) {
		t.Fatalf("saved toolchains = %#v", got.Toolchains)
	}
	if got.CustomContainerfile != "Containerfile.custom" {
		t.Fatalf("saved custom Containerfile = %q", got.CustomContainerfile)
	}
	if len(got.AgentCmd) != 3 || got.AgentCmd[0] != "custom-agent" || got.AgentCmd[2] != "{task}" {
		t.Fatalf("saved agent command = %#v", got.AgentCmd)
	}
	if got.GitHubToken != "ghp-test-token" {
		t.Fatalf("saved GitHub token = %q, want configured token", got.GitHubToken)
	}
	if got.Env == nil || len(got.Env) != 0 {
		t.Fatalf("saved empty env list = %#v, want an empty list", got.Env)
	}
	if got.AllowedDomains == nil || len(got.AllowedDomains) != 0 {
		t.Fatalf("saved empty allowed-domains list = %#v, want an empty list", got.AllowedDomains)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory permissions = %o, want 700", dirInfo.Mode().Perm())
	}
}

func TestConfigTextLines(t *testing.T) {
	got := configTextLines(" first \r\n\nsecond\n")
	want := []string{"first", "second"}
	if len(got) != len(want) {
		t.Fatalf("configTextLines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("configTextLines = %#v, want %#v", got, want)
		}
	}
	if empty := configTextLines(""); empty == nil || len(empty) != 0 {
		t.Fatalf("empty config text lines = %#v, want a non-nil empty slice", empty)
	}
}

func TestConfigureRequiresInteractiveTerminal(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	if err := requireInteractiveTerminal(input, output); err == nil {
		t.Fatal("expected non-terminal streams to be rejected")
	}
}

func TestConcurrentProjectInitializationUsesSeparateConfigFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	projects := []string{
		filepath.Join(t.TempDir(), "project-one"),
		filepath.Join(t.TempDir(), "project-two"),
	}
	for _, project := range projects {
		if err := os.Mkdir(project, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	type result struct {
		path string
		err  error
	}
	results := make(chan result, len(projects))
	var wg sync.WaitGroup
	for _, project := range projects {
		wg.Add(1)
		go func(project string) {
			defer wg.Done()
			path, err := registerProject(project)
			results <- result{path: path, err: err}
		}(project)
	}
	wg.Wait()
	close(results)

	paths := make(map[string]bool, len(projects))
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		paths[result.path] = true
	}
	if len(paths) != len(projects) {
		t.Fatalf("concurrent projects wrote to %d config files, want %d", len(paths), len(projects))
	}
	for _, project := range projects {
		if _, err := loadConfigFor(project); err != nil {
			t.Fatalf("load config for %s: %v", project, err)
		}
	}
}

func TestProjectsRegistryConfigIsRejectedWithoutRewriting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := configPath(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	unsupported := []byte(`{"projects":{"/project":{"snapshot_dir":"/snapshots"}}}`)
	if err := os.WriteFile(path, unsupported, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := registerProject(project); err == nil {
		t.Fatal("expected projects registry to be rejected")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(unsupported) {
		t.Fatal("unsupported config was unexpectedly rewritten")
	}
}

func TestInitCreatesOneConfigPerProject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	projectOne := filepath.Join(t.TempDir(), "project-one")
	projectTwo := filepath.Join(t.TempDir(), "project-two")
	for _, project := range []string{projectOne, projectTwo} {
		if err := os.Mkdir(project, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	for _, project := range []string{projectOne, projectTwo} {
		if err := os.Chdir(project); err != nil {
			t.Fatal(err)
		}
		if err := cmdInit(nil); err != nil {
			t.Fatalf("init %s: %v", project, err)
		}
	}
	for _, project := range []string{projectOne, projectTwo} {
		cfg, err := loadConfigFor(project)
		if err != nil {
			t.Fatalf("load config for %s: %v", project, err)
		}
		if cfg.Project != project {
			t.Errorf("selected project = %q, want %q", cfg.Project, project)
		}
		path, err := configPath(project)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var persisted map[string]json.RawMessage
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatal(err)
		}
		if _, ok := persisted["projects"]; ok {
			t.Fatal("per-project config must not persist a projects registry")
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentbox", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected shared config file: %v", err)
	}
}

func TestInitRefusesMigrationBackup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	backup := filepath.Join(t.TempDir(), ".agentbox-init-test", "original")
	nested := filepath.Join(backup, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{nil, {"--subvolume"}} {
		err := cmdInit(args)
		if err == nil || !strings.Contains(err.Error(), backup) {
			t.Fatalf("cmdInit(%v) error = %v, want migration-backup refusal containing %q", args, err, backup)
		}
	}
	if _, err := initializeProjectSubvolume(nested); err == nil || !strings.Contains(err.Error(), backup) {
		t.Fatalf("initializeProjectSubvolume error = %v, want migration-backup refusal containing %q", err, backup)
	}
}

func TestLoadConfigSelectsProjectContainingCurrentDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	child := filepath.Join(project, "nested")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registerProject(project); err != nil {
		t.Fatal(err)
	}

	withWorkingDirectory(t, child)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != project {
		t.Fatalf("selected project = %q, want %q", cfg.Project, project)
	}
}

func TestLoadConfigRejectsUninitializedDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	unregistered := t.TempDir()
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registerProject(project); err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, unregistered)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error for an uninitialized working directory")
	}
	if _, err := loadConfigFor(unregistered); err == nil {
		t.Fatal("expected an error for an explicit project without a config")
	}
}

func TestFinishRefusesProjectsWithSnapshots(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath, err := registerProject(project)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.SnapshotDir, "unfinished-agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	withWorkingDirectory(t, project)
	if err := cmdFinish(nil); err == nil {
		t.Fatal("expected finish to refuse while snapshots exist")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config was removed despite snapshots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "unfinished-agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project directory unexpectedly changed: %v", err)
	}
}

func writeProjectConfig(t *testing.T, project, format string) {
	t.Helper()
	path, err := configPath(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(format, project)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withWorkingDirectory(t *testing.T, dir string) {
	t.Helper()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}
