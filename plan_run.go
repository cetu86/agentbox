package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The agent reports its outcome by writing a file. The files live inside the
// snapshot's .git directory so they are visible to the host, never show up in
// `git status`, and cannot be committed by accident.
const (
	doneMarkerName    = "agentbox-done"
	blockedMarkerName = "agentbox-blocked"

	maxMarkerBytes = 4096
)

// taskResult is what the agent reported through its marker files.
type taskResult struct {
	Done    bool
	Blocked bool   // the agent says it cannot complete the task
	Summary string // the agent's own words (summary or reason), possibly truncated
}

func markerPath(snapOrProject, name string) string {
	return filepath.Join(snapOrProject, ".git", name)
}

// readTaskMarker inspects a snapshot for the agent's markers. The blocked
// marker wins if both exist. A missing marker is not an error.
func readTaskMarker(snap string) (taskResult, error) {
	for _, m := range []struct {
		name    string
		blocked bool
	}{{blockedMarkerName, true}, {doneMarkerName, false}} {
		text, found, err := readMarker(markerPath(snap, m.name))
		if err != nil {
			return taskResult{}, err
		}
		if found {
			return taskResult{Done: !m.blocked, Blocked: m.blocked, Summary: text}, nil
		}
	}
	return taskResult{}, nil
}

func readMarker(path string) (text string, found bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes+1))
	if err != nil {
		return "", false, err
	}
	text = strings.TrimSpace(string(data))
	if len(data) > maxMarkerBytes {
		text = strings.TrimSpace(string(data[:maxMarkerBytes])) + " …"
	}
	return text, true, nil
}

// clearTaskMarkers removes stale markers, e.g. before an agent is restarted
// with feedback.
func clearTaskMarkers(snap string) error {
	for _, name := range []string{doneMarkerName, blockedMarkerName} {
		if err := os.Remove(markerPath(snap, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func taskLabel(t *planTask) string {
	if t.Title != "" {
		return fmt.Sprintf("%s (%s)", t.Title, t.ID)
	}
	return t.ID
}

// buildTaskPrompt renders the instructions handed to the agent. project is the
// project path as seen inside the container, where the marker files are written.
func buildTaskPrompt(p *plan, t *planTask, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are one agent in an automated plan called %q. Your task: %s\n\n", p.Name, taskLabel(t))
	b.WriteString(strings.TrimSpace(t.Prompt))
	b.WriteString("\n\n")

	if len(t.DependsOn) > 0 {
		b.WriteString("Already finished and included in your checkout (build on it, do not redo it):\n")
		for _, d := range t.DependsOn {
			if dep := p.task(d); dep != nil {
				fmt.Fprintf(&b, "- %s\n", taskLabel(dep))
			}
		}
		b.WriteString("\n")
	}
	var others []string
	for i := range p.Tasks {
		if p.Tasks[i].ID != t.ID && !slices.Contains(t.DependsOn, p.Tasks[i].ID) {
			others = append(others, "- "+taskLabel(&p.Tasks[i]))
		}
	}
	if len(others) > 0 {
		b.WriteString("Other tasks in this plan, handled by other agents. Stay out of their scope to avoid merge conflicts:\n")
		b.WriteString(strings.Join(others, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString(completionProtocol(project))
	return b.String()
}

// buildFeedbackPrompt is used to restart a task after the human rejected it.
func buildFeedbackPrompt(p *plan, t *planTask, project, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are one agent in an automated plan called %q. Your task: %s\n\n", p.Name, taskLabel(t))
	b.WriteString(strings.TrimSpace(t.Prompt))
	b.WriteString("\n\nYou already worked on this and the reviewer sent it back. The earlier work is in your checkout. Reviewer feedback:\n\n")
	b.WriteString(strings.TrimSpace(feedback))
	b.WriteString("\n\n")
	b.WriteString(completionProtocol(project))
	return b.String()
}

func completionProtocol(project string) string {
	done := filepath.Join(project, ".git", doneMarkerName)
	blocked := filepath.Join(project, ".git", blockedMarkerName)
	return fmt.Sprintf(`When you have finished, and only then:
1. Make sure the work is complete and your changes are saved (committing is optional; uncommitted changes are committed for you).
2. Write a short summary of what you did to the file %s
   (for example: echo "summary..." > %s)
   This file is how the orchestrator learns you are done. Do not create it earlier.

If you cannot complete the task, write the reason to %s instead and stop.
`, done, done, blocked)
}

// taskConfig returns a copy of the config with the task's network override.
func (c *Config) taskConfig(t *planTask) *Config {
	cfg := *c
	if t.Net != "" {
		cfg.Network = t.Net
	}
	return &cfg
}

// startTask creates the task's box from the integration branch and starts its
// agent detached with the generated prompt. It returns the snapshot path.
func (c *Config) startTask(p *plan, t *planTask) (string, error) {
	cfg := c.taskConfig(t)
	snap, err := cfg.createTaskBox(p.Name, t.ID)
	if err != nil {
		return "", err
	}
	prompt := buildTaskPrompt(p, t, cfg.Project)
	if err := cfg.startContainer(t.ID, snap, cfg.agentCommand(prompt)); err != nil {
		return snap, err
	}
	return snap, nil
}

// restartTask starts a stopped task's agent again with the reviewer's feedback.
// The task's snapshot and its earlier work are kept; stale markers are cleared.
func (c *Config) restartTask(p *plan, t *planTask, feedback string) error {
	cfg := c.taskConfig(t)
	snap, err := cfg.requireSnapshot(t.ID)
	if err != nil {
		return err
	}
	status, err := cfg.containerStatus(t.ID)
	if err != nil {
		return err
	}
	if containerIsRunning(status) {
		return fmt.Errorf("agent for task %q is still running", t.ID)
	}
	if err := clearTaskMarkers(snap); err != nil {
		return err
	}
	prompt := buildFeedbackPrompt(p, t, cfg.Project, feedback)
	return cfg.startContainer(t.ID, snap, cfg.agentCommand(prompt))
}

// taskOutcome inspects a task's box. agentRunning tells whether the agent
// container is still up; stopped is true when the container has exited (or was
// never found) without writing a marker, which the orchestrator surfaces so the
// human can mark the task done manually.
type taskOutcome struct {
	taskResult
	AgentRunning bool
	Stopped      bool // container gone/exited and no marker was written
}

func (c *Config) checkTask(t *planTask) (taskOutcome, error) {
	snap, err := c.requireSnapshot(t.ID)
	if err != nil {
		return taskOutcome{}, err
	}
	res, err := readTaskMarker(snap)
	if err != nil {
		return taskOutcome{}, err
	}
	status, err := c.containerStatus(t.ID)
	if err != nil {
		return taskOutcome{}, err
	}
	running := containerIsRunning(status)
	return taskOutcome{taskResult: res, AgentRunning: running, Stopped: !running && !res.Done && !res.Blocked}, nil
}
