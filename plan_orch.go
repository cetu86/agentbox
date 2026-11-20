package main

import (
	"errors"
	"fmt"
	"time"
)

// planBackend is everything the orchestrator needs from the outside world
// (containers, snapshots, git). *Config implements it; tests use a fake.
type planBackend interface {
	startTask(p *plan, t *planTask) (snap string, err error)
	restartTask(p *plan, t *planTask, feedback string) error
	checkTask(t *planTask) (taskOutcome, error)
	integrateTask(planName, taskID string) error
	startResolver(p *plan, t *planTask) (needsAgent bool, err error)
	removeTaskBox(taskID string) error
}

// orchestrator advances a plan: it starts ready tasks, notices finished
// agents, and merges approved work into the integration branch. It holds no
// state of its own; everything lives in plan and state, which the caller loads
// and saves (see withPlan).
type orchestrator struct {
	plan        *plan
	state       *planState
	backend     planBackend
	maxParallel int
	now         func() time.Time
	events      []string
}

func (o *orchestrator) logf(format string, args ...any) {
	o.events = append(o.events, fmt.Sprintf(format, args...))
}

func (o *orchestrator) timestamp() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// setStatus records a transition and the accompanying note.
func (o *orchestrator) setStatus(id, status, note string) *taskState {
	ts := o.state.set(id, status, o.timestamp())
	ts.Note = note
	return ts
}

// setNote changes the note only, and reports whether it changed.
func (o *orchestrator) setNote(id, note string) bool {
	ts := o.state.Tasks[id]
	if ts == nil || ts.Note == note {
		return false
	}
	ts.Note = note
	ts.Updated = o.timestamp()
	return true
}

func (o *orchestrator) block(id, reason string) {
	o.setStatus(id, taskBlocked, reason)
	o.logf("%s: blocked: %s", id, reason)
}

// step is one orchestration pass: reconcile running tasks with their markers,
// merge auto-approved work, and start whatever is ready. A returned error means
// something outside the plan failed (e.g. a dirty project); state changes made
// before it still count and must be saved.
func (o *orchestrator) step() error {
	o.reconcile()
	return o.startReady()
}

func (o *orchestrator) reconcile() {
	for i := range o.plan.Tasks {
		t := &o.plan.Tasks[i]
		switch o.state.status(t.ID) {
		case taskRunning, taskConflict:
			o.reconcileAgent(t)
		case taskMerging:
			if err := o.merge(t); err != nil {
				o.noteProblem(t.ID, err)
			}
		case taskReview:
			if t.AutoApprove {
				if err := o.merge(t); err != nil {
					o.noteProblem(t.ID, err)
				}
			}
		}
	}
}

func (o *orchestrator) noteProblem(id string, err error) {
	if o.setNote(id, err.Error()) {
		o.logf("%s: %v", id, err)
	}
}

func (o *orchestrator) reconcileAgent(t *planTask) {
	out, err := o.backend.checkTask(t)
	if err != nil {
		o.block(t.ID, "cannot inspect box: "+err.Error())
		return
	}
	status := o.state.status(t.ID)
	switch {
	case out.Blocked:
		o.block(t.ID, orDefault(out.Summary, "the agent reported it cannot complete the task"))
	case out.Done:
		o.agentDone(t, status, out.Summary)
	case out.Stopped:
		if o.setNote(t.ID, "agent exited without writing the done marker; mark it done, resume it, or retry") {
			o.logf("%s: agent exited without finishing", t.ID)
		}
	}
}

// agentDone handles a finished agent: a worker moves to review, a resolver
// goes straight on to the merge the human already approved.
func (o *orchestrator) agentDone(t *planTask, status, summary string) {
	if status == taskConflict {
		o.logf("%s: conflict resolver finished", t.ID)
		if err := o.merge(t); err != nil {
			o.noteProblem(t.ID, err)
		}
		return
	}
	o.setStatus(t.ID, taskReview, summary)
	if t.AutoApprove {
		o.logf("%s: finished, auto-approved", t.ID)
		if err := o.merge(t); err != nil {
			o.noteProblem(t.ID, err)
		}
		return
	}
	o.logf("%s: finished, waiting for review", t.ID)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// merge integrates an approved task into the plan branch. On a conflict it
// starts a resolver agent once; a second conflict blocks the task.
func (o *orchestrator) merge(t *planTask) error {
	ts := o.state.Tasks[t.ID]
	if ts == nil || ts.Status != taskMerging {
		o.setStatus(t.ID, taskMerging, "")
	}
	err := o.backend.integrateTask(o.plan.Name, t.ID)
	switch {
	case err == nil:
		o.setStatus(t.ID, taskAccepted, "")
		o.logf("%s: accepted into %s", t.ID, planBranch(o.plan.Name))
		if rerr := o.backend.removeTaskBox(t.ID); rerr != nil {
			o.logf("%s: accepted, but removing its box failed: %v", t.ID, rerr)
		}
		return nil
	case errors.Is(err, errPlanConflict):
		if ts != nil && ts.Resolved {
			o.block(t.ID, "still conflicts with "+planBranch(o.plan.Name)+" after the resolver agent")
			return nil
		}
		return o.resolveConflict(t)
	default:
		// Keep "merging" so the next pass retries once the cause is fixed.
		return err
	}
}

func (o *orchestrator) resolveConflict(t *planTask) error {
	ts := o.setStatus(t.ID, taskConflict, "merge conflict; resolver agent started")
	ts.Resolved = true
	needsAgent, err := o.backend.startResolver(o.plan, t)
	if err != nil {
		o.block(t.ID, "conflict resolver could not start: "+err.Error())
		return nil
	}
	if !needsAgent {
		o.logf("%s: conflict vanished after updating the box; merging again", t.ID)
		return o.merge(t)
	}
	o.logf("%s: merge conflict, resolver agent started", t.ID)
	return nil
}

// startReady starts pending tasks whose dependencies are accepted, within the
// parallel limit.
func (o *orchestrator) startReady() error {
	for _, id := range o.plan.readyTasks(o.state, o.maxParallel) {
		t := o.plan.task(id)
		snap, err := o.backend.startTask(o.plan, t)
		if err != nil {
			if snap == "" {
				return fmt.Errorf("start %s: %w", id, err)
			}
			o.block(id, "agent failed to start: "+err.Error())
			continue
		}
		ts := o.setStatus(id, taskRunning, "")
		ts.Box = id
		ts.Attempts++
		ts.Resolved = false
		o.logf("%s: started", id)
	}
	return nil
}

func (o *orchestrator) requireStatus(id string, allowed ...string) (*planTask, error) {
	t := o.plan.task(id)
	if t == nil {
		return nil, fmt.Errorf("no task %q in plan %q", id, o.plan.Name)
	}
	st := o.state.status(id)
	for _, a := range allowed {
		if st == a {
			return t, nil
		}
	}
	return nil, fmt.Errorf("task %q is %s; expected %s", id, st, joinOr(allowed))
}

func joinOr(list []string) string {
	switch len(list) {
	case 1:
		return list[0]
	case 2:
		return list[0] + " or " + list[1]
	}
	return fmt.Sprint(list)
}

// approve signs off a task in review (or retries a merge that failed) and
// merges it into the integration branch.
func (o *orchestrator) approve(id string) error {
	t, err := o.requireStatus(id, taskReview, taskMerging)
	if err != nil {
		return err
	}
	if err := o.merge(t); err != nil {
		o.noteProblem(id, err)
		return err
	}
	return nil
}

// reject sends a task in review back. With feedback the agent is restarted with
// it; without, the task is blocked until the human steps in.
func (o *orchestrator) reject(id, feedback string) error {
	t, err := o.requireStatus(id, taskReview)
	if err != nil {
		return err
	}
	if feedback == "" {
		o.block(id, "rejected")
		return nil
	}
	if err := o.backend.restartTask(o.plan, t, feedback); err != nil {
		return err
	}
	ts := o.setStatus(id, taskRunning, "")
	ts.Attempts++
	o.logf("%s: sent back with feedback", id)
	return nil
}

// markDone is the manual substitute for the done marker, e.g. when the agent
// exited without writing it.
func (o *orchestrator) markDone(id string) error {
	t, err := o.requireStatus(id, taskRunning, taskConflict)
	if err != nil {
		return err
	}
	o.agentDone(t, o.state.status(id), "marked done manually")
	return nil
}

// retry throws away a blocked (or stuck) task's box and branch and makes the
// task pending again; the next pass starts it from the integration branch.
func (o *orchestrator) retry(id string) error {
	if _, err := o.requireStatus(id, taskBlocked, taskRunning, taskConflict); err != nil {
		return err
	}
	if err := o.backend.removeTaskBox(id); err != nil {
		return err
	}
	ts := o.setStatus(id, taskPending, "")
	ts.Resolved = false
	o.logf("%s: reset, will start again", id)
	return nil
}

// idle reports whether nothing will happen without the human or the end of the
// plan: no agent is working, nothing is mergeable, and nothing can start.
func (o *orchestrator) idle() bool {
	for _, t := range o.plan.Tasks {
		switch o.state.status(t.ID) {
		case taskRunning, taskConflict, taskMerging:
			return false
		}
	}
	return len(o.plan.readyTasks(o.state, o.maxParallel)) == 0
}

// withPlan loads the plan and state under the plan lock, runs fn, and saves
// the state. The state is saved even when fn fails, since steps may have made
// progress (started agents) before the failure.
func (c *Config) withPlan(fn func(o *orchestrator) error) error {
	unlock, err := planLock(c.Project)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := loadPlan(c.Project)
	if err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	st, err := loadPlanState(c.Project)
	if err != nil {
		return err
	}
	o := &orchestrator{plan: p, state: st, backend: c, maxParallel: c.MaxParallel}
	ferr := fn(o)
	if serr := savePlanState(c.Project, st); serr != nil {
		return errors.Join(ferr, serr)
	}
	return ferr
}
