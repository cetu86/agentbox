package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const planStepInterval = 5 * time.Second

// planLoadedMsg carries a read-only snapshot of the plan and its state.
type planLoadedMsg struct {
	plan  *plan
	state *planState
	err   error
}

// planStepMsg reports one orchestrator pass.
type planStepMsg struct {
	events    []string
	finished  bool
	idle      bool
	reviewing bool
	err       error
	log       string
}

// planFinishMsg carries the final-review summary before merging the plan.
type planFinishMsg struct {
	stat string
	err  error
}

func (m tuiModel) planLoadCmd() tea.Cmd {
	root := m.cfg.Project
	return func() tea.Msg {
		p, err := loadPlan(root)
		if err == nil {
			err = p.validate()
		}
		if err != nil {
			return planLoadedMsg{err: err}
		}
		st, err := loadPlanState(root)
		return planLoadedMsg{plan: p, state: st, err: err}
	}
}

func (m tuiModel) planStepCmd() tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		var res planStepMsg
		log, err := captureCommands(func() error {
			return cfg.withPlan(func(o *orchestrator) error {
				err := o.step()
				res.events = o.events
				res.finished = o.plan.isComplete(o.state)
				res.idle = o.idle()
				for _, t := range o.plan.Tasks {
					if o.state.status(t.ID) == taskReview {
						res.reviewing = true
					}
				}
				return err
			})
		})
		res.err, res.log = err, log
		return res
	}
}

func (m tuiModel) openPlan() (tea.Model, tea.Cmd) {
	m.screen = screenPlan
	m.status, m.statusErr = "", false
	m.planFeedback = false
	return m, m.planLoadCmd()
}

func (m tuiModel) applyPlanLoaded(msg planLoadedMsg) tuiModel {
	m.planLoaded = true
	if msg.err != nil {
		m.plan, m.planSt = nil, nil
		if errors.Is(msg.err, os.ErrNotExist) {
			m.planErr = ""
		} else {
			m.planErr = msg.err.Error()
		}
		return m
	}
	m.plan, m.planSt, m.planErr = msg.plan, msg.state, ""
	m.planCursor = clampIndex(m.planCursor, len(m.planOrder()))
	return m
}

func (m tuiModel) applyPlanStep(msg planStepMsg) (tuiModel, tea.Cmd) {
	m.planStepping = false
	m.planNextStep = time.Now().Add(planStepInterval)
	stamp := time.Now().Format("15:04:05")
	for _, e := range msg.events {
		m.planLog = append(m.planLog, stamp+"  "+e)
	}
	if len(m.planLog) > 50 {
		m.planLog = m.planLog[len(m.planLog)-50:]
	}
	switch {
	case msg.err != nil:
		if msg.err.Error() != m.planLastErr {
			m.setError(msg.err, msg.log)
		}
		m.planLastErr = msg.err.Error()
	case msg.finished:
		m.planRunning = false
		m.status, m.statusErr = "plan complete: press f to review and merge it", false
	case msg.idle && !msg.reviewing:
		m.planRunning = false
		m.status, m.statusErr = "nothing left to run; blocked tasks need your attention", true
	default:
		m.planLastErr = ""
	}
	return m, m.planLoadCmd()
}

// planOrder lists task IDs grouped by parallel wave, in display order.
func (m tuiModel) planOrder() []string {
	if m.plan == nil {
		return nil
	}
	var order []string
	for _, level := range m.plan.levels() {
		order = append(order, level...)
	}
	return order
}

func (m tuiModel) planSelected() (planTask, bool) {
	order := m.planOrder()
	if m.planCursor < 0 || m.planCursor >= len(order) {
		return planTask{}, false
	}
	return *m.plan.task(order[m.planCursor]), true
}

func (m tuiModel) boxByName(name string) (boxInfo, bool) {
	for _, b := range m.boxes {
		if b.Name == name {
			return b, true
		}
	}
	return boxInfo{}, false
}

func (m tuiModel) updatePlan(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.planFeedback {
		if m.planGoal {
			return m.updatePlanGoal(msg)
		}
		return m.updatePlanFeedback(msg)
	}
	key := msg.String()
	order := m.planOrder()
	switch key {
	case "q", "esc":
		m.screen = screenList
		m.status, m.statusErr = "", false
		return m, nil
	case "up", "k":
		m.planCursor = clampIndex(m.planCursor-1, len(order))
		return m, nil
	case "down", "j":
		m.planCursor = clampIndex(m.planCursor+1, len(order))
		return m, nil
	case "home", "g":
		m.planCursor = 0
		return m, nil
	case "end", "G":
		m.planCursor = clampIndex(len(order)-1, len(order))
		return m, nil
	case "r":
		return m.togglePlanRun()
	case "f":
		return m.askFinishPlan()
	}
	if m.plan == nil {
		return m.updateNoPlan(key)
	}
	t, ok := m.planSelected()
	if !ok {
		return m, nil
	}
	m.status, m.statusErr = "", false
	status := m.planSt.status(t.ID)
	switch key {
	case "u":
		return m.toggleAutoApprove(t, status)
	case "enter", "v":
		return m.planBoxAction(t, key == "v")
	case "a":
		if status != taskReview && status != taskMerging {
			return m.planNotice(t.ID + " is not waiting for review"), nil
		}
		return m.askPlanApprove(t), nil
	case "x":
		if status != taskReview {
			return m.planNotice(t.ID + " is not waiting for review"), nil
		}
		m.planFeedback = true
		m.feedbackInput.Reset()
		m.feedbackInput.Focus()
		return m, textinput.Blink
	case "d":
		if status != taskRunning && status != taskConflict {
			return m.planNotice(t.ID + " has no running agent to mark done"), nil
		}
		return m.planTaskOp("Marking "+t.ID+" done", func(o *orchestrator) error { return o.markDone(t.ID) })
	case "t":
		if status != taskBlocked && status != taskRunning && status != taskConflict {
			return m.planNotice("only blocked or stuck tasks can be retried"), nil
		}
		return m.askPlanRetry(t), nil
	}
	return m, nil
}

func (m tuiModel) planNotice(text string) tuiModel {
	m.status, m.statusErr = text, true
	return m
}

func (m tuiModel) togglePlanRun() (tea.Model, tea.Cmd) {
	if m.plan == nil {
		return m.planNotice("no valid plan to run"), nil
	}
	if m.planRunning {
		m.planRunning = false
		m.status, m.statusErr = "orchestration paused; running agents keep going", false
		return m, nil
	}
	m.planRunning = true
	m.planLastErr = ""
	m.status, m.statusErr = "", false
	if m.planStepping {
		return m, nil
	}
	m.planStepping = true
	return m, m.planStepCmd()
}

func (m tuiModel) toggleAutoApprove(t planTask, status string) (tea.Model, tea.Cmd) {
	if status == taskAccepted {
		return m.planNotice(t.ID + " is already accepted"), nil
	}
	cfg := m.cfg
	enable := !t.AutoApprove
	label := "Disabling auto-approve for " + t.ID
	msg := "auto-approve off for " + t.ID
	if enable {
		label = "Enabling auto-approve for " + t.ID
		msg = "auto-approve ON for " + t.ID + ": its work is merged into the plan branch without your review"
	}
	return m.startOp(label, func() (string, error) {
		return msg, cfg.withPlan(func(o *orchestrator) error {
			pt := o.plan.task(t.ID)
			pt.AutoApprove = enable
			return savePlan(cfg.Project, o.plan)
		})
	}, nil)
}

// planBoxAction attaches to a task's agent, or shows its diff.
func (m tuiModel) planBoxAction(t planTask, diff bool) (tea.Model, tea.Cmd) {
	box, ok := m.boxByName(t.ID)
	if !ok {
		return m.planNotice(t.ID + " has no box (not started yet, or already merged and removed)"), nil
	}
	if diff {
		cfg := m.cfg
		m.viewBack = screenPlan
		m.busy = "Loading diff of " + t.ID
		return m, func() tea.Msg {
			text, changed, err := cfg.boxDiff(t.ID)
			if err == nil && !changed {
				text = "No changes since the session started."
			}
			return viewLoadedMsg{title: "Changes in " + t.ID, text: text, err: err}
		}
	}
	if box.Running {
		return m, m.attachCmd(box.Name, false)
	}
	m.confirmBack = screenPlan
	return m.askResume(box), nil
}

// planTaskOp runs an orchestrator action for one task in the background.
func (m tuiModel) planTaskOp(label string, fn func(o *orchestrator) error) (tea.Model, tea.Cmd) {
	cfg := m.cfg
	return m.startOp(label, func() (string, error) {
		var events []string
		err := cfg.withPlan(func(o *orchestrator) error {
			err := fn(o)
			events = o.events
			return err
		})
		return strings.Join(events, "; "), err
	}, nil)
}

func (m tuiModel) askPlanApprove(t planTask) tuiModel {
	m.screen = screenConfirm
	m.confirmBack = screenPlan
	m.confirm = confirmDialog{
		title: fmt.Sprintf("Approve %q?", t.ID),
		lines: []string{
			"Stops its agent, commits its work and merges " + taskBranch(t.ID) + " into " + planBranch(m.plan.Name) + ".",
			"Tasks that depend on it can start afterwards.",
			"A merge conflict starts a resolver agent.",
		},
		run: func() tea.Cmd {
			cfg := m.cfg
			return opCmd(func() (string, error) {
				var events []string
				err := cfg.withPlan(func(o *orchestrator) error {
					err := o.approve(t.ID)
					events = o.events
					return err
				})
				return strings.Join(events, "; "), err
			}, nil)
		},
	}
	return m
}

func (m tuiModel) askPlanRetry(t planTask) tuiModel {
	m.screen = screenConfirm
	m.confirmBack = screenPlan
	m.confirm = confirmDialog{
		title:  fmt.Sprintf("Retry %q from scratch?", t.ID),
		lines:  []string{"Deletes its box and branch " + taskBranch(t.ID) + "; work in it is lost.", "The task starts again from the plan branch."},
		danger: true,
		run: func() tea.Cmd {
			cfg := m.cfg
			return opCmd(func() (string, error) {
				err := cfg.withPlan(func(o *orchestrator) error { return o.retry(t.ID) })
				return t.ID + " reset", err
			}, nil)
		},
	}
	return m
}

func (m tuiModel) askFinishPlan() (tea.Model, tea.Cmd) {
	if m.plan == nil {
		return m.planNotice("no valid plan"), nil
	}
	if !m.plan.isComplete(m.planSt) {
		return m.planNotice("every task must be accepted before the plan can be merged"), nil
	}
	cfg, name := m.cfg, m.plan.Name
	m.busy = "Summarizing " + planBranch(name)
	return m, func() tea.Msg {
		stat, err := cfg.planDiffStat(name)
		return planFinishMsg{stat: stat, err: err}
	}
}

func (m tuiModel) openFinishConfirm(msg planFinishMsg) tuiModel {
	cfg, name := m.cfg, m.plan.Name
	m.screen = screenConfirm
	m.confirmBack = screenPlan
	lines := []string{
		"Merges " + planBranch(name) + " into the project's current branch.",
		"The project needs a clean worktree; a failed merge is aborted.",
		"",
	}
	if msg.stat == "" {
		lines = append(lines, "(no changes)")
	} else {
		lines = append(lines, strings.Split(msg.stat, "\n")...)
	}
	m.confirm = confirmDialog{
		title: "Merge the finished plan?",
		lines: lines,
		run: func() tea.Cmd {
			return opCmd(func() (string, error) { return cfg.finishPlan(name) }, nil)
		},
	}
	return m
}

func (m tuiModel) updateNoPlan(key string) (tea.Model, tea.Cmd) {
	if m.planErr != "" {
		return m, nil
	}
	box, hasPlanner := m.boxByName(plannerBox)
	switch key {
	case "n":
		if hasPlanner {
			return m.planNotice("a planner already exists: ⏎ attach, c collect its plan"), nil
		}
		m.planFeedback, m.planGoal = true, true
		m.feedbackInput.Reset()
		m.feedbackInput.Focus()
		return m, textinput.Blink
	case "c":
		if !hasPlanner {
			return m.planNotice("no planner yet; press n to start one"), nil
		}
		return m.collectPlanOp()
	case "enter":
		if !hasPlanner {
			return m.planNotice("no planner yet; press n to start one"), nil
		}
		if !box.Running {
			return m.planNotice("the planner is not running; press c to collect its plan"), nil
		}
		return m, m.plannerAttachCmd()
	}
	return m, nil
}

func (m tuiModel) plannerAttachCmd() tea.Cmd {
	cfg := m.cfg
	return execCmd(func() error { return cfg.attachSessionAt(plannerBox, false) },
		"Detached from the planner. Press c to collect the plan once it reported done.")
}

func (m tuiModel) collectPlanOp() (tea.Model, tea.Cmd) {
	cfg := m.cfg
	return m.startOp("Collecting the plan", func() (string, error) {
		r, err := cfg.collectPlan()
		if err != nil {
			return "", err
		}
		switch r.Kind {
		case plannerWorking:
			return "the planner has not reported done yet", nil
		case plannerBlocked:
			return "", errors.New("the planner gave up: " + r.Message)
		case plannerInvalid:
			return "", errors.New("invalid plan, sent back to the planner (⏎ to attach): " + firstLine(r.Message))
		case plannerCollected:
			return fmt.Sprintf("plan %q saved: %d tasks; review it, then press r to run", r.Plan.Name, len(r.Plan.Tasks)), nil
		}
		return "", errors.New("no planner box")
	}, nil)
}

func (m tuiModel) updatePlanGoal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.planFeedback, m.planGoal = false, false
		return m, nil
	case "enter":
		goal := strings.TrimSpace(m.feedbackInput.Value())
		m.planFeedback, m.planGoal = false, false
		if goal == "" {
			return m, nil
		}
		cfg := m.cfg
		return m.startOp("Starting the planner", func() (string, error) {
			return "", cfg.startPlanner(goal)
		}, m.plannerAttachCmd())
	}
	var cmd tea.Cmd
	m.feedbackInput, cmd = m.feedbackInput.Update(msg)
	return m, cmd
}

func (m tuiModel) updatePlanFeedback(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.planFeedback = false
		return m, nil
	case "enter":
		t, ok := m.planSelected()
		m.planFeedback = false
		if !ok {
			return m, nil
		}
		feedback := strings.TrimSpace(m.feedbackInput.Value())
		label := "Sending " + t.ID + " back"
		if feedback == "" {
			label = "Rejecting " + t.ID
		}
		return m.planTaskOp(label, func(o *orchestrator) error { return o.reject(t.ID, feedback) })
	}
	var cmd tea.Cmd
	m.feedbackInput, cmd = m.feedbackInput.Update(msg)
	return m, cmd
}

func planStatusStyle(status string) string {
	switch status {
	case taskRunning:
		return sgrTUIGreen
	case taskReview, taskMerging, taskConflict:
		return sgrTUIBold + sgrTUIYellow
	case taskBlocked:
		return sgrTUIRed
	case "ready":
		return sgrTUICyan
	case taskAccepted, "waiting", "unreachable":
		return sgrTUIDim
	}
	return ""
}

func (m tuiModel) viewPlan() string {
	title := "plan: " + shortPath(m.cfg.Project)
	if m.plan != nil {
		title = fmt.Sprintf("plan %s → %s", m.plan.Name, planBranch(m.plan.Name))
	}
	lines := []string{titleBar(title, m.width)}

	switch {
	case !m.planLoaded:
		lines = append(lines, "", "   Loading…")
	case m.planErr != "":
		lines = append(lines, "", styled(sgrTUIRed, "   ✗ "+m.planErr), "", "   Fix .agentbox/plan.json and reopen this screen.")
	case m.plan == nil:
		lines = append(lines, "", "   No plan found at .agentbox/plan.json.", "")
		if box, ok := m.boxByName(plannerBox); ok {
			state := "exited"
			if box.Running {
				state = "running"
			}
			lines = append(lines, "   Planner agent: "+state+" (⏎ attach, c collect the plan once it reported done)")
		} else {
			lines = append(lines, "   Press n to describe a goal; a planner agent turns it into a task plan.")
		}
	default:
		lines = append(lines, m.planSummaryLine(), "")
	}
	head := len(lines)

	footer := m.planFooter()
	var detail []string
	var rows []string
	cursorRow := 0
	if m.plan != nil {
		detail = m.planDetail()
		rows, cursorRow = m.planRows()
	}
	logLines := m.planLog
	if len(logLines) > 3 {
		logLines = logLines[len(logLines)-3:]
	}
	room := max(m.height-head-len(footer)-len(detail)-len(logLines), 3)
	start := max(min(cursorRow-room/2, len(rows)-room), 0)
	end := min(start+room, len(rows))
	lines = append(lines, rows[start:end]...)
	for len(lines) < head+min(room, len(rows)) {
		lines = append(lines, "")
	}
	lines = append(lines, detail...)
	for _, l := range logLines {
		lines = append(lines, styled(sgrTUIDim, "   "+l))
	}
	body := max(m.height-len(footer), 0)
	for len(lines) < body {
		lines = append(lines, "")
	}
	lines = lines[:min(len(lines), body)]
	return clipLines(append(lines, footer...), m.width)
}

func (m tuiModel) planSummaryLine() string {
	run := styled(sgrTUIDim, "paused")
	switch {
	case m.planRunning && m.planStepping:
		run = styled(sgrTUIGreen, "orchestrating…")
	case m.planRunning:
		run = styled(sgrTUIGreen, "orchestrating")
	}
	counts := m.plan.planSummary(m.planSt)
	var parts []string
	for _, st := range []string{taskRunning, taskReview, taskMerging, taskConflict, taskBlocked, "ready", "waiting", "unreachable", taskAccepted} {
		if n := counts[st]; n > 0 {
			parts = append(parts, styled(planStatusStyle(st), fmt.Sprintf("%d %s", n, st)))
		}
	}
	return "   " + run + "   " + strings.Join(parts, styled(sgrTUIDim, " · "))
}

// planRows renders the tasks grouped by parallel wave and returns the row
// that holds the cursor.
func (m tuiModel) planRows() ([]string, int) {
	var rows []string
	cursorRow, idx := 0, 0
	idW := 8
	for _, t := range m.plan.Tasks {
		idW = max(idW, ansi.StringWidth(t.ID))
	}
	idW = min(idW, 24)
	for i, level := range m.plan.levels() {
		note := "parallel"
		if len(level) == 1 {
			note = "single"
		}
		rows = append(rows, styled(sgrTUIDim, fmt.Sprintf("   wave %d (%s)", i+1, note)))
		for _, id := range level {
			t := m.plan.task(id)
			status := m.plan.displayStatus(m.planSt, id)
			agent := "  "
			if b, ok := m.boxByName(id); ok && b.Running {
				agent = styled(sgrTUIGreen, "● ")
			}
			auto := "      "
			if t.AutoApprove {
				auto = styled(sgrTUIYellow, "⚡auto")
			}
			after := ""
			if len(t.DependsOn) > 0 {
				after = "after " + strings.Join(t.DependsOn, ",")
			}
			row := agent + fit(id, idW) + "  " + styled(planStatusStyle(status), fit(status, 11)) + " " + auto + " " + styled(sgrTUIDim, after)
			if idx == m.planCursor {
				cursorRow = len(rows)
				row = styled(sgrTUIReverse, fit(" ▸ "+ansi.Strip(row), m.width))
			} else {
				row = "   " + row
			}
			rows = append(rows, row)
			idx++
		}
	}
	return rows, cursorRow
}

func (m tuiModel) planDetail() []string {
	t, ok := m.planSelected()
	if !ok {
		return nil
	}
	width := max(m.width-6, 20)
	out := []string{"", styled(sgrTUIBold, "   "+orDefault(t.Title, t.ID))}
	prompt := strings.Split(strings.TrimSpace(ansi.Wrap(t.Prompt, width, "")), "\n")
	if len(prompt) > 3 {
		prompt = append(prompt[:2], prompt[2]+" …")
	}
	for _, l := range prompt {
		out = append(out, styled(sgrTUIDim, "   "+l))
	}
	if ts := m.planSt.Tasks[t.ID]; ts != nil && ts.Note != "" {
		style := ""
		if ts.Status == taskBlocked {
			style = sgrTUIRed
		}
		out = append(out, styled(style, "   → "+firstLine(ts.Note)))
	}
	return out
}

func (m tuiModel) planFooter() []string {
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
	if m.planFeedback && m.planGoal {
		return append(out, " Goal for the planner: "+m.feedbackInput.View(),
			styled(sgrTUIDim, " ⏎ start the planner   esc cancel"))
	}
	if m.plan == nil && m.planErr == "" {
		return append(out, " "+keyHint("n", "new plan")+"  "+keyHint("⏎", "attach planner")+"  "+
			keyHint("c", "collect plan")+"  "+keyHint("esc", "back"))
	}
	if m.planFeedback {
		return append(out, " Feedback for the agent (empty blocks the task): "+m.feedbackInput.View(),
			styled(sgrTUIDim, " ⏎ send back   esc cancel"))
	}
	run := "run"
	if m.planRunning {
		run = "pause"
	}
	keys := []string{
		keyHint("r", run), keyHint("⏎", "attach"), keyHint("v", "diff"), keyHint("a", "approve"),
		keyHint("x", "reject"), keyHint("d", "mark done"), keyHint("t", "retry"),
		keyHint("u", "auto-approve"), keyHint("f", "finish"), keyHint("esc", "back"),
	}
	for _, line := range wrapKeys(keys, max(m.width-2, 10)) {
		out = append(out, " "+line)
	}
	return out
}
