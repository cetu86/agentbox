package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

const usageText = `agentbox - throwaway agent containers on btrfs snapshots of your working dir

Usage: agentbox                            open the interactive TUI for this project
       agentbox <command> [flags] [args]   (flags go before positional args)

  init [--subvolume]         register this project; --subvolume converts it
  configure [--project DIR]  interactively configure this project
  finish                     remove this project's config and restore a normal directory
  daemon                     run the privileged btrfs companion daemon
  build                      build the agent image from the configured Containerfile
  versions                   show the tool versions resolved in the built image
  start   <name> [task...]   snapshot, branch agent/<name>, start and attach agent
  resume  <name> [task...]   restart agent on an existing snapshot and attach
  attach  <name>             reconnect to a running agent's interactive terminal
  watch   <name>             follow agent output without sending input
  shell   <name>             open Bash in the agent container or snapshot
  path    <name>             print snapshot path (e.g. goland $(agentbox path foo))
  diff    <name>             show session changes without accepting them
  list                       show snapshots, branches, dirty state, running containers
  accept  <name>             bring work back: git fetch (default), --merge or --promote
  discard <name>             stop container and delete the snapshot
  plan    <subcommand>       run a task plan with parallel agents (see: agentbox plan)
  proxy   up|down|logs       manage the allowlist proxy
  completion <bash|zsh>      print shell completion setup

Common flags: --project DIR, --net proxy|none|open
`

func main() {
	if len(os.Args) < 2 {
		if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			if err := runTUI(); err != nil {
				fmt.Fprintln(os.Stderr, "agentbox:", err)
				os.Exit(1)
			}
			return
		}
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "__authorize-fds":
		err = runAuthorizeChild(args)
	case "__complete":
		err = completionCandidates(args)
	case "daemon":
		err = cmdDaemon(args)
	case "init":
		err = cmdInit(args)
	case "configure", "config":
		err = cmdConfigure(args)
	case "finish":
		err = cmdFinish(args)
	case "build":
		err = cmdBuild(args)
	case "versions":
		err = cmdVersions(args)
	case "start":
		err = cmdStart(args, true)
	case "resume":
		err = cmdStart(args, false)
	case "shell":
		err = cmdShell(args)
	case "attach":
		err = cmdAttach(args)
	case "watch":
		err = cmdWatch(args)
	case "path":
		err = cmdPath(args)
	case "diff":
		err = cmdDiff(args)
	case "list", "ls":
		err = cmdList(args)
	case "accept":
		err = cmdAccept(args)
	case "discard", "rm":
		err = cmdDiscard(args)
	case "plan":
		err = cmdPlan(args)
	case "proxy":
		err = cmdProxy(args)
	case "completion":
		err = cmdCompletion(args)
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
	cfg, err := loadConfigFor(*project)
	if err != nil {
		return nil, nil, err
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

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	makeSubvolume := fs.Bool("subvolume", false, "copy the project into a btrfs subvolume and retain the original as a backup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: agentbox init [--subvolume]")
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, backup, err := initProject(wd, *makeSubvolume)
	if err != nil {
		return err
	}
	fmt.Printf("registered project %s in %s - run `agentbox configure` to adjust settings, then `agentbox build`\n", wd, p)
	if backup != "" {
		fmt.Printf("project is now a btrfs subvolume; original files retained at %s\n", backup)
	}
	return nil
}

func cmdFinish(args []string) error {
	cfg, rest, err := parse("finish", args, nil)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("usage: agentbox finish [--project DIR]")
	}
	entries, err := os.ReadDir(cfg.SnapshotDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	sessionEntries := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), diffMetadataPrefix) {
			sessionEntries++
		}
	}
	if sessionEntries > 0 {
		return fmt.Errorf("project has %d entries in %s; accept or discard agent snapshots before finishing", sessionEntries, cfg.SnapshotDir)
	}

	backup, err := finishProjectSubvolume(cfg.Project)
	if err != nil {
		return err
	}
	p, err := configPath(cfg.Project)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if backup != "" {
			return fmt.Errorf("project is now a regular directory and its original subvolume is retained at %s, but could not remove config %s: %w", backup, p, err)
		}
		return fmt.Errorf("project is already a regular directory, but could not remove config %s: %w", p, err)
	}
	if backup == "" {
		fmt.Printf("project is already a regular directory; removed config %s\n", p)
	} else {
		fmt.Printf("project is now a regular directory; removed config %s; original subvolume retained at %s\n", p, backup)
	}
	return nil
}

func cmdBuild(args []string) error {
	var base string
	cfg, _, err := parse("build", args, func(fs *flag.FlagSet) {
		fs.StringVar(&base, "base", "", "base image for built-in toolchains (match Playwright and host libc)")
	})
	if err != nil {
		return err
	}
	return cfg.buildImage(base)
}

// buildImage builds the agent image from the configured toolchains.
func (cfg *Config) buildImage(base string) (retErr error) {
	if err := validateToolchainConfig(cfg); err != nil {
		return err
	}
	useCustomBase := containsString(cfg.Toolchains, customToolchain)
	if useCustomBase && base != "" {
		return fmt.Errorf("--base cannot be used with a custom base Containerfile")
	}
	dir, err := os.MkdirTemp("", "agentbox-build")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	baseImage := base
	if baseImage == "" && containsString(cfg.Toolchains, playwrightToolchain) {
		baseImage = playwrightBaseImage(cfg.PlaywrightVersion)
	}
	if useCustomBase {
		customFile, err := resolveCustomContainerfile(cfg)
		if err != nil {
			return err
		}
		tempImage := "agentbox-custom-base:build-" + strings.TrimPrefix(filepath.Base(dir), "agentbox-build")
		customArgs := []string{"build", "-t", tempImage, "-f", customFile, filepath.Dir(customFile)}
		if err := run(cfg.Runtime, customArgs...); err != nil {
			return fmt.Errorf("build custom base Containerfile %s: %w", customFile, err)
		}
		defer func() {
			if err := run(cfg.Runtime, "rmi", "-f", tempImage); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("remove temporary custom base image %s: %w", tempImage, err))
			}
		}()
		baseImage = tempImage
	}

	cf := filepath.Join(dir, "Containerfile")
	data, err := renderContainerfile(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cf, data, 0o644); err != nil {
		return err
	}
	a := []string{"build", "-t", cfg.Image, "-f", cf}
	if baseImage != "" {
		a = append(a, "--build-arg", "BASE="+baseImage)
	}
	retErr = run(cfg.Runtime, append(a, dir)...)
	return retErr
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
		if snap, err = cfg.createBox(name, base, noBranch); err != nil {
			return err
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
	status, err := cfg.containerStatus(name)
	if err != nil {
		return err
	}
	if containerIsRunning(status) {
		return run(cfg.Runtime, "exec", "-it", cfg.containerName(name), "bash")
	}
	return cfg.launchShell(name, snap)
}

func cmdAttach(args []string) error {
	cfg, rest, err := parse("attach", args, nil)
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
	status, err := cfg.containerStatus(name)
	if err != nil {
		return err
	}
	if !containerIsRunning(status) {
		return fmt.Errorf("agent container for %q is not running; use `agentbox watch %s` to review output or `agentbox resume %s` to restart it", name, name, name)
	}
	return cfg.attachContainer(name)
}

func cmdWatch(args []string) error {
	cfg, rest, err := parse("watch", args, nil)
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
	status, err := cfg.containerStatus(name)
	if err != nil {
		return err
	}
	if status == "" {
		return fmt.Errorf("no agent container for %q; start or resume the agent first", name)
	}
	return run(cfg.Runtime, "logs", "-f", cfg.containerName(name))
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

func cmdDiff(args []string) error {
	cfg, rest, err := parse("diff", args, nil)
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: agentbox diff [--project DIR] NAME")
	}
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	snap, err := cfg.requireSnapshot(name)
	if err != nil {
		return err
	}
	baseline, err := sessionDiffBaseline(cfg.SnapshotDir, name)
	if err != nil {
		return err
	}
	changed, err := showSnapshotDiff(snap, baseline, os.Stdout)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("No changes since the session started.")
	}
	return nil
}

func cmdList(args []string) error {
	cfg, _, err := parse("list", args, nil)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.SnapshotDir); err != nil {
		return err
	}
	boxes, err := cfg.listBoxes()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBRANCH\tDIRTY\tCONTAINER")
	for _, b := range boxes {
		status := b.Status
		if status == "" {
			status = "none"
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", b.Name, b.Branch, b.Dirty, status)
	}
	return w.Flush()
}

func cmdAccept(args []string) error {
	var opts acceptOptions
	cfg, rest, err := parse("accept", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&opts.Promote, "promote", false, "swap the snapshot in as the project (keeps build artifacts)")
		fs.BoolVar(&opts.Force, "force", false, "with --promote: allow a dirty project dir")
		fs.BoolVar(&opts.Keep, "keep", false, "keep the snapshot afterwards")
		fs.BoolVar(&opts.Merge, "merge", false, "git merge the fetched branch into the project's current branch")
	})
	if err != nil {
		return err
	}
	name, err := needName(rest)
	if err != nil {
		return err
	}
	message, err := cfg.acceptBox(name, opts)
	if message != "" {
		fmt.Println(message)
	}
	return err
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
	return cfg.discardBox(name)
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
