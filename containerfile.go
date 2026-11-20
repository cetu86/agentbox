package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed Containerfile
var containerfile []byte

//go:embed Containerfiles/toolchains/*
var toolchainContainerfiles embed.FS

const (
	customToolchain          = "custom"
	playwrightToolchain      = "playwright"
	defaultPlaywrightVersion = "1.49.0"
)

var playwrightVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[A-Za-z0-9.]+)?$`)

// playwrightBaseImage is the official image bundling the browsers for version.
func playwrightBaseImage(version string) string {
	return "mcr.microsoft.com/playwright:v" + version + "-noble"
}

type toolchainPreset struct {
	value string
	label string
	file  string
}

var toolchainPresets = []toolchainPreset{
	{value: "java", label: "Java (OpenJDK 21)", file: "Containerfiles/toolchains/java"},
	{value: "nodejs", label: "Node.js", file: "Containerfiles/toolchains/nodejs"},
	{value: "go", label: "Go", file: "Containerfiles/toolchains/go"},
	{value: "rust", label: "Rust", file: "Containerfiles/toolchains/rust"},
	{value: "cpp", label: "C/C++ (GCC/G++, CMake, Ninja)", file: "Containerfiles/toolchains/cpp"},
	{value: "vim", label: "Vim", file: "Containerfiles/toolchains/vim"},
	{value: "less", label: "less", file: "Containerfiles/toolchains/less"},
	// Playwright adds no layer: it selects the Playwright base image.
	{value: playwrightToolchain, label: "Playwright (browsers; version set separately)"},
}

func toolchainMenuOptions() []configMenuOption {
	options := make([]configMenuOption, 0, len(toolchainPresets)+1)
	for _, preset := range toolchainPresets {
		options = append(options, configMenuOption{label: preset.label, value: preset.value})
	}
	return append(options, configMenuOption{
		label: "Custom base Containerfile",
		value: customToolchain,
	})
}

func validateToolchainConfig(cfg *Config) error {
	if err := validateMountTemplates(cfg.MountTemplates); err != nil {
		return err
	}
	selected := make(map[string]bool, len(cfg.Toolchains))
	for _, value := range cfg.Toolchains {
		if selected[value] {
			return fmt.Errorf("toolchain %q is selected more than once", value)
		}
		selected[value] = true
		if value == customToolchain {
			continue
		}
		found := false
		for _, preset := range toolchainPresets {
			if value == preset.value {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unsupported toolchain %q", value)
		}
	}
	if selected[playwrightToolchain] {
		if selected[customToolchain] {
			return fmt.Errorf("the playwright toolchain cannot be combined with a custom base Containerfile; base it on a Playwright image instead")
		}
		if !playwrightVersionRe.MatchString(cfg.PlaywrightVersion) {
			return fmt.Errorf("playwright version %q must look like 1.49.0", cfg.PlaywrightVersion)
		}
	}
	if selected[customToolchain] && strings.TrimSpace(cfg.CustomContainerfile) == "" {
		return fmt.Errorf("custom base Containerfile path must be set when the custom toolchain is selected")
	}
	return nil
}

func renderContainerfile(cfg *Config) ([]byte, error) {
	if err := validateToolchainConfig(cfg); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.Write(containerfile)
	for _, preset := range toolchainPresets {
		if preset.file == "" || !containsString(cfg.Toolchains, preset.value) {
			continue
		}
		data, err := toolchainContainerfiles.ReadFile(preset.file)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s toolchain Containerfile: %w", preset.value, err)
		}
		b.WriteString("\n")
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteString("\n")
		}
	}
	return []byte(b.String()), nil
}

func resolveCustomContainerfile(cfg *Config) (string, error) {
	if err := validateToolchainConfig(cfg); err != nil {
		return "", err
	}
	if !containsString(cfg.Toolchains, customToolchain) {
		return "", fmt.Errorf("custom base Containerfile is not selected")
	}
	path := expandHome(strings.TrimSpace(cfg.CustomContainerfile))
	if !filepath.IsAbs(path) {
		path = filepath.Join(cfg.Project, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve custom base Containerfile path: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve custom base Containerfile %s: %w", path, err)
	}
	path = resolvedPath
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat custom base Containerfile %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("custom base Containerfile %s is not a regular file", path)
	}
	return path, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
