package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	tuiRefreshInterval = 2 * time.Second

	sgrTUIReverse = "\x1b[7m"
	sgrTUIBold    = "\x1b[1m"
	sgrTUIDim     = "\x1b[2m"
	sgrTUIGreen   = "\x1b[32m"
	sgrTUIRed     = "\x1b[31m"
	sgrTUIYellow  = "\x1b[33m"
	sgrTUICyan    = "\x1b[36m"
)

func styled(code, text string) string { return code + text + sgrReset }

type tuiScreen int

const (
	screenList tuiScreen = iota
	screenNew
	screenConfirm
	screenView
	screenWizard
	screenPlan
)

type boxesMsg struct {
	boxes []boxInfo
	err   error
}

type tickMsg struct{}

// opDoneMsg reports a finished background operation. then runs on success.
type opDoneMsg struct {
	message string
	err     error
	log     string
	then    tea.Cmd
}

type viewLoadedMsg struct {
	title string
	text  string
	err   error
}

// execDoneMsg reports that a program that had the terminal has finished.
type execDoneMsg struct {
	err     error
	message string
}

type confirmDialog struct {
	title  string
	lines  []string
	danger bool
	run    func() tea.Cmd
}

type tuiModel struct {
	cfg    *Config
	width  int
	height int
	screen tuiScreen

	boxes      []boxInfo
	cursor     int
	loaded     bool
	refreshing bool

	status    string
	statusErr bool
	busy      string

	nameInput textinput.Model
	taskInput textinput.Model
	newFocus  int

	confirm confirmDialog

	viewer    viewport.Model
	viewTitle string

	wizard *wizard

	// confirmBack and viewBack are the screens to return to after a
	// confirmation or diff; they reset to the list.
	confirmBack tuiScreen
	viewBack    tuiScreen

	plan          *plan
	planSt        *planState
	planErr       string
	planLoaded    bool
	planCursor    int
	planRunning   bool
	planStepping  bool
	planNextStep  time.Time
	planLastErr   string
	planLog       []string
	planFeedback  bool
	planGoal      bool // the feedback input collects the planner's goal instead
	feedbackInput textinput.Model
}

// funcExec adapts a function to tea.ExecCommand. The function gets the real
// terminal, which bubbletea releases while it runs.
type funcExec struct{ fn func() error }

func (f funcExec) Run() error        { return f.fn() }
func (funcExec) SetStdin(io.Reader)  {}
func (funcExec) SetStdout(io.Writer) {}
func (funcExec) SetStderr(io.Writer) {}

func newTUIModel(cfg *Config) tuiModel {
	name := textinput.New()
	name.Prompt = ""
	name.CharLimit = 60
	task := textinput.New()
	task.Prompt = ""
	task.Placeholder = "optional: what should the agent do?"
	feedback := textinput.New()
	feedback.Prompt = ""
	feedback.Placeholder = "what should the agent change?"
	return tuiModel{cfg: cfg, nameInput: name, taskInput: task, feedbackInput: feedback, width: 80, height: 24}
}

// runTUI starts the project TUI, or the setup wizard when the current
// directory is not an initialized project.
func runTUI() error {
	if err := requireInteractiveTerminal(os.Stdin, os.Stdout); err != nil {
		return errors.New("the agentbox TUI requires an interactive terminal; run `agentbox help` for the commands")
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	var model tuiModel
	cfg, err := loadConfigFor("")
	switch {
	case err == nil:
		model = newTUIModel(cfg)
	case errors.Is(err, errNoProject):
		model = newTUIModel(nil)
		model.screen = screenWizard
		model.wizard = newWizard(wd)
	default:
		return err
	}
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func (m tuiModel) Init() tea.Cmd {
	if m.screen == screenWizard {
		return m.wizard.inspectCmd()
	}
	return tea.Batch(m.refreshCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m tuiModel) refreshCmd() tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		boxes, err := cfg.listBoxes()
		return boxesMsg{boxes: boxes, err: err}
	}
}

// opCmd runs fn in the background with command output captured.
func opCmd(fn func() (string, error), then tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		var message string
		log, err := captureCommands(func() error {
			var err error
			message, err = fn()
			return err
		})
		return opDoneMsg{message: message, err: err, log: log, then: then}
	}
}

func (m tuiModel) startOp(label string, fn func() (string, error), then tea.Cmd) (tuiModel, tea.Cmd) {
	m.busy = label
	m.status = ""
	return m, opCmd(fn, then)
}

func execCmd(fn func() error, message string) tea.Cmd {
	return tea.Exec(funcExec{fn}, func(err error) tea.Msg { return execDoneMsg{err: err, message: message} })
}

func (m tuiModel) attachCmd(name string, shell bool) tea.Cmd {
	cfg := m.cfg
	return execCmd(func() error { return cfg.attachSessionAt(name, shell) }, "")
}

// pauseForEnter keeps the output of a foreground program readable.
func pauseForEnter(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", err)
	}
	fmt.Fprint(os.Stderr, "\nPress Enter to return to agentbox...")
	buf := make([]byte, 1)
	for {
		n, readErr := os.Stdin.Read(buf)
		if readErr != nil || (n > 0 && (buf[0] == '\n' || buf[0] == '\r')) {
			return
		}
	}
}

// buildCmd builds the image in the foreground so its progress is visible.
func buildCmd(cfg *Config) tea.Cmd {
	return execCmd(func() error {
		err := cfg.buildImage("")
		pauseForEnter(err)
		return err
	}, "image built")
}

// configureCmd edits and saves the project configuration in the config menu.
func configureCmd(cfg *Config) tea.Cmd {
	return execCmd(func() error {
		updated, saved, err := runConfigMenu(*cfg)
		if err != nil {
			return fmt.Errorf("configure project: %w", err)
		}
		if !saved {
			return nil
		}
		if _, err = saveConfig(&updated); err != nil {
			return err
		}
		*cfg = updated // keep the running TUI in sync with the saved file
		return nil
	}, "")
}

func (m tuiModel) selected() (boxInfo, bool) {
	if m.cursor < 0 || m.cursor >= len(m.boxes) {
		return boxInfo{}, false
	}
	return m.boxes[m.cursor], true
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewer.Width, m.viewer.Height = m.width, max(m.height-2, 1)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	if m.screen == screenWizard {
		return m.updateWizard(msg)
	}

	switch msg := msg.(type) {
	case tickMsg:
		cmds := []tea.Cmd{tickCmd()}
		if (m.screen == screenList || m.screen == screenPlan) && m.busy == "" && !m.refreshing {
			m.refreshing = true
			cmds = append(cmds, m.refreshCmd())
		}
		if m.screen == screenPlan && m.busy == "" && !m.planStepping {
			cmds = append(cmds, m.planLoadCmd())
		}
		if m.planRunning && !m.planStepping && m.busy == "" && time.Now().After(m.planNextStep) {
			m.planStepping = true
			cmds = append(cmds, m.planStepCmd())
		}
		return m, tea.Batch(cmds...)
	case planLoadedMsg:
		return m.applyPlanLoaded(msg), nil
	case planStepMsg:
		return m.applyPlanStep(msg)
	case planFinishMsg:
		m.busy = ""
		if msg.err != nil {
			m.setError(msg.err, "")
			return m, nil
		}
		return m.openFinishConfirm(msg), nil
	case boxesMsg:
		return m.applyBoxes(msg), nil
	case opDoneMsg:
		m.busy = ""
		m.refreshing = true
		if msg.err != nil {
			m.setError(msg.err, msg.log)
			return m, tea.Batch(m.refreshCmd(), m.planReloadIfOpen())
		}
		m.status, m.statusErr = msg.message, false
		if msg.then != nil {
			return m, tea.Batch(msg.then, m.refreshCmd(), m.planReloadIfOpen())
		}
		return m, tea.Batch(m.refreshCmd(), m.planReloadIfOpen())
	case execDoneMsg:
		m.refreshing = true
		if msg.err != nil {
			m.setError(msg.err, "")
		} else {
			m.status, m.statusErr = msg.message, false
		}
		return m, tea.Batch(m.refreshCmd(), m.planReloadIfOpen())
	case viewLoadedMsg:
		m.busy = ""
		if msg.err != nil {
			m.setError(msg.err, "")
			return m, nil
		}
		m.screen = screenView
		m.viewTitle = msg.title
		m.viewer = viewport.New(m.width, max(m.height-2, 1))
		m.viewer.SetContent(colorizeDiff(msg.text))
		return m, nil
	case tea.KeyMsg:
		if m.busy != "" {
			return m, nil
		}
		switch m.screen {
		case screenNew:
			return m.updateNew(msg)
		case screenConfirm:
			return m.updateConfirm(msg)
		case screenView:
			return m.updateView(msg)
		case screenPlan:
			return m.updatePlan(msg)
		default:
			return m.updateList(msg)
		}
	}
	return m, nil
}

func (m *tuiModel) setError(err error, log string) {
	m.status, m.statusErr = err.Error(), true
	if tail := lastLines(log, 3); tail != "" {
		m.status += " — " + tail
	}
}

// lastLines returns the last n non-empty output lines, skipping the echoed
// "+ command" lines.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" && !strings.HasPrefix(line, "+ ") {
			kept = append([]string{line}, kept...)
		}
	}
	return strings.Join(kept, " | ")
}

func (m tuiModel) applyBoxes(msg boxesMsg) tuiModel {
	m.refreshing = false
	if msg.err != nil {
		m.setError(msg.err, "")
		return m
	}
	selected, hadSelection := m.selected()
	m.boxes, m.loaded = msg.boxes, true
	m.cursor = clampIndex(m.cursor, len(m.boxes))
	if hadSelection {
		for i, b := range m.boxes {
			if b.Name == selected.Name {
				m.cursor = i
			}
		}
	}
	return m
}

func clampIndex(i, n int) int {
	if n == 0 {
		return 0
	}
	return min(max(i, 0), n-1)
}

func (m tuiModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cursor = clampIndex(m.cursor-1, len(m.boxes))
		return m, nil
	case "down", "j":
		m.cursor = clampIndex(m.cursor+1, len(m.boxes))
		return m, nil
	case "home", "g":
		m.cursor = 0
		return m, nil
	case "end", "G":
		m.cursor = clampIndex(len(m.boxes)-1, len(m.boxes))
		return m, nil
	case "r":
		m.status, m.statusErr = "", false
		m.refreshing = true
		return m, m.refreshCmd()
	case "n":
		return m.openNew(), textinput.Blink
	case "P":
		return m.openPlan()
	case "b":
		return m, buildCmd(m.cfg)
	case "c":
		return m, configureCmd(m.cfg)
	case "enter", "s", "v", "a", "m", "p", "d", "x":
	default:
		return m, nil
	}

	box, ok := m.selected()
	if !ok {
		m.status, m.statusErr = "no box selected; press n to start one", true
		return m, nil
	}
	m.status, m.statusErr = "", false
	switch key {
	case "enter":
		if box.Running {
			return m, m.attachCmd(box.Name, false)
		}
		return m.askResume(box), nil
	case "s":
		return m, m.attachCmd(box.Name, true)
	case "v":
		m.busy = "Loading diff of " + box.Name
		m.viewBack = screenList
		cfg := m.cfg
		return m, func() tea.Msg {
			text, changed, err := cfg.boxDiff(box.Name)
			if err == nil && !changed {
				text = "No changes since the session started."
			}
			return viewLoadedMsg{title: "Changes in " + box.Name, text: text, err: err}
		}
	case "a":
		return m.askAccept(box, acceptOptions{}), nil
	case "m":
		return m.askAccept(box, acceptOptions{Merge: true}), nil
	case "p":
		return m.askAccept(box, acceptOptions{Promote: true}), nil
	case "d":
		return m.askDiscard(box), nil
	default:
		return m.askStop(box), nil
	}
}

func (m tuiModel) askResume(box boxInfo) tuiModel {
	cfg := m.cfg
	m.screen = screenConfirm
	m.confirm = confirmDialog{
		title: fmt.Sprintf("The agent of %q is not running", box.Name),
		lines: []string{"Restart the agent on this box's snapshot and attach to it?"},
		run: func() tea.Cmd {
			return opCmd(func() (string, error) {
				snap, err := cfg.requireSnapshot(box.Name)
				if err != nil {
					return "", err
				}
				return "", cfg.startContainer(box.Name, snap, cfg.agentCommand(""))
			}, m.attachCmd(box.Name, false))
		},
	}
	return m
}

func (m tuiModel) askAccept(box boxInfo, opts acceptOptions) tuiModel {
	cfg := m.cfg
	if box.Running {
		m.status, m.statusErr = fmt.Sprintf("%s is still running; exit its agent first (or stop it with x)", box.Name), true
		return m
	}
	m.screen = screenConfirm
	d := confirmDialog{}
	switch {
	case opts.Promote:
		d.title = fmt.Sprintf("Promote %q as the project?", box.Name)
		d.lines = []string{
			"Swaps the snapshot in as " + cfg.Project + ", build artifacts included.",
			"The previous project is kept as _pre-" + box.Name + " next to the snapshots.",
			"The project directory must have no uncommitted changes.",
		}
	case opts.Merge:
		d.title = fmt.Sprintf("Accept %q and merge it?", box.Name)
		d.lines = []string{
			"Commits uncommitted work in the box and fetches " + box.Branch + " into the project,",
			"then runs `git merge " + box.Branch + "` on the project's current branch.",
			"The project needs a clean worktree. A failed merge is aborted and the box is kept.",
			"The box is deleted after a successful merge.",
		}
	default:
		d.title = fmt.Sprintf("Accept %q?", box.Name)
		d.lines = []string{
			"Commits uncommitted work in the box and fetches " + box.Branch + " into the project.",
			"Merge it yourself afterwards (git merge " + box.Branch + "), or use m instead.",
			"The box is deleted.",
		}
	}
	d.run = func() tea.Cmd {
		return opCmd(func() (string, error) { return cfg.acceptBox(box.Name, opts) }, nil)
	}
	m.confirm = d
	return m
}

func (m tuiModel) askDiscard(box boxInfo) tuiModel {
	cfg := m.cfg
	m.screen = screenConfirm
	m.confirm = confirmDialog{
		title:  fmt.Sprintf("Discard %q?", box.Name),
		lines:  []string{"Stops its containers and deletes the snapshot.", "Work that was not accepted is lost."},
		danger: true,
		run: func() tea.Cmd {
			return opCmd(func() (string, error) { return "discarded " + box.Name, cfg.discardBox(box.Name) }, nil)
		},
	}
	return m
}

func (m tuiModel) askStop(box boxInfo) tuiModel {
	cfg := m.cfg
	if !box.Running {
		m.status, m.statusErr = box.Name+" is not running", true
		return m
	}
	m.screen = screenConfirm
	m.confirm = confirmDialog{
		title: fmt.Sprintf("Stop the agent of %q?", box.Name),
		lines: []string{"The container is stopped; the snapshot and its files are kept."},
		run: func() tea.Cmd {
			return opCmd(func() (string, error) {
				return "stopped " + box.Name, run(cfg.Runtime, "stop", cfg.containerName(box.Name))
			}, nil)
		},
	}
	return m
}

func (m tuiModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.screen, m.confirmBack = m.confirmBack, screenList
		m.busy = strings.TrimSuffix(strings.SplitN(m.confirm.title, "?", 2)[0], "?")
		m.status = ""
		return m, m.confirm.run()
	case "n", "N", "esc", "q":
		m.screen, m.confirmBack = m.confirmBack, screenList
	}
	return m, nil
}

func (m tuiModel) openNew() tuiModel {
	m.screen = screenNew
	m.newFocus = 0
	m.status, m.statusErr = "", false
	m.nameInput.SetValue(m.suggestName())
	m.nameInput.CursorEnd()
	m.taskInput.SetValue("")
	m.nameInput.Focus()
	m.taskInput.Blur()
	return m
}

func (m tuiModel) suggestName() string {
	taken := map[string]bool{}
	for _, b := range m.boxes {
		taken[b.Name] = true
	}
	for i := 1; ; i++ {
		if name := fmt.Sprintf("agent-%d", i); !taken[name] {
			return name
		}
	}
}

func (m *tuiModel) focusNew(field int) {
	m.newFocus = field
	if field == 0 {
		m.nameInput.Focus()
		m.taskInput.Blur()
	} else {
		m.taskInput.Focus()
		m.nameInput.Blur()
	}
}

func (m tuiModel) updateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.screen = screenList
		m.status, m.statusErr = "", false
		return m, nil
	case "tab", "shift+tab", "down", "up":
		m.focusNew(1 - m.newFocus)
		return m, textinput.Blink
	case "enter":
		if m.newFocus == 0 {
			m.focusNew(1)
			return m, textinput.Blink
		}
		return m.submitNew()
	}
	var cmd tea.Cmd
	if m.newFocus == 0 {
		m.nameInput, cmd = m.nameInput.Update(msg)
	} else {
		m.taskInput, cmd = m.taskInput.Update(msg)
	}
	return m, cmd
}

func (m tuiModel) submitNew() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.nameInput.Value())
	task := strings.TrimSpace(m.taskInput.Value())
	if err := m.validateNewName(name); err != nil {
		m.setError(err, "")
		m.focusNew(0)
		return m, nil
	}
	m.screen = screenList
	cfg := m.cfg
	return m.startOp("Starting "+name, func() (string, error) {
		snap, err := cfg.createBox(name, "", false)
		if err != nil {
			return "", err
		}
		return "", cfg.startContainer(name, snap, cfg.agentCommand(task))
	}, m.attachCmd(name, false))
}

func (m tuiModel) validateNewName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q (lowercase letters, digits, . _ -)", name)
	}
	for _, b := range m.boxes {
		if b.Name == name {
			return fmt.Errorf("a box named %q already exists", name)
		}
	}
	return nil
}

func (m tuiModel) updateView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "v", "enter":
		m.screen, m.viewBack = m.viewBack, screenList
		return m, nil
	}
	var cmd tea.Cmd
	m.viewer, cmd = m.viewer.Update(msg)
	return m, cmd
}

func colorizeDiff(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "):
			lines[i] = styled(sgrTUIBold, line)
		case strings.HasPrefix(line, "@@"):
			lines[i] = styled(sgrTUICyan, line)
		case strings.HasPrefix(line, "+"):
			lines[i] = styled(sgrTUIGreen, line)
		case strings.HasPrefix(line, "-"):
			lines[i] = styled(sgrTUIRed, line)
		}
	}
	return strings.Join(lines, "\n")
}

// fit pads or truncates text to exactly width cells.
func fit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(text) > width {
		text = ansi.Truncate(text, width, "…")
	}
	if pad := width - ansi.StringWidth(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return text
}

func titleBar(title string, width int) string {
	return styled(sgrTitleBar, fit(" "+title, width))
}

// wrapKeys lays out key hints on as many lines as needed.
func wrapKeys(items []string, width int) []string {
	var lines []string
	line := ""
	for _, item := range items {
		if line != "" && ansi.StringWidth(line)+2+ansi.StringWidth(item) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += "  "
		}
		line += item
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func keyHint(key, label string) string {
	return styled(sgrTUIBold, key) + " " + styled(sgrTUIDim, label)
}

func clipLines(lines []string, width int) string {
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) View() string {
	switch {
	case m.screen == screenWizard:
		return m.viewWizard()
	case m.screen == screenView:
		hint := styled(sgrTUIDim, " ↑/↓ PgUp/PgDn scroll   q back")
		return strings.Join([]string{titleBar(m.viewTitle, m.width), m.viewer.View(), fit(hint, m.width)}, "\n")
	case m.screen == screenNew:
		return m.viewNew()
	case m.screen == screenConfirm:
		return m.viewConfirm()
	case m.screen == screenPlan:
		return m.viewPlan()
	}
	return m.viewList()
}

func (m tuiModel) viewList() string {
	lines := []string{titleBar("agentbox: "+shortPath(m.cfg.Project), m.width), ""}
	nameW, branchW := 12, 14
	for _, b := range m.boxes {
		nameW = max(nameW, ansi.StringWidth(b.Name))
		branchW = max(branchW, ansi.StringWidth(b.Branch))
	}
	nameW, branchW = min(nameW, 30), min(branchW, 36)
	header := "   " + fit("NAME", nameW) + "  " + fit("BRANCH", branchW) + "  AGENT"
	lines = append(lines, styled(sgrTUIDim, fit(header, m.width)))

	switch {
	case !m.loaded:
		lines = append(lines, "", "   Loading…")
	case len(m.boxes) == 0:
		lines = append(lines, "", "   No boxes yet. Press "+styled(sgrTUIBold, "n")+" to start an agent on a fresh snapshot of the project.")
	}
	for i, b := range m.boxes {
		state := styled(sgrTUIDim, "none")
		switch {
		case b.Running:
			state = styled(sgrTUIGreen, "● running")
		case b.Status != "":
			state = styled(sgrTUIYellow, "○ "+strings.ToLower(b.Status))
		}
		if b.Dirty {
			state += styled(sgrTUIDim, "  · uncommitted changes")
		}
		row := fit(b.Name, nameW) + "  " + fit(b.Branch, branchW) + "  " + state
		if i == m.cursor {
			row = styled(sgrTUIReverse, fit(" ▸ "+ansi.Strip(row), m.width))
		} else {
			row = ansi.Truncate("   "+row, m.width, "…")
		}
		lines = append(lines, row)
	}

	footer := m.footer()
	body := max(m.height-len(footer), 0)
	for len(lines) < body {
		lines = append(lines, "")
	}
	return strings.Join(append(lines[:body], footer...), "\n")
}

func (m tuiModel) footer() []string {
	var status string
	switch {
	case m.busy != "":
		status = styled(sgrTUIYellow, " … "+m.busy)
	case m.statusErr:
		status = styled(sgrTUIRed, " ✗ "+m.status)
	case m.status != "":
		status = styled(sgrTUIGreen, " ✓ "+m.status)
	}
	out := []string{ansi.Truncate(status, m.width, "…")}
	keys := []string{
		keyHint("⏎", "attach"), keyHint("n", "new"), keyHint("s", "shell"), keyHint("v", "diff"),
		keyHint("a", "accept"), keyHint("m", "accept+merge"), keyHint("p", "promote"),
		keyHint("d", "discard"), keyHint("x", "stop"), keyHint("P", "plan"), keyHint("b", "build image"),
		keyHint("c", "configure"), keyHint("r", "refresh"), keyHint("q", "quit"),
	}
	for _, line := range wrapKeys(keys, max(m.width-2, 10)) {
		out = append(out, " "+line)
	}
	return out
}

func (m tuiModel) viewNew() string {
	label := func(text string, focused bool) string {
		if focused {
			return styled(sgrTUIBold, " ▸ "+text)
		}
		return "   " + text
	}
	lines := []string{
		titleBar("New box", m.width), "",
		"   Snapshots " + shortPath(m.cfg.Project) + " and starts the agent in it.", "",
		label("Name", m.newFocus == 0) + "   " + m.nameInput.View(),
		label("Task", m.newFocus == 1) + "   " + m.taskInput.View(), "",
		styled(sgrTUIDim, "   ⏎ next / start   tab switch field   esc cancel"),
	}
	if m.statusErr {
		lines = append(lines, "", styled(sgrTUIRed, "   ✗ "+m.status))
	}
	return clipLines(lines, m.width)
}

func (m tuiModel) viewConfirm() string {
	style := sgrTUIBold
	if m.confirm.danger {
		style = sgrTUIBold + sgrTUIRed
	}
	lines := []string{titleBar("agentbox: "+shortPath(m.cfg.Project), m.width), "", "   " + styled(style, m.confirm.title), ""}
	for _, l := range m.confirm.lines {
		lines = append(lines, "   "+l)
	}
	lines = append(lines, "", "   "+keyHint("y", "yes")+"   "+keyHint("n", "no"))
	return clipLines(lines, m.width)
}

// shortPath abbreviates the home directory for display.
func shortPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

// planReloadIfOpen refreshes the plan view after an operation changed it.
func (m tuiModel) planReloadIfOpen() tea.Cmd {
	if m.screen == screenPlan {
		return m.planLoadCmd()
	}
	return nil
}
