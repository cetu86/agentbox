package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	planDir       = ".agentbox"
	planFileName  = "plan.json"
	planStateName = "plan-state.json"
	planLockName  = "plan.lock"

	defaultMaxParallel = 3
)

// Task statuses stored in the plan state. "ready" is never stored: it is
// derived from pending tasks whose dependencies are all accepted.
const (
	taskPending  = "pending"
	taskRunning  = "running"  // agent box is working
	taskReview   = "review"   // agent finished; waiting for human sign-off
	taskMerging  = "merging"  // approved; merging into the integration branch
	taskConflict = "conflict" // merge conflicted; resolver agent is working
	taskAccepted = "accepted" // merged into the integration branch
	taskBlocked  = "blocked"  // rejected or failed; needs the human
)

// planTask is one unit of work. The ID doubles as the box name, so it must be
// a valid box name.
type planTask struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Prompt      string   `json:"prompt"`
	DependsOn   []string `json:"depends_on,omitempty"`
	AutoApprove bool     `json:"auto_approve,omitempty"`
	Net         string   `json:"net,omitempty"`
}

type plan struct {
	Name  string     `json:"name"`
	Tasks []planTask `json:"tasks"`
}

type taskState struct {
	Status   string    `json:"status"`
	Box      string    `json:"box,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	Note     string    `json:"note,omitempty"`     // latest summary, reason or problem, shown to the human
	Resolved bool      `json:"resolved,omitempty"` // a conflict resolver agent was already used
	Updated  time.Time `json:"updated,omitzero"`
}

// planState is the volatile execution state, kept apart from the committed plan.
type planState struct {
	Tasks map[string]*taskState `json:"tasks"`
}

func planPath(root string) string      { return filepath.Join(root, planDir, planFileName) }
func planStatePath(root string) string { return filepath.Join(root, planDir, planStateName) }

func parsePlan(data []byte) (*plan, error) {
	var p plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func loadPlan(root string) (*plan, error) {
	data, err := os.ReadFile(planPath(root))
	if err != nil {
		return nil, err
	}
	return parsePlan(data)
}

func savePlan(root string, p *plan) error {
	if err := p.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(planPath(root), append(data, '\n'), 0o644)
}

// validate reports every problem it finds, so a planner agent can fix them in
// one pass.
func (p *plan) validate() error {
	var problems []string
	if !validName.MatchString(p.Name) {
		problems = append(problems, fmt.Sprintf("plan name %q must match %s", p.Name, validName))
	}
	if len(p.Tasks) == 0 {
		problems = append(problems, "plan has no tasks")
	}
	ids := map[string]bool{}
	for _, t := range p.Tasks {
		switch {
		case !validName.MatchString(t.ID):
			problems = append(problems, fmt.Sprintf("task id %q must match %s", t.ID, validName))
		case ids[t.ID]:
			problems = append(problems, fmt.Sprintf("duplicate task id %q", t.ID))
		}
		ids[t.ID] = true
		if strings.TrimSpace(t.Prompt) == "" {
			problems = append(problems, fmt.Sprintf("task %q has an empty prompt", t.ID))
		}
		switch t.Net {
		case "", "proxy", "none", "open":
		default:
			problems = append(problems, fmt.Sprintf("task %q: net %q must be proxy, none or open", t.ID, t.Net))
		}
		seen := map[string]bool{}
		for _, d := range t.DependsOn {
			switch {
			case d == t.ID:
				problems = append(problems, fmt.Sprintf("task %q depends on itself", t.ID))
			case seen[d]:
				problems = append(problems, fmt.Sprintf("task %q lists dependency %q twice", t.ID, d))
			}
			seen[d] = true
		}
	}
	for _, t := range p.Tasks {
		for _, d := range t.DependsOn {
			if !ids[d] {
				problems = append(problems, fmt.Sprintf("task %q depends on unknown task %q", t.ID, d))
			}
		}
	}
	if len(problems) == 0 {
		if cycle := p.findCycle(); cycle != nil {
			problems = append(problems, "dependency cycle: "+strings.Join(cycle, " -> "))
		}
	}
	if len(problems) > 0 {
		return errors.New("invalid plan:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

func (p *plan) task(id string) *planTask {
	for i := range p.Tasks {
		if p.Tasks[i].ID == id {
			return &p.Tasks[i]
		}
	}
	return nil
}

// findCycle returns one dependency cycle (first and last element equal), or nil.
// It assumes every dependency refers to an existing task.
func (p *plan) findCycle() []string {
	const (
		unvisited = iota
		visiting
		done
	)
	mark := map[string]int{}
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		mark[id] = visiting
		stack = append(stack, id)
		for _, d := range p.task(id).DependsOn {
			switch mark[d] {
			case visiting:
				for i, s := range stack {
					if s == d {
						return append(append([]string{}, stack[i:]...), d)
					}
				}
			case unvisited:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		mark[id] = done
		return nil
	}
	for _, t := range p.Tasks {
		if mark[t.ID] == unvisited {
			if c := visit(t.ID); c != nil {
				return c
			}
		}
	}
	return nil
}

// levels groups tasks by depth: every task in a level depends only on earlier
// levels, so the tasks within one level can run in parallel. Each level is
// sorted by plan order. The plan must be valid (acyclic).
func (p *plan) levels() [][]string {
	depth := map[string]int{}
	var depthOf func(id string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		d := 0
		for _, dep := range p.task(id).DependsOn {
			d = max(d, depthOf(dep)+1)
		}
		depth[id] = d
		return d
	}
	var out [][]string
	for _, t := range p.Tasks {
		d := depthOf(t.ID)
		for len(out) <= d {
			out = append(out, nil)
		}
		out[d] = append(out[d], t.ID)
	}
	return out
}

// dependents returns the ids of tasks that transitively depend on id, in plan order.
func (p *plan) dependents(id string) []string {
	hit := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for _, t := range p.Tasks {
			if hit[t.ID] {
				continue
			}
			for _, d := range t.DependsOn {
				if hit[d] {
					hit[t.ID] = true
					changed = true
					break
				}
			}
		}
	}
	var out []string
	for _, t := range p.Tasks {
		if t.ID != id && hit[t.ID] {
			out = append(out, t.ID)
		}
	}
	return out
}

func newPlanState() *planState {
	return &planState{Tasks: map[string]*taskState{}}
}

func (s *planState) status(id string) string {
	if ts := s.Tasks[id]; ts != nil && ts.Status != "" {
		return ts.Status
	}
	return taskPending
}

// set records a status change, creating the entry if needed.
func (s *planState) set(id, status string, now time.Time) *taskState {
	if s.Tasks == nil {
		s.Tasks = map[string]*taskState{}
	}
	ts := s.Tasks[id]
	if ts == nil {
		ts = &taskState{}
		s.Tasks[id] = ts
	}
	ts.Status = status
	ts.Updated = now
	return ts
}

func loadPlanState(root string) (*planState, error) {
	data, err := os.ReadFile(planStatePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return newPlanState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := newPlanState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", planStatePath(root), err)
	}
	if s.Tasks == nil {
		s.Tasks = map[string]*taskState{}
	}
	return s, nil
}

func savePlanState(root string, s *planState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, planDir), 0o755); err != nil {
		return err
	}
	// The volatile files must not show up as untracked changes in the project.
	ignore := filepath.Join(root, planDir, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte(planStateName+"\n"+planLockName+"\n"), 0o644); err != nil {
			return err
		}
	}
	return writeFileAtomic(planStatePath(root), append(data, '\n'), 0o644)
}

// isBlockedByDeps reports whether a task cannot ever start until the human
// resolves a blocked ancestor.
func (p *plan) isBlockedByDeps(s *planState, id string) bool {
	seen := map[string]bool{}
	var walk func(id string) bool
	walk = func(id string) bool {
		for _, d := range p.task(id).DependsOn {
			if seen[d] {
				continue
			}
			seen[d] = true
			if s.status(d) == taskBlocked || walk(d) {
				return true
			}
		}
		return false
	}
	return walk(id)
}

// readyTasks returns the pending tasks that may start now: all dependencies
// accepted, and not more than maxParallel agents active (running or resolving
// a conflict). Tasks awaiting review do not hold a slot. maxParallel <= 0 uses
// the default.
func (p *plan) readyTasks(s *planState, maxParallel int) []string {
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallel
	}
	active := 0
	for _, t := range p.Tasks {
		if st := s.status(t.ID); st == taskRunning || st == taskConflict {
			active++
		}
	}
	var out []string
	for _, t := range p.Tasks {
		if active+len(out) >= maxParallel {
			break
		}
		if s.status(t.ID) != taskPending {
			continue
		}
		ready := true
		for _, d := range t.DependsOn {
			if s.status(d) != taskAccepted {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, t.ID)
		}
	}
	return out
}

// isComplete reports whether every task has been accepted.
func (p *plan) isComplete(s *planState) bool {
	for _, t := range p.Tasks {
		if s.status(t.ID) != taskAccepted {
			return false
		}
	}
	return true
}

// planSummary counts tasks per status; "ready" counts startable pending tasks
// ignoring the parallel limit.
func (p *plan) planSummary(s *planState) map[string]int {
	counts := map[string]int{}
	for _, t := range p.Tasks {
		counts[p.displayStatus(s, t.ID)]++
	}
	return counts
}

// displayStatus is the stored status, with pending tasks refined into ready
// (can start), unreachable (an ancestor is blocked) or waiting.
func (p *plan) displayStatus(s *planState, id string) string {
	st := s.status(id)
	if st != taskPending {
		return st
	}
	switch {
	case p.depsAccepted(s, id):
		return "ready"
	case p.isBlockedByDeps(s, id):
		return "unreachable"
	}
	return "waiting"
}

func (p *plan) depsAccepted(s *planState, id string) bool {
	for _, d := range p.task(id).DependsOn {
		if s.status(d) != taskAccepted {
			return false
		}
	}
	return true
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
