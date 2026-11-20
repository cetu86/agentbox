package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitTestFile(t *testing.T, dir, name, contents, msg string) {
	t.Helper()
	writeDiffTestFile(t, dir, name, contents)
	runDiffTestGit(t, dir, "add", name)
	runDiffTestGit(t, dir, "commit", "-q", "-m", msg)
}

func currentTestBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMergeBranchMergesCleanly(t *testing.T) {
	project := initDiffTestRepository(t)
	commitTestFile(t, project, "a.txt", "base\n", "base")
	current := currentTestBranch(t, project)
	runDiffTestGit(t, project, "switch", "-q", "-c", "agent/x")
	commitTestFile(t, project, "b.txt", "from agent\n", "agent work")
	runDiffTestGit(t, project, "switch", "-q", current)

	cfg := &Config{Project: project}
	got, err := cfg.mergePreflight()
	if err != nil || got != current {
		t.Fatalf("mergePreflight() = %q, %v; want %q", got, err, current)
	}
	if err := cfg.mergeBranch("agent/x", current); err != nil {
		t.Fatalf("mergeBranch() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "b.txt")); err != nil {
		t.Fatalf("merged file missing: %v", err)
	}
}

func TestMergeBranchAbortsOnConflict(t *testing.T) {
	project := initDiffTestRepository(t)
	commitTestFile(t, project, "a.txt", "base\n", "base")
	current := currentTestBranch(t, project)
	runDiffTestGit(t, project, "switch", "-q", "-c", "agent/x")
	commitTestFile(t, project, "a.txt", "agent\n", "agent edit")
	runDiffTestGit(t, project, "switch", "-q", current)
	commitTestFile(t, project, "a.txt", "mainline\n", "main edit")

	cfg := &Config{Project: project}
	err := cfg.mergeBranch("agent/x", current)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("mergeBranch() error = %v, want an abort error", err)
	}
	data, readErr := os.ReadFile(filepath.Join(project, "a.txt"))
	if readErr != nil || string(data) != "mainline\n" {
		t.Fatalf("project file = %q, %v; want it untouched", data, readErr)
	}
	if out, _ := gitOut(project, "status", "--porcelain"); out != "" {
		t.Fatalf("project is not clean after aborted merge: %q", out)
	}
}

func TestMergePreflightRefusals(t *testing.T) {
	project := initDiffTestRepository(t)
	commitTestFile(t, project, "a.txt", "base\n", "base")
	cfg := &Config{Project: project}

	writeDiffTestFile(t, project, "a.txt", "dirty\n")
	if _, err := cfg.mergePreflight(); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty worktree error = %v", err)
	}
	runDiffTestGit(t, project, "checkout", "-q", "--", "a.txt")

	// Untracked files do not block a merge.
	writeDiffTestFile(t, project, "untracked.txt", "x\n")
	if _, err := cfg.mergePreflight(); err != nil {
		t.Fatalf("untracked file should be allowed: %v", err)
	}

	runDiffTestGit(t, project, "checkout", "-q", "--detach")
	if _, err := cfg.mergePreflight(); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("detached HEAD error = %v", err)
	}
}

func TestMergeBranchRejectsSameBranch(t *testing.T) {
	cfg := &Config{Project: t.TempDir()}
	if err := cfg.mergeBranch("main", "main"); err == nil {
		t.Fatal("merging a branch into itself should fail")
	}
	if err := cfg.mergeBranch("HEAD", "main"); err == nil {
		t.Fatal("merging a detached box should fail")
	}
}

func TestAcceptBoxRejectsPromoteWithMerge(t *testing.T) {
	cfg := &Config{}
	if _, err := cfg.acceptBox("x", acceptOptions{Promote: true, Merge: true}); err == nil {
		t.Fatal("--promote with --merge should fail")
	}
}

func TestListBoxesMissingSnapshotDir(t *testing.T) {
	cfg := &Config{SnapshotDir: filepath.Join(t.TempDir(), "none")}
	boxes, err := cfg.listBoxes()
	if err != nil || len(boxes) != 0 {
		t.Fatalf("listBoxes() = %v, %v; want empty", boxes, err)
	}
}

func TestCaptureCommandsCollectsOutput(t *testing.T) {
	out, err := captureCommands(func() error { return run("sh", "-c", "echo hello; echo oops >&2") })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+ sh", "hello", "oops"} {
		if !strings.Contains(out, want) {
			t.Fatalf("captured output %q is missing %q", out, want)
		}
	}
	if activeCapture.Load() != nil {
		t.Fatal("capture should be cleared afterwards")
	}
	out, err = captureCommands(func() error { return run("sh", "-c", "echo bad >&2; exit 3") })
	if err == nil || !strings.Contains(out, "bad") {
		t.Fatalf("failing command: err = %v, output %q", err, out)
	}
}
