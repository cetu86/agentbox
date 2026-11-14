package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

//go:embed Containerfile
var containerfile []byte

const usageText = `agentbox - throwaway agent containers on btrfs snapshots of your working dir

Usage: agentbox <command> [flags] [args]   (flags go before positional args)

  init                       write default config (project = current dir)
  daemon                     run the privileged btrfs companion daemon
  build                      build the agent image from the embedded Containerfile
  start   <name> [task...]   snapshot, branch agent/<name>, run agent TUI in a container
  resume  <name> [task...]   new container on an existing snapshot
  shell   <name>             shell in the running container (or a fresh one)
  path    <name>             print snapshot path (e.g. goland $(agentbox path foo))
  list                       show snapshots, branches, dirty state, running containers
  accept  <name>             bring work back: git fetch (default) or --promote
  discard <name>             stop container and delete the snapshot
  proxy   up|down|logs       manage the allowlist proxy

Common flags: --project DIR, --net proxy|none|open
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "__authorize-fds":
		err = runAuthorizeChild(args)
	case "daemon":
		err = cmdDaemon(args)
	case "init":
		err = cmdInit()
	case "build":
		err = cmdBuild(args)
	case "start":
		err = cmdStart(args, true)
	case "resume":
		err = cmdStart(args, false)
	case "shell":
		err = cmdShell(args)
	case "path":
		err = cmdPath(args)
	case "list", "ls":
		err = cmdList(args)
	case "accept":
		err = cmdAccept(args)
	case "discard", "rm":
		err = cmdDiscard(args)
	case "proxy":
		err = cmdProxy(args)
	default:
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentbox:", err)
		os.Exit(1)
	}
}

// parse handles the common flags plus command-specific ones.
func parse(name string, args []string, extra func(*flag.FlagSet)) (*Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	project := fs.String("project", "", "override project path")
	network := fs.String("net", "", "network mode: proxy|none|open")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	if *project != "" {
		cfg.Project, _ = filepath.Abs(*project)
	}
	if *network != "" {
		cfg.Network = *network
	}
	return cfg, fs.Args(), nil
}

func needName(rest []string) (string, error) {
	if len(rest) < 1 {
		return "", fmt.Errorf("missing <name>")
	}
	return rest[0], nil
}

func cmdInit() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := writeDefaultConfig(wd)
	if err != nil {
		return err
	}
	fmt.Println("wrote", p, "- review it, then run `agentbox build`")
	return nil
}

func cmdBuild(args []string) error {
	var base string
	cfg, _, err := parse("build", args, func(fs *flag.FlagSet) {
		fs.StringVar(&base, "base", "", "base image (match your host libc and Playwright version)")
	})
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agentbox-build")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cf := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(cf, containerfile, 0o644); err != nil {
		return err
	}
	a := []string{"build", "-t", cfg.Image, "-f", cf}
	if base != "" {
		a = append(a, "--build-arg", "BASE="+base)
	}
	return run(cfg.Runtime, append(a, dir)...)
}

// cmdStart snapshots (create=true) or reuses (create=false) and launches the agent.
func cmdStart(args []string, create bool) error {
	var base string
	var noBranch bool
	cfg, rest, err := parse("start", args, func(fs *flag.FlagSet) {
		fs.StringVar(&base, "base", "", "git ref to branch from (default: current HEAD)")
		fs.BoolVar(&noBranch, "no-branch", false, "don't create agent/<name>")
	})
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	task := strings.Join(rest[1:], " ")

	var snap string
	if create {
		if snap, err = cfg.snapshot(name); err != nil {
			return err
		}
		if !noBranch {
			a := []string{"switch", "-c", "agent/" + name}
			if base != "" {
				a = append(a, base)
			}
			if err := git(snap, a...); err != nil {
				return err
			}
		}
	} else if snap, err = cfg.requireSnapshot(name); err != nil {
		return err
	}
	return cfg.launch(name, snap, cfg.agentCommand(task))
}

func cmdShell(args []string) error {
	cfg, rest, err := parse("shell", args, nil)
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	snap, err := cfg.requireSnapshot(name)
	if err != nil {
		return err
	}
	if cfg.containerRunning(name) {
		return run(cfg.Runtime, "exec", "-it", cfg.containerName(name), "bash")
	}
	return cfg.launch(name, snap, []string{"bash"})
}

func cmdPath(args []string) error {
	cfg, rest, err := parse("path", args, nil)
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	p, err := cfg.requireSnapshot(name)
	if err == nil {
		fmt.Println(p)
	}
	return err
}

func cmdList(args []string) error {
	cfg, _, err := parse("list", args, nil)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(cfg.SnapshotDir)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBRANCH\tDIRTY\tRUNNING")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := cfg.snapPath(e.Name())
		branch, _ := gitOut(p, "rev-parse", "--abbrev-ref", "HEAD")
		dirty, _ := isDirty(p)
		fmt.Fprintf(w, "%s\t%s\t%v\t%v\n", e.Name(), branch, dirty, cfg.containerRunning(e.Name()))
	}
	return w.Flush()
}

func cmdAccept(args []string) error {
	var promote, force, keep bool
	cfg, rest, err := parse("accept", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&promote, "promote", false, "swap the snapshot in as the project (keeps build artifacts)")
		fs.BoolVar(&force, "force", false, "with --promote: allow a dirty project dir")
		fs.BoolVar(&keep, "keep", false, "keep the snapshot afterwards")
	})
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	snap, err := cfg.requireSnapshot(name)
	if err != nil {
		return err
	}
	if cfg.containerRunning(name) {
		return fmt.Errorf("container for %q is still running", name)
	}

	if promote {
		backup, err := cfg.promote(name, force)
		if err != nil {
			return err
		}
		fmt.Printf("promoted; previous project kept at %s (agentbox discard _pre-%s when happy)\n", backup, name)
	} else {
		branch, err := gitOut(snap, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return err
		}
		if err := commitIfDirty(snap, "agentbox: "+name); err != nil {
			return err
		}
		if err := git(cfg.Project, "fetch", snap, branch+":"+branch); err != nil {
			return err
		}
		fmt.Printf("fetched %s into %s; merge it with: git merge %s\n", branch, cfg.Project, branch)
	}
	if keep {
		return nil
	}
	return cfg.deleteSnapshot(name)
}

func cmdDiscard(args []string) error {
	cfg, rest, err := parse("discard", args, nil)
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	if _, err := cfg.requireSnapshot(name); err != nil {
		return err
	}
	_ = run(cfg.Runtime, "rm", "-f", cfg.containerName(name)) // ok if it doesn't exist
	return cfg.deleteSnapshot(name)
}

func cmdProxy(args []string) error {
	cfg, rest, err := parse("proxy", args, nil)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: agentbox proxy up|down|logs")
	}
	switch rest[0] {
	case "up":
		_ = run(cfg.Runtime, "rm", "-f", proxyName) // restart so allowlist changes apply
		return cfg.proxyUp()
	case "down":
		return run(cfg.Runtime, "rm", "-f", proxyName)
	case "logs":
		return run(cfg.Runtime, "logs", "-f", proxyName)
	}
	return fmt.Errorf("unknown proxy command %q", rest[0])
}
