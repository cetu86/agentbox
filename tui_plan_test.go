package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// testPlanModel is a TUI model on the plan screen with the demo plan loaded
// (order: a, b, c, d) and a real project directory for plan edits.
func testPlanModel(t *testing.T, boxes ...boxInfo) tuiModel {
	t.Helper()
	root := t.TempDir()
	p := testPlan(t)
	if err := savePlan(root, p); err != nil {
		t.Fatal(err)
	}
	m := testTUIModel(boxes...)
	m.cfg.Project = root
	m.screen = screenPlan
	m.plan, m.planSt, m.planLoaded = p, newPlanState(), true
	return m
}

func TestPlanScreenOpensFromList(t *testing.T) {
	m := testTUIModel()
	m, cmd := press(t, m, "P")
	if m.screen != screenPlan || cmd == nil {
		t.Fatalf("screen = %v, cmd = %v", m.screen, cmd)
	}
	// No plan file: the view explains instead of failing.
	msg := cmd().(planLoadedMsg)
	m = m.applyPlanLoaded(msg)
	if !strings.Contains(m.View(), "No plan found") {
		t.Fatalf("view:\n%s", m.View())
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("screen = %v", m.screen)
	}
}

func TestPlanLoadShowsWavesAndStates(t *testing.T) {
	m := testPlanModel(t, boxInfo{Name: "a", Running: true})
	m.planSt.set("a", taskReview, time.Time{})
	m.width, m.height = 70, 24
	view := ansi.Strip(m.View())
	for _, want := range []string{"wave 1 (parallel)", "wave 2 (single)", "wave 3", "after a,b", "⚡auto", "review", "waiting", "ready", "● a"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("%d lines, want %d", len(lines), m.height)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > m.width {
			t.Fatalf("line too wide: %q", l)
		}
	}
}

func TestPlanViewFitsSmallTerminal(t *testing.T) {
	m := testPlanModel(t)
	m.width, m.height = 40, 10
	for i := range 4 {
		m.planCursor = i
		lines := strings.Split(m.View(), "\n")
		if len(lines) > m.height {
			t.Fatalf("%d lines > %d", len(lines), m.height)
		}
	}
}

func TestPlanKeysRespectStatus(t *testing.T) {
	m := testPlanModel(t)
	for _, key := range []string{"a", "x", "d", "t"} {
		next, cmd := press(t, m, key)
		if !next.statusErr || cmd != nil || next.screen != screenPlan {
			t.Errorf("%s on a pending task: err=%v status=%q", key, next.statusErr, next.status)
		}
	}
	m.planSt.set("a", taskReview, time.Time{})
	next, _ := press(t, m, "a")
	if next.screen != screenConfirm || next.confirmBack != screenPlan {
		t.Fatalf("approve: screen=%v back=%v", next.screen, next.confirmBack)
	}
	// Declining returns to the plan screen, not the list.
	next, _ = press(t, next, "n")
	if next.screen != screenPlan || next.confirmBack != screenList {
		t.Fatalf("after n: screen=%v back=%v", next.screen, next.confirmBack)
	}
}

func TestPlanRejectFeedbackInput(t *testing.T) {
	m := testPlanModel(t)
	m.planSt.set("a", taskReview, time.Time{})
	m, _ = press(t, m, "x")
	if !m.planFeedback {
		t.Fatal("feedback input should open")
	}
	m, _ = press(t, m, "q") // typed into the field, not "back"
	if m.screen != screenPlan || m.feedbackInput.Value() != "q" {
		t.Fatalf("screen=%v value=%q", m.screen, m.feedbackInput.Value())
	}
	m, _ = press(t, m, "esc")
	if m.planFeedback || m.screen != screenPlan {
		t.Fatal("esc should only close the input")
	}
}

func TestPlanToggleAutoApprovePersists(t *testing.T) {
	m := testPlanModel(t)
	m, cmd := press(t, m, "u")
	if cmd == nil || m.busy == "" {
		t.Fatal("toggle should run an operation")
	}
	done := cmd().(opDoneMsg)
	if done.err != nil || !strings.Contains(done.message, "auto-approve ON for a") {
		t.Fatalf("done = %+v", done)
	}
	p, err := loadPlan(m.cfg.Project)
	if err != nil || !p.task("a").AutoApprove {
		t.Fatalf("plan not saved: %v %+v", err, p)
	}
	m.busy = ""
	m = m.applyPlanLoaded(m.planLoadCmd()().(planLoadedMsg))
	_, cmd = press(t, m, "u")
	if done := cmd().(opDoneMsg); done.err != nil {
		t.Fatal(done.err)
	}
	p, _ = loadPlan(m.cfg.Project)
	if p.task("a").AutoApprove {
		t.Fatal("second toggle should turn it off")
	}
}

func TestPlanAutoApproveRefusedWhenAccepted(t *testing.T) {
	m := testPlanModel(t)
	m.planSt.set("a", taskAccepted, time.Time{})
	next, cmd := press(t, m, "u")
	if cmd != nil || !next.statusErr {
		t.Fatalf("cmd=%v status=%q", cmd, next.status)
	}
}

func TestPlanBoxActionNeedsBox(t *testing.T) {
	m := testPlanModel(t)
	next, cmd := press(t, m, "enter")
	if cmd != nil || !next.statusErr {
		t.Fatalf("attach without box: cmd=%v status=%q", cmd, next.status)
	}
	m.boxes = []boxInfo{{Name: "a", Status: "exited"}}
	next, cmd = press(t, m, "enter")
	if cmd != nil || next.screen == screenConfirm {
		t.Fatalf("leftover box of an unstarted task must not be resumed: screen=%v", next.screen)
	}
	m.planSt.set("a", taskRunning, time.Time{})
	next, _ = press(t, m, "enter")
	if next.screen != screenConfirm || next.confirmBack != screenPlan {
		t.Fatalf("resume prompt: screen=%v back=%v", next.screen, next.confirmBack)
	}
	m.boxes = []boxInfo{{Name: "a", Running: true}}
	if _, cmd = press(t, m, "enter"); cmd == nil {
		t.Fatal("running box should attach")
	}
}

func TestPlanDiffReturnsToPlan(t *testing.T) {
	m := testPlanModel(t, boxInfo{Name: "a"})
	m, cmd := press(t, m, "v")
	if cmd == nil || m.viewBack != screenPlan {
		t.Fatalf("viewBack=%v", m.viewBack)
	}
	next, _ := m.Update(viewLoadedMsg{title: "t", text: "+x"})
	m = next.(tuiModel)
	m, _ = press(t, m, "q")
	if m.screen != screenPlan {
		t.Fatalf("screen = %v", m.screen)
	}
}

func TestPlanFinishRequiresCompletePlan(t *testing.T) {
	m := testPlanModel(t)
	m, cmd := press(t, m, "f")
	if cmd != nil || !m.statusErr {
		t.Fatalf("incomplete plan: cmd=%v status=%q", cmd, m.status)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		m.planSt.set(id, taskAccepted, time.Time{})
	}
	m, cmd = press(t, m, "f")
	if cmd == nil || m.busy == "" {
		t.Fatal("complete plan should load the final summary")
	}
	m = m.openFinishConfirm(planFinishMsg{stat: " a.txt | 1 +\n 1 file changed"})
	if m.screen != screenConfirm || m.confirmBack != screenPlan || !strings.Contains(ansi.Strip(m.View()), "a.txt") {
		t.Fatalf("screen=%v\n%s", m.screen, m.View())
	}
}

func TestPlanStepResultHandling(t *testing.T) {
	m := testPlanModel(t)
	m.planRunning, m.planStepping = true, true

	next, cmd := m.applyPlanStep(planStepMsg{events: []string{"a: started"}})
	if next.planStepping || !next.planRunning || cmd == nil || len(next.planLog) != 1 {
		t.Fatalf("running step: %+v", next)
	}

	next, _ = m.applyPlanStep(planStepMsg{finished: true, idle: true})
	if next.planRunning || !strings.Contains(next.status, "plan complete") {
		t.Fatalf("finished: running=%v status=%q", next.planRunning, next.status)
	}

	next, _ = m.applyPlanStep(planStepMsg{idle: true})
	if next.planRunning || !next.statusErr {
		t.Fatalf("idle: running=%v status=%q", next.planRunning, next.status)
	}

	next, _ = m.applyPlanStep(planStepMsg{idle: true, reviewing: true})
	if !next.planRunning {
		t.Fatal("pending reviews keep the orchestrator waiting")
	}

	next, _ = m.applyPlanStep(planStepMsg{err: errors.New("boom")})
	if !next.planRunning || !next.statusErr || next.planLastErr != "boom" {
		t.Fatalf("error: %+v", next)
	}
}

func TestPlanRunToggleNeedsPlan(t *testing.T) {
	m := testTUIModel()
	m.screen = screenPlan
	m, cmd := press(t, m, "r")
	if cmd != nil || !m.statusErr || m.planRunning {
		t.Fatalf("no plan: running=%v status=%q", m.planRunning, m.status)
	}
	m = testPlanModel(t)
	m, cmd = press(t, m, "r")
	if cmd == nil || !m.planRunning || !m.planStepping {
		t.Fatal("run should start a step")
	}
	m.planStepping = false
	m, _ = press(t, m, "r")
	if m.planRunning {
		t.Fatal("second press pauses")
	}
}

func TestPlanLoadErrorsAreShown(t *testing.T) {
	m := testTUIModel()
	m.screen = screenPlan
	m = m.applyPlanLoaded(planLoadedMsg{err: errors.New("plan: cycle a -> b")})
	if view := ansi.Strip(m.View()); !strings.Contains(view, "cycle a -> b") {
		t.Fatalf("view:\n%s", view)
	}
	m = m.applyPlanLoaded(planLoadedMsg{err: os.ErrNotExist})
	if m.planErr != "" {
		t.Fatalf("missing plan is not an error: %q", m.planErr)
	}
}

func TestPlanScreenWithoutPlanOffersPlanner(t *testing.T) {
	m := testTUIModel()
	m.screen, m.planLoaded = screenPlan, true
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Press n") || !strings.Contains(v, "new plan") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(t, m, "n")
	if !m.planFeedback || !m.planGoal {
		t.Fatal("n did not open the goal input")
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Goal for the planner") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(t, m, "esc")
	if m.planFeedback || m.planGoal {
		t.Fatal("esc did not close the goal input")
	}
	// c/enter without a planner explain instead of acting.
	m, cmd := press(t, m, "c")
	if cmd != nil || !m.statusErr {
		t.Fatalf("c without planner: cmd=%v status=%q", cmd, m.status)
	}
}

func TestPlanScreenWithPlannerBox(t *testing.T) {
	m := testTUIModel(boxInfo{Name: plannerBox, Running: true})
	m.screen, m.planLoaded = screenPlan, true
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Planner agent: running") {
		t.Fatalf("view:\n%s", v)
	}
	if _, cmd := press(t, m, "enter"); cmd == nil {
		t.Fatal("enter did not attach to the planner")
	}
	m, _ = press(t, m, "n")
	if m.planFeedback || !m.statusErr {
		t.Fatalf("n with an existing planner must refuse: %q", m.status)
	}
}
