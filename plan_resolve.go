package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// stopBox force-stops the box's agent and shell containers but keeps its
// snapshot. Interactive agents keep running after they finish, so approving a
// task stops the agent before its work is collected.
func (c *Config) stopBox(name string) error {
	status, err := c.containerStatus(name)
	if err != nil {
		return err
	}
	if status != "" {
		if err := run(c.Runtime, "rm", "-f", c.containerName(name)); err != nil {
			return err
		}
	}
	return c.removeShell(name)
}

// removeTaskBox deletes a task's box and its branch so the task can start from
// scratch. Missing pieces are fine.
func (c *Config) removeTaskBox(taskID string) error {
	if _, err := c.requireSnapshot(taskID); err == nil {
		if err := c.discardBox(taskID); err != nil {
			return err
		}
	}
	if branchExists(c.Project, taskBranch(taskID)) {
		return git(c.Project, "branch", "-D", taskBranch(taskID))
	}
	return nil
}

// unresolvedFiles lists the files with unresolved merge conflicts in dir.
func unresolvedFiles(dir string) ([]string, error) {
	out, err := gitOut(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// integrateTask stops the task's agent, collects its work and merges it into
// the integration branch. It refuses a box that still has unresolved conflicts
// from a resolver run. A conflict is reported as errPlanConflict.
func (c *Config) integrateTask(planName, taskID string) error {
	snap, err := c.requireSnapshot(taskID)
	if err != nil {
		return err
	}
	if err := c.stopBox(taskID); err != nil {
		return err
	}
	files, err := unresolvedFiles(snap)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return fmt.Errorf("unresolved conflicts remain in %s", strings.Join(files, ", "))
	}
	return c.integrateBox(planName, taskID)
}

// startResolver prepares the task's own box for conflict resolution: the
// integration branch is merged into the task branch, leaving the conflicts in
// the working tree, and a fresh agent is started to resolve them. It reports
// false when that merge turned out to be clean and no agent is needed.
func (c *Config) startResolver(p *plan, t *planTask) (bool, error) {
	cfg := c.taskConfig(t)
	snap, err := cfg.requireSnapshot(t.ID)
	if err != nil {
		return false, err
	}
	if err := cfg.stopBox(t.ID); err != nil {
		return false, err
	}
	if err := commitIfDirty(snap, "agentbox: "+t.ID); err != nil {
		return false, err
	}
	if err := git(snap, "fetch", cfg.Project, planBranch(p.Name)); err != nil {
		return false, err
	}
	if err := git(snap, "merge", "--no-edit", "FETCH_HEAD"); err == nil {
		return false, nil
	}
	files, ferr := unresolvedFiles(snap)
	if ferr != nil {
		return false, ferr
	}
	if len(files) == 0 {
		return false, fmt.Errorf("merging %s into the box of task %q failed without conflicts", planBranch(p.Name), t.ID)
	}
	if err := clearTaskMarkers(snap); err != nil {
		return false, err
	}
	prompt := buildResolverPrompt(p, t, cfg.Project, files)
	return true, cfg.startContainer(t.ID, snap, cfg.agentCommand(prompt))
}

func buildResolverPrompt(p *plan, t *planTask, project string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a conflict resolver in an automated plan called %q. The task %s was finished, but merging it into the integration branch %s conflicted.\n\n",
		p.Name, taskLabel(t), planBranch(p.Name))
	fmt.Fprintf(&b, "The merge is in progress in your checkout. Conflicted files:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	b.WriteString("\nResolve every conflict so that both the intent of this task and the work already on the integration branch are kept. ")
	b.WriteString("Remove all conflict markers, make sure the project still builds and its tests pass, then stage the files and complete the merge with `git commit --no-edit`. Do not change anything unrelated.\n\n")
	b.WriteString("This task was:\n")
	b.WriteString(strings.TrimSpace(t.Prompt))
	b.WriteString("\n\n")
	b.WriteString(completionProtocol(project))
	return b.String()
}

// planLock serializes plan operations across processes (the CLI, `plan run`
// and the TUI all read and rewrite the same state file).
func planLock(root string) (unlock func(), err error) {
	dir := filepath.Join(root, planDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, planLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
