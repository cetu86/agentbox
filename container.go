package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	proxyName = "agentbox-proxy"
	intNet    = "agentbox-int" // internal network: no route out except via the proxy
	proxyPort = "3128"
)

func (c *Config) containerName(name string) string { return "agentbox-" + name }

func (c *Config) containerRunning(name string) bool {
	out, _ := output(c.Runtime, "ps", "--filter", "name=^"+c.containerName(name)+"$", "--format", "{{.Names}}")
	return out != ""
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

// launch runs cmd in a throwaway container with the snapshot mounted at the
// project's original absolute path (keeps absolute paths in caches valid).
func (c *Config) launch(name, snap string, cmd []string) error {
	args := []string{
		"run", "--rm", "-it",
		"--name", c.containerName(name),
		"--hostname", name,
		"-v", snap + ":" + c.Project,
		"-w", c.Project,
		"--shm-size", c.ShmSize,
		"-e", "HOME=/home/agent",
		"--tmpfs", "/home/agent:exec,mode=1777",
	}
	if c.Runtime == "podman" {
		args = append(args, "--userns=keep-id")
	} else {
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}
	for _, m := range c.Mounts {
		args = append(args, "-v", expandHome(m))
	}
	for _, k := range c.Env {
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
		return err
	}
	args = append(args, netArgs...)
	args = append(args, c.Image)
	args = append(args, cmd...)
	return run(c.Runtime, args...)
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
