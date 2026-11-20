package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderContainerfileCombinesSelectedToolchains(t *testing.T) {
	cfg := defaultConfig("/project")
	cfg.Toolchains = []string{"go", "java", customToolchain}
	cfg.CustomContainerfile = "/project/Containerfile.custom"

	rendered, err := renderContainerfile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	content := string(rendered)
	javaIndex := strings.Index(content, "openjdk-21-jdk")
	goIndex := strings.Index(content, "golang-go")
	if javaIndex < 0 || goIndex < 0 {
		t.Fatalf("generated Containerfile lacks selected toolchains:\n%s", content)
	}
	if javaIndex >= goIndex {
		t.Fatalf("toolchain instructions are not in stable preset order:\n%s", content)
	}
	for _, notSelected := range []string{"nodejs npm", "rustc cargo", "ninja-build"} {
		if strings.Contains(content, notSelected) {
			t.Errorf("generated Containerfile includes unselected toolchain %q", notSelected)
		}
	}
}

func TestValidateToolchainConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "unknown toolchain",
			cfg:  Config{Toolchains: []string{"unknown"}},
			want: `unsupported toolchain "unknown"`,
		},
		{
			name: "custom file required",
			cfg:  Config{Toolchains: []string{customToolchain}},
			want: "custom base Containerfile path must be set",
		},
		{
			name: "duplicate toolchain",
			cfg:  Config{Toolchains: []string{"go", "go"}},
			want: `toolchain "go" is selected more than once`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateToolchainConfig(&test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestResolveCustomContainerfileRelativeToProject(t *testing.T) {
	project := t.TempDir()
	customPath := filepath.Join(project, "build", "Containerfile.custom")
	if err := os.MkdirAll(filepath.Dir(customPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPath, []byte("FROM ubuntu:24.04\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Project:             project,
		Toolchains:          []string{customToolchain},
		CustomContainerfile: filepath.Join("build", "Containerfile.custom"),
	}

	got, err := resolveCustomContainerfile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != customPath {
		t.Fatalf("custom Containerfile path = %q, want %q", got, customPath)
	}
}

func TestCmdBuildUsesCustomBaseThenSelectedToolchains(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	customPath := filepath.Join(project, "Containerfile.custom")
	if err := os.WriteFile(customPath, []byte("FROM mcr.microsoft.com/playwright:v1.49.0-noble\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(t.TempDir(), "runtime")
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf '%s\\n' CALL \"$@\" >> \"$AGENTBOX_RUNTIME_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBOX_RUNTIME_LOG", logPath)
	cfg := defaultConfig(project)
	cfg.Runtime = runtimePath
	cfg.Image = "agentbox:test"
	cfg.Toolchains = []string{"custom", "go"}
	cfg.CustomContainerfile = "Containerfile.custom"
	if _, err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if err := cmdBuild([]string{"--project", project}); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	output := string(log)
	for _, want := range []string{
		"build\n-t\nagentbox-custom-base:build-",
		"-f\n" + customPath + "\n" + project + "\n",
		"build\n-t\nagentbox:test\n",
		"--build-arg\nBASE=agentbox-custom-base:build-",
		"rmi\n-f\nagentbox-custom-base:build-",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("runtime calls do not contain %q:\n%s", want, output)
		}
	}
}

func TestPlaywrightToolchainSelectsBaseImage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	runtimePath := filepath.Join(t.TempDir(), "runtime")
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf '%s\\n' CALL \"$@\" >> \"$AGENTBOX_RUNTIME_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBOX_RUNTIME_LOG", logPath)
	cfg := defaultConfig(project)
	cfg.Runtime = runtimePath
	cfg.PlaywrightVersion = "1.57.0"
	if _, err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cmdBuild([]string{"--project", project}); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "BASE=mcr.microsoft.com/playwright:v1.57.0-noble"; !strings.Contains(string(log), want) {
		t.Errorf("runtime calls do not contain %q:\n%s", want, log)
	}

	cfg.Toolchains = []string{}
	if _, err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := cmdBuild([]string{"--project", project}); err != nil {
		t.Fatal(err)
	}
	log, _ = os.ReadFile(logPath)
	if strings.Contains(string(log), "BASE=") {
		t.Errorf("build without the playwright toolchain must keep the default base:\n%s", log)
	}
}

func TestValidatePlaywrightToolchain(t *testing.T) {
	bad := Config{Toolchains: []string{playwrightToolchain}, PlaywrightVersion: "^1.49.0"}
	if err := validateToolchainConfig(&bad); err == nil {
		t.Error("expected an error for a non-exact Playwright version")
	}
	custom := Config{Toolchains: []string{playwrightToolchain, customToolchain}, CustomContainerfile: "x", PlaywrightVersion: "1.49.0"}
	if err := validateToolchainConfig(&custom); err == nil {
		t.Error("expected an error combining playwright with a custom base")
	}
	ok := Config{Toolchains: []string{playwrightToolchain}, PlaywrightVersion: "1.49.0"}
	if err := validateToolchainConfig(&ok); err != nil {
		t.Error(err)
	}
}

func TestCmdVersionsRunsScriptInImage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	runtimePath := filepath.Join(t.TempDir(), "runtime")
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf '%s\\n' CALL \"$@\" >> \"$AGENTBOX_RUNTIME_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBOX_RUNTIME_LOG", logPath)
	cfg := defaultConfig(project)
	cfg.Runtime = runtimePath
	cfg.Image = "agentbox:test"
	if _, err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cmdVersions([]string{"--project", project}); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"run\n--rm\n--entrypoint\nsh\nagentbox:test\n-c\n", "node --version"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("runtime calls do not contain %q:\n%s", want, log)
		}
	}
}
