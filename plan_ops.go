package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errPlanConflict reports that merging a task branch into the integration
// branch conflicted. The merge has been aborted; the integration branch is
// unchanged.
var errPlanConflict = errors.New("merge conflict")

func planBranch(planName string) string { return "plan/" + planName }
func taskBranch(taskID string) string   { return "agent/" + taskID }

func branchExists(dir, branch string) bool {
	_, err := gitOut(dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

// ensurePlanBranch creates the integration branch plan/<name> at the project's
// current HEAD if it does not exist yet and returns its name. The project's
// checked-out branch is not changed.
func (c *Config) ensurePlanBranch(planName string) (string, error) {
	branch := planBranch(planName)
	if branchExists(c.Project, branch) {
		return branch, nil
	}
	if _, err := gitOut(c.Project, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return "", fmt.Errorf("%s has no commits; commit something before running a plan", c.Project)
	}
	if err := git(c.Project, "branch", branch); err != nil {
		return "", err
	}
	return branch, nil
}

// requireCleanProject refuses to start task boxes from a project with
// uncommitted tracked changes: the snapshot copies them, and switching it to
// the integration branch could carry them along or fail.
func (c *Config) requireCleanProject() error {
	out, err := gitOut(c.Project, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them before starting plan tasks", c.Project)
	}
	return nil
}

// createTaskBox snapshots the project as box taskID, on a new branch
// agent/<taskID> that starts from the integration branch tip, so it contains
// all previously approved work. It does not start the agent.
func (c *Config) createTaskBox(planName, taskID string) (string, error) {
	if err := c.requireCleanProject(); err != nil {
		return "", err
	}
	branch, err := c.ensurePlanBranch(planName)
	if err != nil {
		return "", err
	}
	return c.createBox(taskID, branch, false)
}

// fetchBoxBranch commits any uncommitted work in the box's snapshot and fetches
// its branch into the project. It returns the branch name.
func (c *Config) fetchBoxBranch(name string) (string, error) {
	snap, err := c.requireSnapshot(name)
	if err != nil {
		return "", err
	}
	return fetchSnapshotBranch(snap, c.Project, name)
}

func fetchSnapshotBranch(snap, project, name string) (string, error) {
	branch, err := gitOut(snap, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if branch == "HEAD" {
		return "", fmt.Errorf("box %q is on a detached HEAD; there is no branch to integrate", name)
	}
	if err := commitIfDirty(snap, "agentbox: "+name); err != nil {
		return "", err
	}
	if err := git(project, "fetch", snap, "+"+branch+":"+branch); err != nil {
		return "", err
	}
	return branch, nil
}

// integrateBranch merges branch into the integration branch plan/<name>. The
// merge runs in a temporary worktree, so the project's checkout and working
// files are never touched. A branch that is already merged is a no-op. On a
// conflict the merge is aborted and errPlanConflict is returned.
func (c *Config) integrateBranch(planName, branch string) (err error) {
	target := planBranch(planName)
	if !branchExists(c.Project, target) {
		return fmt.Errorf("integration branch %s does not exist", target)
	}
	if !branchExists(c.Project, branch) {
		return fmt.Errorf("branch %s does not exist in %s", branch, c.Project)
	}
	if _, aerr := gitOut(c.Project, "merge-base", "--is-ancestor", branch, target); aerr == nil {
		return nil
	}

	tmp, err := os.MkdirTemp("", "agentbox-integrate-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if err := git(c.Project, "worktree", "add", wt, target); err != nil {
		return fmt.Errorf("check out %s: %w", target, err)
	}
	defer func() {
		if rerr := git(c.Project, "worktree", "remove", "--force", wt); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary worktree: %w", rerr))
		}
	}()

	if merr := git(wt, "merge", "--no-edit", branch); merr != nil {
		if abortErr := git(wt, "merge", "--abort"); abortErr != nil {
			return errors.Join(fmt.Errorf("merge of %s into %s failed", branch, target), fmt.Errorf("abort merge: %w", abortErr))
		}
		return fmt.Errorf("%w: %s into %s (aborted)", errPlanConflict, branch, target)
	}
	return nil
}

// integrateBox fetches the box's branch and merges it into the integration
// branch. It keeps the box; the caller removes it once the task is accepted.
func (c *Config) integrateBox(planName, name string) error {
	branch, err := c.fetchBoxBranch(name)
	if err != nil {
		return err
	}
	return c.integrateBranch(planName, branch)
}

// finishPlan merges the integration branch into the project's checked-out
// branch after the human's final review. It refuses a detached HEAD or a dirty
// project and aborts a conflicting merge.
func (c *Config) finishPlan(planName string) (string, error) {
	branch := planBranch(planName)
	if !branchExists(c.Project, branch) {
		return "", fmt.Errorf("integration branch %s does not exist", branch)
	}
	current, err := c.mergePreflight()
	if err != nil {
		return "", err
	}
	if err := c.mergeBranch(branch, current); err != nil {
		return "", err
	}
	return fmt.Sprintf("merged %s into %s", branch, current), nil
}

// finishAndCleanup merges the finished plan and then removes what the plan
// left behind: the merged integration branch and the execution state. The
// committed plan.json stays. Nothing is removed when the merge fails.
func (c *Config) finishAndCleanup(planName string) (string, error) {
	unlock, err := planLock(c.Project)
	if err != nil {
		return "", err
	}
	defer unlock()
	msg, err := c.finishPlan(planName)
	if err != nil {
		return "", err
	}
	msg += "; " + c.deleteMergedBranch(planBranch(planName))
	if err := os.Remove(planStatePath(c.Project)); err != nil && !errors.Is(err, os.ErrNotExist) {
		msg += fmt.Sprintf("; could not remove the plan state: %v", err)
	}
	return msg, nil
}

// planDiffStat summarizes the integration branch against the branch it will be
// merged into, for the final review.
func (c *Config) planDiffStat(planName string) (string, error) {
	branch := planBranch(planName)
	if !branchExists(c.Project, branch) {
		return "", fmt.Errorf("integration branch %s does not exist", branch)
	}
	current, err := gitOut(c.Project, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || current == "" {
		return "", fmt.Errorf("%s is not on a branch", c.Project)
	}
	out, err := gitOut(c.Project, "diff", "--stat", current+"..."+branch)
	return strings.TrimSpace(out), err
}
