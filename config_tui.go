package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
)

func cmdConfigure(args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	project := fs.String("project", "", "override project path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: agentbox configure [--project DIR]")
	}
	cfg, err := loadConfigFor(*project)
	if err != nil {
		return err
	}
	if err := requireInteractiveTerminal(os.Stdin, os.Stdout); err != nil {
		return err
	}

	updated := *cfg
	var saved bool
	if os.Getenv("ACCESSIBLE") != "" {
		saved, err = runConfigForm(&updated)
	} else {
		updated, saved, err = runConfigMenu(updated)
	}
	if err != nil {
		return fmt.Errorf("configure project: %w", err)
	}
	if !saved {
		fmt.Println("configuration unchanged")
		return nil
	}
	path, err := saveConfig(&updated)
	if err != nil {
		return err
	}
	fmt.Printf("configuration saved to %s\n", path)
	return nil
}

func runConfigForm(cfg *Config) (bool, error) {
	snapshotDir := cfg.SnapshotDir
	runtime := cfg.Runtime
	image := cfg.Image
	toolchains := cloneConfigStrings(cfg.Toolchains)
	customContainerfile := cfg.CustomContainerfile
	command := strings.Join(cfg.AgentCmd, "\n")
	shmSize := cfg.ShmSize
	mounts := strings.Join(cfg.Mounts, "\n")
	env := strings.Join(cfg.Env, "\n")
	network := cfg.Network
	allowedDomains := strings.Join(cfg.AllowedDomains, "\n")
	proxyImage := cfg.ProxyImage
	egressNetwork := cfg.EgressNetwork
	gitHubToken := cfg.GitHubToken
	save := false

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Snapshot directory").
				Validate(requiredConfigValue("snapshot directory")).
				Value(&snapshotDir),
		).Title("Project"),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Container runtime").
				Options(
					huh.NewOption("Podman", "podman"),
					huh.NewOption("Docker", "docker"),
				).
				Value(&runtime),
			huh.NewInput().
				Title("Agent image").
				Validate(requiredConfigValue("agent image")).
				Value(&image),
			huh.NewText().
				Title("Agent command arguments (one argument per line; {task} is substituted)").
				Validate(func(value string) error {
					if len(configTextLines(value)) == 0 {
						return errors.New("agent command must contain at least one argument")
					}
					return nil
				}).
				Value(&command),
			huh.NewInput().
				Title("Shared memory size").
				Validate(requiredConfigValue("shared memory size")).
				Value(&shmSize),
		).Title("Agent"),
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Language toolchains (multiple selections are combined)").
				Options(
					huh.NewOption("Java (OpenJDK 21)", "java"),
					huh.NewOption("Node.js", "nodejs"),
					huh.NewOption("Go", "go"),
					huh.NewOption("Rust", "rust"),
					huh.NewOption("C/C++ (GCC/G++, CMake, Ninja)", "cpp"),
					huh.NewOption("Custom base Containerfile", customToolchain),
				).
				Value(&toolchains),
			huh.NewInput().
				Title("Custom base Containerfile path (required when selected; relative to project)").
				Validate(func(value string) error {
					if slices.Contains(toolchains, customToolchain) && strings.TrimSpace(value) == "" {
						return errors.New("custom base Containerfile path must not be empty")
					}
					return nil
				}).
				Value(&customContainerfile),
			huh.NewText().
				Title("Extra mounts (one host:container[:opts] mount per line)").
				Value(&mounts),
			huh.NewText().
				Title("Host environment variable names to pass through (one per line)").
				Value(&env),
		).Title("Container"),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Network mode").
				Options(
					huh.NewOption("Open (normal container networking)", "open"),
					huh.NewOption("None (no container networking)", "none"),
					huh.NewOption("Proxy (domain allowlist)", "proxy"),
				).
				Value(&network),
			huh.NewText().
				Title("Allowed domains for proxy mode (one domain per line)").
				Value(&allowedDomains),
			huh.NewInput().
				Title("Proxy image").
				Validate(requiredConfigValue("proxy image")).
				Value(&proxyImage),
			huh.NewInput().
				Title("Proxy egress network (Podman: podman; Docker: bridge by default)").
				Validate(requiredConfigValue("proxy egress network")).
				Value(&egressNetwork),
		).Title("Network"),
		huh.NewGroup(
			huh.NewInput().
				Title("GitHub personal access token for Copilot").
				EchoMode(huh.EchoModePassword).
				Value(&gitHubToken),
		).Title("Credentials"),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Save these settings?").
				Affirmative("Save").
				Negative("Cancel").
				Value(&save),
		).Title("Finish"),
	).WithAccessible(os.Getenv("ACCESSIBLE") != "")

	if err := form.Run(); err != nil {
		return false, err
	}
	if !save {
		return false, nil
	}
	cfg.SnapshotDir = strings.TrimSpace(snapshotDir)
	cfg.Runtime = runtime
	cfg.Image = strings.TrimSpace(image)
	cfg.Toolchains = toolchains
	cfg.CustomContainerfile = strings.TrimSpace(customContainerfile)
	cfg.AgentCmd = configTextLines(command)
	cfg.ShmSize = strings.TrimSpace(shmSize)
	cfg.Mounts = configTextLines(mounts)
	cfg.Env = configTextLines(env)
	cfg.Network = network
	cfg.AllowedDomains = configTextLines(allowedDomains)
	cfg.ProxyImage = strings.TrimSpace(proxyImage)
	cfg.EgressNetwork = strings.TrimSpace(egressNetwork)
	cfg.GitHubToken = strings.TrimSpace(gitHubToken)

	if err := validateConfiguredValues(cfg); err != nil {
		return false, err
	}
	return true, nil
}

func validateConfiguredValues(cfg *Config) error {
	if err := validateToolchainConfig(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.SnapshotDir) == "" {
		return errors.New("snapshot directory must not be empty")
	}
	if cfg.Runtime != "podman" && cfg.Runtime != "docker" {
		return fmt.Errorf("unsupported container runtime %q", cfg.Runtime)
	}
	if strings.TrimSpace(cfg.Image) == "" {
		return errors.New("agent image must not be empty")
	}
	if len(configTextLines(strings.Join(cfg.AgentCmd, "\n"))) == 0 {
		return errors.New("agent command must contain at least one argument")
	}
	if strings.TrimSpace(cfg.ShmSize) == "" {
		return errors.New("shared memory size must not be empty")
	}
	if cfg.MaxParallel < 1 {
		return errors.New("max parallel agents must be a positive whole number")
	}
	if cfg.Network != "open" && cfg.Network != "none" && cfg.Network != "proxy" {
		return fmt.Errorf("unsupported network mode %q", cfg.Network)
	}
	if strings.TrimSpace(cfg.ProxyImage) == "" {
		return errors.New("proxy image must not be empty")
	}
	if strings.TrimSpace(cfg.EgressNetwork) == "" {
		return errors.New("proxy egress network must not be empty")
	}
	if strings.ContainsAny(cfg.GitHubToken, "\r\n\x00") {
		return errors.New("GitHub token must be a single line")
	}
	return nil
}

func configTextLines(text string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func requiredConfigValue(label string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", label)
		}
		return nil
	}
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func requireInteractiveTerminal(input, output *os.File) error {
	if !isTerminal(input) || !isTerminal(output) {
		return errors.New("configure requires an interactive terminal")
	}
	return nil
}
