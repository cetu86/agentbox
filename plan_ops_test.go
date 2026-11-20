package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planTestProject(t *testing.T) (*Config, string) {
	t.Helper()
	project := initDiffTestRepository(t)
	commitTestFile(t, project, "a.txt", "base\n", "base")
	return &Config{Project: project}, currentTestBranch(t, project)
}

// cloneBox mimics a box: a copy of the project whose branch agent/<task>
// starts at the integration branch.
func cloneBox(t *testing.T, project, planName, task string) string {
	t.Helper()
	snap := filepath.Join(t.TempDir(), task)
	runDiffTestGit(t, filepath.Dir(snap), "clone", "-q", project, snap)
	runDiffTestGit(t, snap, "config", "user.name", "Agentbox Test")
	runDiffTestGit(t, snap, "config", "user.email", "agentbox-test@example.com")
	runDiffTestGit(t, snap, "switch", "-q", "-c", taskBranch(task), "--no-track", "origin/"+planBranch(planName))
	return snap
}

func TestEnsurePlanBranch(t *testing.T) {
	cfg, current := planTestProject(t)
	branch, err := cfg.ensurePlanBranch("demo")
	if err != nil || branch != "plan/demo" {
		t.Fatalf("ensurePlanBranch() = %q, %v", branch, err)
	}
	if got := currentTestBranch(t, cfg.Project); got != current {
		t.Fatalf("checked-out branch changed to %q", got)
	}
	// An existing branch is kept as it is.
	runDiffTestGit(t, cfg.Project, "switch", "-q", "plan/demo")
	commitTestFile(t, cfg.Project, "p.txt", "plan\n", "plan work")
	runDiffTestGit(t, cfg.Project, "switch", "-q", current)
	before, _ := gitOut(cfg.Project, "rev-parse", "plan/demo")
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	if after, _ := gitOut(cfg.Project, "rev-parse", "plan/demo"); after != before {
		t.Fatalf("existing branch moved: %s -> %s", before, after)
	}
}

func TestEnsurePlanBranchNoCommits(t *testing.T) {
	cfg := &Config{Project: initDiffTestRepository(t)}
	if _, err := cfg.ensurePlanBranch("demo"); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequireCleanProject(t *testing.T) {
	cfg, _ := planTestProject(t)
	if err := cfg.requireCleanProject(); err != nil {
		t.Fatal(err)
	}
	writeDiffTestFile(t, cfg.Project, "a.txt", "dirty\n")
	if err := cfg.requireCleanProject(); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error = %v", err)
	}
}

func TestIntegrateBoxesInOrderAndDependentsSeeParents(t *testing.T) {
	cfg, current := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}

	// Two independent tasks branch from the same integration tip.
	snapA := cloneBox(t, cfg.Project, "demo", "a")
	snapB := cloneBox(t, cfg.Project, "demo", "b")
	writeDiffTestFile(t, snapA, "a-file.txt", "from a\n") // uncommitted: fetch commits it
	commitTestFile(t, snapB, "b-file.txt", "from b\n", "b work")

	for _, tc := range []struct{ task, snap string }{{"a", snapA}, {"b", snapB}} {
		branch, err := fetchSnapshotBranch(tc.snap, cfg.Project, tc.task)
		if err != nil || branch != taskBranch(tc.task) {
			t.Fatalf("fetchSnapshotBranch(%s) = %q, %v", tc.task, branch, err)
		}
		if err := cfg.integrateBranch("demo", branch); err != nil {
			t.Fatalf("integrateBranch(%s) = %v", tc.task, err)
		}
	}

	for _, f := range []string{"a-file.txt", "b-file.txt"} {
		if _, err := gitOut(cfg.Project, "cat-file", "-e", "plan/demo:"+f); err != nil {
			t.Fatalf("plan/demo lacks %s: %v", f, err)
		}
	}
	// The project's own checkout and files are untouched.
	if got := currentTestBranch(t, cfg.Project); got != current {
		t.Fatalf("checked-out branch changed to %q", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.Project, "a-file.txt")); !os.IsNotExist(err) {
		t.Fatalf("project working tree was modified: %v", err)
	}
	if out, _ := gitOut(cfg.Project, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Fatalf("temporary worktree leaked:\n%s", out)
	}

	// A dependent starts from the integration tip and sees both parents.
	snapC := cloneBox(t, cfg.Project, "demo", "c")
	for _, f := range []string{"a-file.txt", "b-file.txt"} {
		if _, err := os.Stat(filepath.Join(snapC, f)); err != nil {
			t.Fatalf("dependent box lacks %s: %v", f, err)
		}
	}
}

func TestIntegrateBranchIdempotent(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	snap := cloneBox(t, cfg.Project, "demo", "a")
	commitTestFile(t, snap, "a-file.txt", "a\n", "a work")
	branch, err := fetchSnapshotBranch(snap, cfg.Project, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.integrateBranch("demo", branch); err != nil {
		t.Fatal(err)
	}
	tip, _ := gitOut(cfg.Project, "rev-parse", "plan/demo")
	if err := cfg.integrateBranch("demo", branch); err != nil {
		t.Fatalf("second integrate = %v", err)
	}
	if again, _ := gitOut(cfg.Project, "rev-parse", "plan/demo"); again != tip {
		t.Fatal("repeat integration created a new commit")
	}
}

func TestIntegrateBranchConflictIsAborted(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	snapA := cloneBox(t, cfg.Project, "demo", "a")
	snapB := cloneBox(t, cfg.Project, "demo", "b")
	commitTestFile(t, snapA, "a.txt", "from a\n", "a edit")
	commitTestFile(t, snapB, "a.txt", "from b\n", "b edit")

	for _, tc := range []struct{ task, snap string }{{"a", snapA}, {"b", snapB}} {
		branch, err := fetchSnapshotBranch(tc.snap, cfg.Project, tc.task)
		if err != nil {
			t.Fatal(err)
		}
		err = cfg.integrateBranch("demo", branch)
		if tc.task == "a" && err != nil {
			t.Fatal(err)
		}
		if tc.task == "b" && !errors.Is(err, errPlanConflict) {
			t.Fatalf("integrate b error = %v, want errPlanConflict", err)
		}
	}

	if out, _ := gitOut(cfg.Project, "show", "plan/demo:a.txt"); out != "from a" {
		t.Fatalf("plan/demo a.txt = %q; integration branch must be unchanged by the failed merge", out)
	}
	if out, _ := gitOut(cfg.Project, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Fatalf("temporary worktree leaked:\n%s", out)
	}
}

func TestIntegrateBranchMissingBranches(t *testing.T) {
	cfg, _ := planTestProject(t)
	if err := cfg.integrateBranch("demo", "agent/x"); err == nil || !strings.Contains(err.Error(), "integration branch") {
		t.Fatalf("error = %v", err)
	}
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.integrateBranch("demo", "agent/x"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error = %v", err)
	}
}

func TestFinishPlan(t *testing.T) {
	cfg, current := planTestProject(t)
	if _, err := cfg.finishPlan("demo"); err == nil {
		t.Fatal("expected missing-branch error")
	}
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	snap := cloneBox(t, cfg.Project, "demo", "a")
	commitTestFile(t, snap, "a-file.txt", "a\n", "a work")
	branch, err := fetchSnapshotBranch(snap, cfg.Project, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.integrateBranch("demo", branch); err != nil {
		t.Fatal(err)
	}

	stat, err := cfg.planDiffStat("demo")
	if err != nil || !strings.Contains(stat, "a-file.txt") {
		t.Fatalf("planDiffStat = %q, %v", stat, err)
	}

	msg, err := cfg.finishPlan("demo")
	if err != nil || !strings.Contains(msg, "plan/demo into "+current) {
		t.Fatalf("finishPlan = %q, %v", msg, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Project, "a-file.txt")); err != nil {
		t.Fatalf("final merge did not reach the project: %v", err)
	}
}

func TestFinishPlanRefusesDirtyProject(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	writeDiffTestFile(t, cfg.Project, "a.txt", "dirty\n")
	if _, err := cfg.finishPlan("demo"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error = %v", err)
	}
}

func TestFetchSnapshotBranchDetachedHead(t *testing.T) {
	cfg, _ := planTestProject(t)
	snap := filepath.Join(t.TempDir(), "s")
	runDiffTestGit(t, filepath.Dir(snap), "clone", "-q", cfg.Project, snap)
	runDiffTestGit(t, snap, "checkout", "-q", "--detach")
	if _, err := fetchSnapshotBranch(snap, cfg.Project, "s"); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("error = %v", err)
	}
}

func TestFinishAndCleanupRemovesBranchAndState(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	snap := cloneBox(t, cfg.Project, "demo", "a")
	commitTestFile(t, snap, "a-file.txt", "a\n", "a work")
	branch, err := fetchSnapshotBranch(snap, cfg.Project, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.integrateBranch("demo", branch); err != nil {
		t.Fatal(err)
	}
	if err := savePlanState(cfg.Project, newPlanState()); err != nil {
		t.Fatal(err)
	}

	msg, err := cfg.finishAndCleanup("demo")
	if err != nil || !strings.Contains(msg, "deleted branch plan/demo") {
		t.Fatalf("finishAndCleanup = %q, %v", msg, err)
	}
	if branchExists(cfg.Project, "plan/demo") {
		t.Error("integration branch was not deleted")
	}
	if _, err := os.Stat(planStatePath(cfg.Project)); !os.IsNotExist(err) {
		t.Errorf("plan state still exists: %v", err)
	}
}

func TestFinishAndCleanupKeepsEverythingOnFailure(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	if err := savePlanState(cfg.Project, newPlanState()); err != nil {
		t.Fatal(err)
	}
	writeDiffTestFile(t, cfg.Project, "a.txt", "dirty\n")
	if _, err := cfg.finishAndCleanup("demo"); err == nil {
		t.Fatal("expected error")
	}
	if !branchExists(cfg.Project, "plan/demo") {
		t.Error("integration branch was deleted despite the failure")
	}
	if _, err := os.Stat(planStatePath(cfg.Project)); err != nil {
		t.Errorf("plan state was removed: %v", err)
	}
}

func TestDeleteMergedBranchRefusesUnmerged(t *testing.T) {
	cfg, _ := planTestProject(t)
	if _, err := cfg.ensurePlanBranch("demo"); err != nil {
		t.Fatal(err)
	}
	snap := cloneBox(t, cfg.Project, "demo", "a")
	commitTestFile(t, snap, "a-file.txt", "a\n", "a work")
	branch, err := fetchSnapshotBranch(snap, cfg.Project, "a")
	if err != nil {
		t.Fatal(err)
	}
	if msg := cfg.deleteMergedBranch(branch); !strings.Contains(msg, "could not delete") || !branchExists(cfg.Project, branch) {
		t.Fatalf("unmerged branch handled wrongly: %q", msg)
	}
	if err := git(cfg.Project, "merge", "--no-edit", branch); err != nil {
		t.Fatal(err)
	}
	if msg := cfg.deleteMergedBranch(branch); !strings.Contains(msg, "deleted branch") || branchExists(cfg.Project, branch) {
		t.Fatalf("merged branch not deleted: %q", msg)
	}
}
