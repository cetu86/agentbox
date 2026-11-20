package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	plannerBox      = "planner"
	plannerGoalName = "agentbox-planner-goal"
)

// plannerKind classifies what collectPlan found.
type plannerKind int

const (
	plannerMissing   plannerKind = iota // no planner box exists
	plannerWorking                      // the planner has not written its marker yet
	plannerBlocked                      // the planner gave up
	plannerInvalid                      // the plan failed validation and was sent back
	plannerCollected                    // a valid plan was saved into the project
)

type plannerResult struct {
	Kind    plannerKind
	Message string // the planner's summary or reason, or the validation problems
	Plan    *plan
}

// buildPlannerPrompt instructs an agent to turn a goal into plan.json. project
// is the project path as seen inside the container.
func buildPlannerPrompt(goal, project string) string {
	planFile := filepath.Join(project, planDir, planFileName)
	var b strings.Builder
	fmt.Fprintf(&b, "You are the planner of an automated, multi-agent workflow. Goal from the user:\n\n%s\n\n", strings.TrimSpace(goal))
	fmt.Fprintf(&b, `Explore the project, talk to the user if the goal is unclear, and break the goal into tasks that separate agents will carry out, each in its own copy of the repository. Do NOT implement anything yourself.

Write the plan as JSON to %s (create the directory if needed) in exactly this shape:

{
  "name": "short-plan-name",
  "tasks": [
    {
      "id": "task-id",
      "title": "Short human title",
      "prompt": "Complete, self-contained instructions for the agent doing this task.",
      "depends_on": ["id-of-a-task-that-must-be-finished-first"]
    }
  ]
}

Rules:
- "name" and every "id" match %s (lowercase letters, digits, '.', '_', '-'; start with a letter or digit).
- Ids are unique, and depends_on lists only existing ids; there must be no cycles.
- Each prompt is read by an agent that knows nothing else about the goal: state what to build, which files or modules are involved, and what "done" means.
- Make tasks as independent as possible. Tasks without a dependency between them run in parallel, so keep their file scopes apart to avoid merge conflicts. Add depends_on only where one task really needs the other's result.
- Each task should be a reviewable unit of work, neither a one-line change nor a whole feature.
- When something needs verification, make the tests their own task that comes before the implementation task, so a human can review the tests first.
- Optional field "net" ("proxy", "none" or "open") overrides a task's network mode; leave it out unless needed.
- Do not set "auto_approve"; the user decides that during plan review.
- Allowed keys are exactly: name, tasks, id, title, prompt, depends_on, net. Valid JSON only.

`, planFile, validName)
	b.WriteString(plannerProtocol(project))
	return b.String()
}

func buildPlannerFeedbackPrompt(goal, project, problems string) string {
	planFile := filepath.Join(project, planDir, planFileName)
	var b strings.Builder
	fmt.Fprintf(&b, "You are the planner of an automated, multi-agent workflow. The user's goal:\n\n%s\n\n", strings.TrimSpace(goal))
	fmt.Fprintf(&b, "You already wrote a plan to %s, but it was rejected as invalid. Fix the file. Problems:\n\n%s\n\n", planFile, strings.TrimSpace(problems))
	b.WriteString("Allowed keys are exactly: name, tasks, id, title, prompt, depends_on, net.\n\n")
	b.WriteString(plannerProtocol(project))
	return b.String()
}

func plannerProtocol(project string) string {
	done := filepath.Join(project, ".git", doneMarkerName)
	blocked := filepath.Join(project, ".git", blockedMarkerName)
	return fmt.Sprintf(`When the user is happy with the plan, and only then, write a short summary of it to the file %s
(for example: echo "summary..." > %s). This is how the orchestrator learns the plan is ready; the plan is validated at that point. Do not create the file earlier.

If you cannot produce a plan, write the reason to %s instead and stop.
`, done, done, blocked)
}

func plannerGoalPath(snap string) string { return markerPath(snap, plannerGoalName) }

func readPlannerGoal(snap string) string {
	text, _, _ := readMarker(plannerGoalPath(snap))
	return text
}

// startPlanner creates the planner box and starts its agent with the planning
// prompt. It refuses to run when a plan or a planner already exists.
func (c *Config) startPlanner(goal string) error {
	if strings.TrimSpace(goal) == "" {
		return errors.New("a goal is required")
	}
	if _, err := os.Stat(planPath(c.Project)); err == nil {
		return fmt.Errorf("%s already exists; delete it first to plan again", planPath(c.Project))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := c.requireSnapshot(plannerBox); err == nil {
		return fmt.Errorf("a planner box already exists; attach with `agentbox attach %s`, collect with `agentbox plan collect`, or `agentbox discard %s`", plannerBox, plannerBox)
	}
	snap, err := c.createBox(plannerBox, "", true)
	if err != nil {
		return err
	}
	if err := os.WriteFile(plannerGoalPath(snap), []byte(goal), 0o644); err != nil {
		return err
	}
	return c.startContainer(plannerBox, snap, c.agentCommand(buildPlannerPrompt(goal, c.Project)))
}

// collectPlan looks at the planner box. When the planner reported done, its
// plan.json is validated: a valid one is saved into the project and the planner
// box is removed; an invalid one is sent back to a restarted planner.
func (c *Config) collectPlan() (plannerResult, error) {
	snap, err := c.requireSnapshot(plannerBox)
	if err != nil {
		return plannerResult{Kind: plannerMissing}, nil
	}
	res, err := readTaskMarker(snap)
	if err != nil {
		return plannerResult{}, err
	}
	switch {
	case res.Blocked:
		return plannerResult{Kind: plannerBlocked, Message: res.Summary}, nil
	case !res.Done:
		return plannerResult{Kind: plannerWorking}, nil
	}

	data, rerr := os.ReadFile(planPath(snap))
	var p *plan
	if rerr == nil {
		p, rerr = parsePlan(data)
	} else if errors.Is(rerr, os.ErrNotExist) {
		rerr = fmt.Errorf("%s does not exist", filepath.Join(c.Project, planDir, planFileName))
	}
	if rerr != nil {
		if err := c.sendPlannerBack(snap, rerr.Error()); err != nil {
			return plannerResult{}, errors.Join(rerr, err)
		}
		return plannerResult{Kind: plannerInvalid, Message: rerr.Error()}, nil
	}

	if _, err := os.Stat(planPath(c.Project)); err == nil {
		return plannerResult{}, fmt.Errorf("%s appeared meanwhile; not overwriting it", planPath(c.Project))
	}
	if err := savePlan(c.Project, p); err != nil {
		return plannerResult{}, err
	}
	if err := c.discardBox(plannerBox); err != nil {
		return plannerResult{}, fmt.Errorf("plan saved, but removing the planner box failed: %w", err)
	}
	return plannerResult{Kind: plannerCollected, Message: res.Summary, Plan: p}, nil
}

func (c *Config) sendPlannerBack(snap, problems string) error {
	if err := c.stopBox(plannerBox); err != nil {
		return err
	}
	if err := clearTaskMarkers(snap); err != nil {
		return err
	}
	prompt := buildPlannerFeedbackPrompt(readPlannerGoal(snap), c.Project, problems)
	return c.startContainer(plannerBox, snap, c.agentCommand(prompt))
}
