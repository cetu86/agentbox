package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	started   []string
	restarted map[string]string
	outcomes  map[string]taskOutcome
	integrate map[string][]error // queued results per task; empty means success
	resolver  map[string]bool    // startResolver result (needsAgent); default true
	resolved  []string
	removed   []string
	startErr  error
	startSnap string
}

func newFake() *fakeBackend {
	return &fakeBackend{
		restarted: map[string]string{},
		outcomes:  map[string]taskOutcome{},
		integrate: map[string][]error{},
		resolver:  map[string]bool{},
	}
}

func (f *fakeBackend) startTask(p *plan, t *planTask) (string, error) {
	if f.startErr != nil {
		return f.startSnap, f.startErr
	}
	f.started = append(f.started, t.ID)
	return "/snap/" + t.ID, nil
}

func (f *fakeBackend) restartTask(p *plan, t *planTask, feedback string) error {
	f.restarted[t.ID] = feedback
	return nil
}

func (f *fakeBackend) checkTask(t *planTask) (taskOutcome, error) {
	return f.outcomes[t.ID], nil
}

func (f *fakeBackend) integrateTask(planName, id string) error {
	q := f.integrate[id]
	if len(q) == 0 {
		return nil
	}
	f.integrate[id] = q[1:]
	return q[0]
}

func (f *fakeBackend) startResolver(p *plan, t *planTask) (bool, error) {
	f.resolved = append(f.resolved, t.ID)
	if needs, ok := f.resolver[t.ID]; ok {
		return needs, nil
	}
	return true, nil
}

func (f *fakeBackend) removeTaskBox(id string) error {
	f.removed = append(f.removed, id)
	return nil
}

func newTestOrch(t *testing.T, maxParallel int) (*orchestrator, *fakeBackend) {
	t.Helper()
	f := newFake()
	return &orchestrator{
		plan:        testPlan(t),
		state:       newPlanState(),
		backend:     f,
		maxParallel: maxParallel,
		now:         func() time.Time { return time.Unix(1000, 0) },
	}, f
}

func mustStep(t *testing.T, o *orchestrator) {
	t.Helper()
	if err := o.step(); err != nil {
		t.Fatal(err)
	}
}

func finish(f *fakeBackend, ids ...string) {
	for _, id := range ids {
		f.outcomes[id] = taskOutcome{taskResult: taskResult{Done: true, Summary: "did " + id}}
	}
}

func wantStatus(t *testing.T, o *orchestrator, want map[string]string) {
	t.Helper()
	for id, st := range want {
		if got := o.state.status(id); got != st {
			t.Errorf("%s = %s, want %s", id, got, st)
		}
	}
}

func TestStepStartsRootsInParallel(t *testing.T) {
	o, f := newTestOrch(t, 3)
	mustStep(t, o)
	if strings.Join(f.started, ",") != "a,b" {
		t.Fatalf("started %v", f.started)
	}
	wantStatus(t, o, map[string]string{"a": taskRunning, "b": taskRunning, "c": taskPending})
	if ts := o.state.Tasks["a"]; ts.Attempts != 1 || ts.Box != "a" {
		t.Fatalf("state: %+v", ts)
	}
}

func TestStepRespectsMaxParallel(t *testing.T) {
	o, f := newTestOrch(t, 1)
	mustStep(t, o)
	if strings.Join(f.started, ",") != "a" {
		t.Fatalf("started %v", f.started)
	}
}

func TestFullFlowWithManualAndAutoApproval(t *testing.T) {
	o, f := newTestOrch(t, 3)
	mustStep(t, o)

	finish(f, "a", "b")
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskReview, "b": taskReview, "c": taskPending})
	if o.state.Tasks["a"].Note != "did a" {
		t.Fatalf("note = %q", o.state.Tasks["a"].Note)
	}
	if len(f.started) != 2 {
		t.Fatalf("started too early: %v", f.started)
	}

	if err := o.approve("a"); err != nil {
		t.Fatal(err)
	}
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskAccepted, "b": taskReview, "c": taskPending})

	if err := o.approve("b"); err != nil {
		t.Fatal(err)
	}
	mustStep(t, o) // c becomes ready
	wantStatus(t, o, map[string]string{"c": taskRunning})

	// c has auto_approve: it goes straight through, and d starts.
	finish(f, "c")
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"c": taskAccepted, "d": taskRunning})
	if strings.Join(f.removed, ",") != "a,b,c" {
		t.Fatalf("removed boxes: %v", f.removed)
	}

	finish(f, "d")
	mustStep(t, o)
	if err := o.approve("d"); err != nil {
		t.Fatal(err)
	}
	if !o.plan.isComplete(o.state) {
		t.Fatal("plan should be complete")
	}
}

func TestBlockedMarkerBlocksTaskAndDependents(t *testing.T) {
	o, f := newTestOrch(t, 3)
	mustStep(t, o)
	f.outcomes["a"] = taskOutcome{taskResult: taskResult{Blocked: true, Summary: "no network"}}
	finish(f, "b")
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskBlocked})
	if o.state.Tasks["a"].Note != "no network" {
		t.Fatalf("note = %q", o.state.Tasks["a"].Note)
	}
	if got := o.plan.displayStatus(o.state, "c"); got != "unreachable" {
		t.Fatalf("c = %s", got)
	}
}

func TestStoppedAgentIsFlaggedOnce(t *testing.T) {
	o, f := newTestOrch(t, 3)
	mustStep(t, o)
	f.outcomes["a"] = taskOutcome{Stopped: true}
	mustStep(t, o)
	if o.state.status("a") != taskRunning || !strings.Contains(o.state.Tasks["a"].Note, "exited") {
		t.Fatalf("a: %+v", o.state.Tasks["a"])
	}
	before := len(o.events)
	mustStep(t, o)
	if len(o.events) != before {
		t.Fatalf("flag was reported again: %v", o.events[before:])
	}

	if err := o.markDone("a"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskReview})
	if o.state.Tasks["a"].Note != "marked done manually" {
		t.Fatalf("note = %q", o.state.Tasks["a"].Note)
	}
}

func inReview(t *testing.T, o *orchestrator, f *fakeBackend, id string) {
	t.Helper()
	o.setStatus(id, taskRunning, "")
	finish(f, id)
	o.reconcileAgent(o.plan.task(id))
	if o.state.status(id) != taskReview {
		t.Fatalf("%s not in review: %s", id, o.state.status(id))
	}
}

func TestRejectWithFeedbackRestartsAgent(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	if err := o.reject("a", "add tests"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskRunning})
	if f.restarted["a"] != "add tests" || o.state.Tasks["a"].Attempts != 1 {
		t.Fatalf("restart: %v, attempts %d", f.restarted, o.state.Tasks["a"].Attempts)
	}
}

func TestRejectWithoutFeedbackBlocks(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	if err := o.reject("a", ""); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskBlocked})
	if err := o.reject("b", "x"); err == nil {
		t.Fatal("rejecting a task that is not in review should fail")
	}
}

func TestRetryResetsBlockedTask(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	_ = o.reject("a", "")
	if err := o.retry("a"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskPending})
	if len(f.removed) != 1 || f.removed[0] != "a" {
		t.Fatalf("removed %v", f.removed)
	}
	if err := o.retry("d"); err == nil {
		t.Fatal("retrying a pending task should fail")
	}
}

func TestConflictStartsResolverOnceThenAccepts(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	f.integrate["a"] = []error{fmt.Errorf("%w: x", errPlanConflict)}

	if err := o.approve("a"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskConflict})
	if !o.state.Tasks["a"].Resolved || strings.Join(f.resolved, ",") != "a" {
		t.Fatalf("resolver: %v, %+v", f.resolved, o.state.Tasks["a"])
	}

	// The resolver agent is still working: nothing changes.
	f.outcomes["a"] = taskOutcome{AgentRunning: true}
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskConflict})

	// Resolver finishes; the merge now succeeds without further sign-off.
	finish(f, "a")
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskAccepted})
}

func TestSecondConflictBlocks(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	conflict := fmt.Errorf("%w: x", errPlanConflict)
	f.integrate["a"] = []error{conflict, conflict}
	_ = o.approve("a")
	finish(f, "a")
	mustStep(t, o)
	wantStatus(t, o, map[string]string{"a": taskBlocked})
	if !strings.Contains(o.state.Tasks["a"].Note, "resolver") {
		t.Fatalf("note = %q", o.state.Tasks["a"].Note)
	}
	if len(f.resolved) != 1 {
		t.Fatalf("resolver started %d times", len(f.resolved))
	}
}

func TestConflictThatVanishesMergesAgain(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	f.integrate["a"] = []error{errPlanConflict}
	f.resolver["a"] = false
	if err := o.approve("a"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskAccepted})
}

func TestResolverFailureToStartBlocks(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	f.integrate["a"] = []error{errPlanConflict}
	f.resolver["a"] = true
	o.backend = &resolverFails{f}
	if err := o.approve("a"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskBlocked})
}

type resolverFails struct{ *fakeBackend }

func (resolverFails) startResolver(*plan, *planTask) (bool, error) {
	return false, errors.New("boom")
}

func TestMergeInfrastructureErrorKeepsMergingAndRetries(t *testing.T) {
	o, f := newTestOrch(t, 3)
	inReview(t, o, f, "a")
	f.integrate["a"] = []error{errors.New("worktree busy")}
	if err := o.approve("a"); err == nil {
		t.Fatal("expected the error to be returned")
	}
	wantStatus(t, o, map[string]string{"a": taskMerging})
	if !strings.Contains(o.state.Tasks["a"].Note, "worktree busy") {
		t.Fatalf("note = %q", o.state.Tasks["a"].Note)
	}
	mustStep(t, o) // retries and succeeds
	wantStatus(t, o, map[string]string{"a": taskAccepted})
}

func TestStartFailureHandling(t *testing.T) {
	o, f := newTestOrch(t, 3)
	f.startErr = errors.New("project dirty")
	if err := o.step(); err == nil {
		t.Fatal("expected an error")
	}
	wantStatus(t, o, map[string]string{"a": taskPending})

	// The box exists but the agent failed to start: the task is blocked.
	f.startSnap = "/snap/a"
	if err := o.step(); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, o, map[string]string{"a": taskBlocked, "b": taskBlocked})
}

func TestIdle(t *testing.T) {
	o, f := newTestOrch(t, 3)
	if o.idle() {
		t.Fatal("ready tasks mean not idle")
	}
	mustStep(t, o)
	if o.idle() {
		t.Fatal("running agents mean not idle")
	}
	finish(f, "a", "b")
	mustStep(t, o)
	if !o.idle() {
		t.Fatal("only review is pending: idle (waiting for the human)")
	}
}

func TestApproveRequiresReview(t *testing.T) {
	o, _ := newTestOrch(t, 3)
	if err := o.approve("a"); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("err = %v", err)
	}
	if err := o.approve("zzz"); err == nil {
		t.Fatal("unknown task should fail")
	}
}

func TestWithPlanPersistsStateEvenOnError(t *testing.T) {
	root := t.TempDir()
	if err := savePlan(root, testPlan(t)); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Project: root}
	boom := errors.New("boom")
	err := cfg.withPlan(func(o *orchestrator) error {
		o.setStatus("a", taskBlocked, "why")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	st, err := loadPlanState(root)
	if err != nil || st.status("a") != taskBlocked || st.Tasks["a"].Note != "why" {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestWithPlanRejectsInvalidPlan(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{Project: root}
	if err := cfg.withPlan(func(*orchestrator) error { return nil }); err == nil {
		t.Fatal("missing plan should fail")
	}
}
