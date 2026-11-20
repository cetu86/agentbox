package main

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type wizardStep int

const (
	wizardChecking wizardStep = iota
	wizardCheck
	wizardConfig
	wizardWorking
	wizardImage
)

type wizardAction int

const (
	actQuit wizardAction = iota
	actRecheck
	actGitInit
	actConvert
	actContinue
	actInitDefaults
	actInitCustomize
	actBuildImage
	actSkipImage
)

type wizardOption struct {
	action wizardAction
	label  string
}

// projectFacts describes what the wizard learned about the directory.
type projectFacts struct {
	Dir        string // git root when inside a repository, otherwise the starting directory
	IsRepo     bool
	HasCommits bool
	Btrfs      bool
	Subvolume  bool
	DaemonOK   bool
	Runtime    string // "podman", "docker" or "" when neither is installed
}

// projectProbe gathers the facts; the functions are replaceable in tests.
type projectProbe struct {
	gitRoot    func(dir string) (string, error)
	hasCommits func(dir string) bool
	subvolume  func(dir string) (bool, error)
	daemon     func() error
	lookPath   func(name string) (string, error)
}

func systemProbe() projectProbe {
	return projectProbe{
		gitRoot: projectRoot,
		hasCommits: func(dir string) bool {
			_, err := gitOut(dir, "rev-parse", "--verify", "--quiet", "HEAD")
			return err == nil
		},
		subvolume: func(dir string) (bool, error) {
			id, err := btrfsRootID(dir)
			if err != nil {
				return false, err
			}
			parentID, err := btrfsRootID(filepath.Dir(dir))
			if err != nil {
				return false, err
			}
			return id != parentID, nil
		},
		daemon: func() error {
			conn, err := net.DialTimeout("unix", daemonSocketPath, 2*time.Second)
			if err != nil {
				return err
			}
			return conn.Close()
		},
		lookPath: exec.LookPath,
	}
}

func (p projectProbe) inspect(dir string) projectFacts {
	f := projectFacts{Dir: dir}
	if root, err := p.gitRoot(dir); err == nil {
		f.Dir, f.IsRepo = root, true
		f.HasCommits = p.hasCommits(root)
	}
	if sub, err := p.subvolume(f.Dir); err == nil {
		f.Btrfs, f.Subvolume = true, sub
	}
	f.DaemonOK = p.daemon() == nil
	for _, rt := range []string{"podman", "docker"} {
		if _, err := p.lookPath(rt); err == nil {
			f.Runtime = rt
			break
		}
	}
	return f
}

// wizardChoices turns the facts into explanations and the actions the user can take.
func wizardChoices(f projectFacts) (problems, notes []string, options []wizardOption) {
	recheck := wizardOption{actRecheck, "Check again"}
	quit := wizardOption{actQuit, "Quit"}
	switch {
	case !f.IsRepo:
		problems = append(problems, "This directory is not a git repository. Agents work on a branch of your project, so it needs one.")
		return problems, notes, []wizardOption{{actGitInit, "Run `git init` here"}, recheck, quit}
	case !f.HasCommits:
		problems = append(problems, "The repository has no commits yet. Make an initial commit (git add -A && git commit), then check again.")
		return problems, notes, []wizardOption{recheck, quit}
	case f.Runtime == "":
		problems = append(problems, "Neither podman nor docker was found in PATH. Install one of them, then check again.")
		return problems, notes, []wizardOption{recheck, quit}
	case !f.Btrfs:
		problems = append(problems, "The project is not on a btrfs filesystem (or the btrfs tools are missing). Snapshots need btrfs.")
		return problems, notes, []wizardOption{recheck, quit}
	}
	if !f.Subvolume {
		notes = append(notes, "The project is a regular directory; snapshots need a btrfs subvolume.")
		if !f.DaemonOK {
			problems = append(problems, "Converting it needs the privileged btrfs daemon. Start it in another terminal with `sudo agentbox daemon`, then check again.")
			return problems, notes, []wizardOption{recheck, quit}
		}
		notes = append(notes, "It will be copied into a new subvolume; the original is kept as a backup next to it.")
		return problems, notes, []wizardOption{{actConvert, "Convert to a btrfs subvolume and continue"}, quit}
	}
	if !f.DaemonOK {
		notes = append(notes, "The btrfs daemon is not running. Start it with `sudo agentbox daemon` before creating boxes.")
	}
	return problems, notes, []wizardOption{{actContinue, "Continue"}, quit}
}

type wizard struct {
	dir     string
	probe   projectProbe
	step    wizardStep
	facts   projectFacts
	convert bool
	cursor  int
	options []wizardOption
	problem []string
	notes   []string
	err     string
	cfg     *Config
	backup  string
}

func newWizard(dir string) *wizard {
	return &wizard{dir: dir, probe: systemProbe()}
}

type wizardFactsMsg struct {
	facts projectFacts
	err   error
}

type wizardInitMsg struct {
	backup string
	err    error
	log    string
}

type wizardImageMsg struct {
	cfg    *Config
	exists bool
	err    error
}

func (w *wizard) inspectCmd() tea.Cmd {
	probe, dir := w.probe, w.dir
	return func() tea.Msg { return wizardFactsMsg{facts: probe.inspect(dir)} }
}

func (w *wizard) setOptions(options []wizardOption) {
	w.options, w.cursor = options, 0
}

func (w *wizard) applyFacts(f projectFacts) {
	w.facts, w.step, w.err = f, wizardCheck, ""
	w.problem, w.notes, w.options = nil, nil, nil
	problems, notes, options := wizardChoices(f)
	w.problem, w.notes = problems, notes
	w.setOptions(options)
}

func (w *wizard) configOptions() []wizardOption {
	return []wizardOption{
		{actInitDefaults, "Initialize with the default settings"},
		{actInitCustomize, "Initialize, then customize the settings"},
		{actQuit, "Quit"},
	}
}

// runtimeOverride is the runtime to store when the default (podman) is not installed.
func (w *wizard) runtimeOverride() string {
	if w.facts.Runtime == "docker" {
		return "docker"
	}
	return ""
}

func (m tuiModel) updateWizard(msg tea.Msg) (tea.Model, tea.Cmd) {
	w := m.wizard
	switch msg := msg.(type) {
	case wizardFactsMsg:
		w.applyFacts(msg.facts)
		if msg.err != nil {
			w.err = msg.err.Error()
		}
		return m, nil
	case wizardCustomizeMsg:
		w.backup = msg.backup
		dir := w.facts.Dir
		return m, execCmd(func() error {
			cfg, err := loadConfigFor(dir)
			if err != nil {
				return err
			}
			updated, saved, err := runConfigMenu(*cfg)
			if err != nil {
				return fmt.Errorf("configure project: %w", err)
			}
			if !saved {
				return nil
			}
			_, err = saveConfig(&updated)
			return err
		}, "")
	case wizardInitMsg:
		if msg.err != nil {
			w.step = wizardConfig
			w.err = msg.err.Error()
			if tail := lastLines(msg.log, 3); tail != "" {
				w.err += " — " + tail
			}
			w.setOptions(w.configOptions())
			return m, nil
		}
		w.backup = msg.backup
		return m, w.checkImageCmd()
	case wizardImageMsg:
		return m.wizardInitialized(msg)
	case execDoneMsg:
		// Returned from the config menu or the image build.
		if msg.err != nil {
			w.err = msg.err.Error()
		}
		return m, w.checkImageCmd()
	case tea.KeyMsg:
		return m.updateWizardKey(msg)
	}
	return m, nil
}

func (m tuiModel) updateWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := m.wizard
	if w.step == wizardChecking || w.step == wizardWorking {
		return m, nil
	}
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		w.cursor = clampIndex(w.cursor-1, len(w.options))
	case "down", "j":
		w.cursor = clampIndex(w.cursor+1, len(w.options))
	case "r":
		if w.step == wizardCheck {
			w.step = wizardChecking
			return m, w.inspectCmd()
		}
	case "enter":
		if w.cursor < 0 || w.cursor >= len(w.options) {
			return m, nil
		}
		return m.runWizardAction(w.options[w.cursor].action)
	}
	return m, nil
}

func (m tuiModel) runWizardAction(action wizardAction) (tea.Model, tea.Cmd) {
	w := m.wizard
	w.err = ""
	switch action {
	case actQuit:
		return m, tea.Quit
	case actRecheck:
		w.step = wizardChecking
		return m, w.inspectCmd()
	case actGitInit:
		dir, probe := w.dir, w.probe
		w.step = wizardWorking
		return m, func() tea.Msg {
			_, err := captureCommands(func() error { return git(dir, "init") })
			return wizardFactsMsg{facts: probe.inspect(dir), err: err}
		}
	case actConvert, actContinue:
		w.convert = action == actConvert
		w.step = wizardConfig
		w.setOptions(w.configOptions())
	case actInitDefaults, actInitCustomize:
		w.step = wizardWorking
		dir, convert, customize, runtime := w.facts.Dir, w.convert, action == actInitCustomize, w.runtimeOverride()
		return m, func() tea.Msg {
			var backup string
			log, err := captureCommands(func() error {
				var err error
				_, backup, err = initProject(dir, convert)
				if err != nil {
					return err
				}
				cfg, err := loadConfigFor(dir)
				if err != nil {
					return err
				}
				if runtime != "" && cfg.Runtime != runtime {
					cfg.Runtime = runtime
					_, err = saveConfig(cfg)
				}
				return err
			})
			if err == nil && customize {
				return wizardCustomizeMsg{backup: backup}
			}
			return wizardInitMsg{backup: backup, err: err, log: log}
		}
	case actBuildImage:
		return m, buildCmd(w.cfg)
	case actSkipImage:
		return m.enterProject()
	}
	return m, nil
}

// wizardCustomizeMsg asks the model to open the config menu for the new project.
type wizardCustomizeMsg struct{ backup string }

// checkImageCmd loads the new project's config and checks whether its image exists.
func (w *wizard) checkImageCmd() tea.Cmd {
	dir := w.facts.Dir
	return func() tea.Msg {
		cfg, err := loadConfigFor(dir)
		if err != nil {
			return wizardImageMsg{err: err}
		}
		return wizardImageMsg{cfg: cfg, exists: cfg.imageExists()}
	}
}

func (m tuiModel) wizardInitialized(msg wizardImageMsg) (tea.Model, tea.Cmd) {
	w := m.wizard
	if msg.err != nil {
		w.step = wizardConfig
		w.err = msg.err.Error()
		w.setOptions(w.configOptions())
		return m, nil
	}
	w.cfg = msg.cfg
	if msg.exists {
		return m.enterProject()
	}
	w.step = wizardImage
	w.setOptions([]wizardOption{
		{actBuildImage, "Build the agent image now"},
		{actSkipImage, "Skip (run `agentbox build` later)"},
	})
	return m, nil
}

// enterProject leaves the wizard for the (empty) box list.
func (m tuiModel) enterProject() (tea.Model, tea.Cmd) {
	cfg := m.wizard.cfg
	next := newTUIModel(cfg)
	next.width, next.height = m.width, m.height
	next.status = "project initialized — press n to start your first box"
	if m.wizard.backup != "" {
		next.status += " (original files kept at " + m.wizard.backup + ")"
	}
	return next, tea.Batch(next.refreshCmd(), tickCmd())
}

func (m tuiModel) viewWizard() string {
	w := m.wizard
	title := "agentbox: set up " + shortPath(w.dirOrFacts())
	lines := []string{titleBar(title, m.width), ""}
	add := func(text string) {
		for _, l := range strings.Split(ansi.Wrap(text, max(m.width-6, 20), ""), "\n") {
			lines = append(lines, "   "+l)
		}
	}
	switch w.step {
	case wizardChecking:
		add("Checking the project…")
	case wizardWorking:
		add("Working… (copying a large project into a subvolume can take a while)")
	case wizardCheck:
		add(styled(sgrTUIBold, "This directory is not an agentbox project yet. Let's set it up."))
		lines = append(lines, "")
		for _, row := range w.factRows() {
			add(row)
		}
	case wizardConfig:
		add(styled(sgrTUIBold, "Ready to initialize"))
		lines = append(lines, "")
		for _, row := range w.summaryRows() {
			add(row)
		}
	case wizardImage:
		add(styled(sgrTUIBold, "Project initialized."))
		lines = append(lines, "")
		add("The agent image " + w.cfg.Image + " is not built yet. Building takes a few minutes and is needed before the first box.")
	}
	for _, p := range w.problem {
		lines = append(lines, "")
		add(styled(sgrTUIRed, "✗ "+p))
	}
	for _, n := range w.notes {
		lines = append(lines, "")
		add(styled(sgrTUIYellow, "! "+n))
	}
	if w.err != "" {
		lines = append(lines, "")
		add(styled(sgrTUIRed, "✗ "+w.err))
	}
	if w.step != wizardChecking && w.step != wizardWorking {
		lines = append(lines, "")
		for i, o := range w.options {
			if i == w.cursor {
				lines = append(lines, styled(sgrTUIReverse, fit("  ▸ "+o.label, m.width)))
			} else {
				lines = append(lines, "    "+o.label)
			}
		}
		lines = append(lines, "", styled(sgrTUIDim, "   ↑/↓ select   ⏎ confirm   q quit"))
	}
	return clipLines(lines, m.width)
}

func (w *wizard) dirOrFacts() string {
	if w.facts.Dir != "" {
		return w.facts.Dir
	}
	return w.dir
}

func mark(ok bool) string {
	if ok {
		return styled(sgrTUIGreen, "✓")
	}
	return styled(sgrTUIRed, "✗")
}

func (w *wizard) factRows() []string {
	f := w.facts
	rows := []string{
		fmt.Sprintf("%s git repository%s", mark(f.IsRepo), ifThen(f.IsRepo, " at "+shortPath(f.Dir), "")),
	}
	if f.IsRepo {
		rows = append(rows, fmt.Sprintf("%s has commits", mark(f.HasCommits)))
	}
	rows = append(rows,
		fmt.Sprintf("%s container runtime%s", mark(f.Runtime != ""), ifThen(f.Runtime != "", ": "+f.Runtime, "")),
		fmt.Sprintf("%s btrfs filesystem", mark(f.Btrfs)),
	)
	if f.Btrfs {
		rows = append(rows, fmt.Sprintf("%s project is a btrfs subvolume", mark(f.Subvolume)))
	}
	rows = append(rows, fmt.Sprintf("%s btrfs daemon running", mark(f.DaemonOK)))
	return rows
}

func (w *wizard) summaryRows() []string {
	f := w.facts
	defaults := defaultConfig(f.Dir)
	runtime := defaults.Runtime
	if o := w.runtimeOverride(); o != "" {
		runtime = o
	}
	rows := []string{
		"Project:    " + shortPath(f.Dir),
		"Snapshots:  " + shortPath(defaults.SnapshotDir),
		"Runtime:    " + runtime,
		"Image:      " + defaults.Image,
		"Network:    " + defaults.Network,
		"Agent:      " + strings.Join(defaults.AgentCmd, " "),
	}
	if w.convert {
		rows = append(rows, "", "The project will first be converted to a btrfs subvolume; the original stays as a backup.")
	}
	return rows
}

func ifThen(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
