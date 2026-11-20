package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	proxyName = "agentbox-proxy"
	intNet    = "agentbox-int" // internal network: no route out except via the proxy
	proxyPort = "3128"

	copilotBootstrapScript = `umask 077 && mkdir -p "$HOME/.copilot" && printf '%s' "$1" > "$HOME/.copilot/config.json" && shift && exec "$@"`
)

func (c *Config) containerName(name string) string { return "agentbox-" + name }

// shellContainerName names the persistent shell of a box.
func (c *Config) shellContainerName(name string) string { return c.containerName(name) + ".shell" }

func (c *Config) containerStatus(name string) (string, error) {
	return c.statusOf(c.containerName(name))
}

func (c *Config) statusOf(container string) (string, error) {
	filter := "name=^" + regexp.QuoteMeta(container) + "$"
	out, err := output(c.Runtime, "ps", "-a", "--filter", filter, "--format", "{{.Status}}")
	if err != nil {
		return "", containerStatusError(c.Runtime, err)
	}
	return out, nil
}

func containerStatusError(runtime string, err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || len(exitErr.Stderr) == 0 {
		return fmt.Errorf("inspect container state: %w", err)
	}

	detail := strings.TrimSpace(string(exitErr.Stderr))
	if filepath.Base(runtime) == "docker" && isDockerDaemonUnavailable(detail) {
		return fmt.Errorf("inspect container state: Docker daemon is not reachable; start Docker and retry (%s): %w", detail, err)
	}
	return fmt.Errorf("inspect container state: %s: %w", detail, err)
}

func isDockerDaemonUnavailable(stderr string) bool {
	stderr = strings.ToLower(stderr)
	return strings.Contains(stderr, "failed to connect to the docker api") ||
		strings.Contains(stderr, "cannot connect to the docker daemon") ||
		strings.Contains(stderr, "is the docker daemon running")
}

func containerIsRunning(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "running" || strings.HasPrefix(status, "up ")
}

// ensureShell makes sure the box's persistent shell container is running on
// its snapshot. A shell that already runs keeps its state.
func (c *Config) ensureShell(name, snap string) error {
	container := c.shellContainerName(name)
	status, err := c.statusOf(container)
	if err != nil {
		return err
	}
	if containerIsRunning(status) {
		return nil
	}
	if status != "" {
		if err := run(c.Runtime, "rm", "-f", container); err != nil {
			return err
		}
	}
	args, err := c.containerArgsNamed(name, snap, []string{"bash"}, true, container)
	if err != nil {
		return err
	}
	return c.runContainerRuntime(args...)
}

// removeShell kills and removes the box's shell container, if there is one.
func (c *Config) removeShell(name string) error {
	container := c.shellContainerName(name)
	status, err := c.statusOf(container)
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	return run(c.Runtime, "rm", "-f", container)
}

func (c *Config) removeStoppedContainer(name string) error {
	status, err := c.containerStatus(name)
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	if containerIsRunning(status) {
		return fmt.Errorf("container for %q is already running; attach to it or stop it first", name)
	}
	return run(c.Runtime, "rm", c.containerName(name))
}

// agentCommand substitutes the task into agent_cmd. With no task, the {task}
// argument (and a flag directly before it, e.g. -i) is dropped.
func (c *Config) agentCommand(task string) []string {
	var out []string
	for _, a := range c.AgentCmd {
		if !strings.Contains(a, "{task}") {
			out = append(out, a)
			continue
		}
		if task == "" {
			if n := len(out); n > 0 && strings.HasPrefix(out[n-1], "-") {
				out = out[:n-1]
			}
			continue
		}
		out = append(out, strings.ReplaceAll(a, "{task}", task))
	}
	return out
}

// startContainer starts a detached, persistent agent container.
func (c *Config) startContainer(name, snap string, cmd []string) error {
	if err := c.removeStoppedContainer(name); err != nil {
		return err
	}
	args, err := c.containerArgs(name, snap, cmd, true)
	if err != nil {
		return err
	}
	return c.runContainerRuntime(args...)
}

// launch starts a persistent agent container and attaches the current terminal.
func (c *Config) launch(name, snap string, cmd []string) error {
	if err := c.startContainer(name, snap, cmd); err != nil {
		return err
	}
	return c.attachContainer(name)
}

func (c *Config) attachContainer(name string) error {
	args := []string{"attach", c.containerName(name)}
	fmt.Fprintln(os.Stderr, "+", c.Runtime, strings.Join(args, " "))
	return c.attachSession(name)
}

func (c *Config) runAttachCommand(args []string) error {
	cmd := exec.Command(c.Runtime, args...)
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *Config) launchShell(name, snap string) error {
	args, err := c.containerArgs(name, snap, []string{"bash"}, false)
	if err != nil {
		return err
	}
	return c.runContainerRuntime(args...)
}

func (c *Config) runContainerRuntime(args ...string) error {
	if c.GitHubToken == "" {
		return run(c.Runtime, args...)
	}
	return runWithEnv(c.Runtime, map[string]string{
		"GH_TOKEN":     c.GitHubToken,
		"GITHUB_TOKEN": c.GitHubToken,
	}, args...)
}

// containerArgs configures a container with the snapshot mounted at the
// project's original absolute path (keeps absolute paths in caches valid).
func (c *Config) containerArgs(name, snap string, cmd []string, managed bool) ([]string, error) {
	return c.containerArgsNamed(name, snap, cmd, managed, c.containerName(name))
}

// containerArgsNamed is containerArgs for a managed container with an explicit
// container name.
func (c *Config) containerArgsNamed(name, snap string, cmd []string, managed bool, container string) ([]string, error) {
	args := []string{
		"run",
		"--hostname", name,
		"-v", snap + ":" + c.Project,
		"-w", c.Project,
		"--shm-size", c.ShmSize,
		"-e", "HOME=/home/agent",
		"--tmpfs", "/home/agent:exec,mode=1777",
	}
	if isCopilotCommand(cmd) {
		config, err := json.Marshal(struct {
			TrustedFolders []string `json:"trustedFolders"`
		}{TrustedFolders: []string{c.Project}})
		if err != nil {
			return nil, fmt.Errorf("configure trusted Copilot project: %w", err)
		}
		args = append(args, "--entrypoint", "/bin/sh")
		cmd = append([]string{
			"-c",
			copilotBootstrapScript,
			"agentbox-copilot-bootstrap",
			string(config),
		}, cmd...)
	}
	if managed {
		args = append(args, "-d", "-i", "-t", "--name", container)
	} else {
		args = append(args, "--rm", "-it")
	}
	if c.Runtime == "podman" {
		args = append(args, "--userns=keep-id")
	} else {
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}
	if err := c.ensureTemplateMountDirs(); err != nil {
		return nil, err
	}
	for _, m := range c.effectiveMounts() {
		args = append(args, "-v", expandHome(m))
	}
	if c.GitHubToken != "" {
		args = append(args, "-e", "GH_TOKEN", "-e", "GITHUB_TOKEN")
	}
	for _, k := range c.Env {
		if c.GitHubToken != "" && (k == "GH_TOKEN" || k == "GITHUB_TOKEN") {
			continue
		}
		if _, ok := os.LookupEnv(k); ok {
			args = append(args, "-e", k) // value is inherited, never appears in argv
		}
	}
	if n, _ := gitOut(".", "config", "user.name"); n != "" {
		e, _ := gitOut(".", "config", "user.email")
		args = append(args,
			"-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n,
			"-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}

	netArgs, err := c.networkArgs()
	if err != nil {
		return nil, err
	}
	args = append(args, netArgs...)
	args = append(args, c.Image)
	args = append(args, cmd...)
	return args, nil
}

func isCopilotCommand(cmd []string) bool {
	return len(cmd) > 0 && filepath.Base(cmd[0]) == "copilot"
}

func (c *Config) networkArgs() ([]string, error) {
	switch c.Network {
	case "open":
		return nil, nil
	case "none":
		return []string{"--network", "none"}, nil
	case "proxy":
		if err := c.proxyUp(); err != nil {
			return nil, err
		}
		ip, err := output(c.Runtime, "inspect", "-f",
			fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, intNet), proxyName)
		if err != nil || ip == "" {
			return nil, fmt.Errorf("could not determine proxy IP: %v", err)
		}
		p := fmt.Sprintf("http://%s:%s", ip, proxyPort)
		noProxy := "localhost,127.0.0.1,::1"
		return []string{
			"--network", intNet,
			"-e", "HTTP_PROXY=" + p, "-e", "HTTPS_PROXY=" + p,
			"-e", "http_proxy=" + p, "-e", "https_proxy=" + p,
			"-e", "NO_PROXY=" + noProxy, "-e", "no_proxy=" + noProxy,
		}, nil
	}
	return nil, fmt.Errorf("unknown network mode %q (proxy|none|open)", c.Network)
}

func (c *Config) proxyDir() (string, error) {
	d, err := os.UserConfigDir()
	return filepath.Join(d, "agentbox", "proxy"), err
}

// proxyUp starts the allowlisting squid sidecar if it isn't already running.
// The agent container only sits on an --internal network, so this proxy (which
// is also attached to the egress network) is its only way out.
func (c *Config) proxyUp() error {
	if out, _ := output(c.Runtime, "ps", "--filter", "name=^"+proxyName+"$", "--format", "{{.Names}}"); out != "" {
		return nil
	}
	dir, err := c.proxyDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	conf := `http_port ` + proxyPort + `
acl allowed dstdomain "/etc/squid/allowed.txt"
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access allow CONNECT SSL_ports allowed
http_access allow allowed !CONNECT
http_access deny all
cache deny all
access_log stdio:/dev/stdout
`
	if err := os.WriteFile(filepath.Join(dir, "squid.conf"), []byte(conf), 0o644); err != nil {
		return err
	}
	allowed := strings.Join(c.AllowedDomains, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "allowed.txt"), []byte(allowed), 0o644); err != nil {
		return err
	}

	if _, err := output(c.Runtime, "network", "inspect", intNet); err != nil {
		if err := run(c.Runtime, "network", "create", "--internal", intNet); err != nil {
			return err
		}
	}
	_, _ = output(c.Runtime, "rm", "-f", proxyName)
	if err := run(c.Runtime, "run", "-d", "--name", proxyName, "--network", intNet,
		"-v", filepath.Join(dir, "squid.conf")+":/etc/squid/squid.conf:ro",
		"-v", filepath.Join(dir, "allowed.txt")+":/etc/squid/allowed.txt:ro",
		c.ProxyImage); err != nil {
		return err
	}
	return run(c.Runtime, "network", "connect", c.EgressNetwork, proxyName)
}
