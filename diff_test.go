package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSnapshotDiffIncludesCommittedAndWorkingTreeChanges(t *testing.T) {
	dir := initDiffTestRepository(t)
	writeDiffTestFile(t, dir, "tracked.txt", "starting version\n")
	writeDiffTestFile(t, dir, "starting-untracked.txt", "present at start\n")
	runDiffTestGit(t, dir, "add", "tracked.txt")
	runDiffTestGit(t, dir, "commit", "-m", "starting point")
	writeDiffTestFile(t, dir, "tracked.txt", "starting dirty contents\n")

	indexBefore, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := diffTestGitObjects(t, dir)
	if err := recordDiffBaseline(dir); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	indexAfterBaseline, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfterBaseline) {
		t.Fatal("recording baseline changed the Git index")
	}
	if objectsAfter := diffTestGitObjects(t, dir); !slices.Equal(objectsBefore, objectsAfter) {
		t.Fatal("recording baseline changed the project's Git object database")
	}
	baseline, err := os.ReadFile(filepath.Join(diffMetadataDir(filepath.Dir(dir), filepath.Base(dir)), "tree"))
	if err != nil {
		t.Fatal(err)
	}
	baselineTree := strings.TrimSpace(string(baseline))

	writeDiffTestFile(t, dir, "tracked.txt", "committed version\n")
	writeDiffTestFile(t, dir, "committed-only.txt", "committed version\n")
	runDiffTestGit(t, dir, "add", "tracked.txt")
	runDiffTestGit(t, dir, "add", "committed-only.txt")
	runDiffTestGit(t, dir, "commit", "-m", "agent commit")
	writeDiffTestFile(t, dir, "tracked.txt", "unstaged version\n")
	writeDiffTestFile(t, dir, "new-file.txt", "new untracked file\n")
	writeDiffTestFile(t, dir, "starting-untracked.txt", "present at start\n")

	indexBeforeDiff, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	objectsBeforeDiff := diffTestGitObjects(t, dir)
	var diff bytes.Buffer
	changed, err := showSnapshotDiff(dir, baselineTree, &diff)
	if err != nil {
		t.Fatalf("show diff: %v", err)
	}
	if !changed {
		t.Fatal("showSnapshotDiff reported no changes")
	}
	for _, want := range []string{
		"committed version",
		"unstaged version",
		"new untracked file",
		"starting dirty contents",
	} {
		if !strings.Contains(diff.String(), want) {
			t.Errorf("diff output %q does not contain %q", diff.String(), want)
		}
	}
	if strings.Contains(diff.String(), "starting-untracked.txt") || strings.Contains(diff.String(), "starting version") {
		t.Errorf("diff included a file that was already present at session start: %s", diff.String())
	}
	indexAfterDiff, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBeforeDiff, indexAfterDiff) {
		t.Fatal("showing the diff changed the Git index")
	}
	if objectsAfter := diffTestGitObjects(t, dir); !slices.Equal(objectsBeforeDiff, objectsAfter) {
		t.Fatal("showing the diff changed the project's Git object database")
	}
}

func TestSnapshotDiffReportsNoChanges(t *testing.T) {
	dir := initDiffTestRepository(t)
	writeDiffTestFile(t, dir, "tracked.txt", "unchanged\n")
	writeDiffTestFile(t, dir, ".gitignore", "ignored.log\n")
	runDiffTestGit(t, dir, "add", "tracked.txt")
	runDiffTestGit(t, dir, "add", ".gitignore")
	runDiffTestGit(t, dir, "commit", "-m", "starting point")
	if err := recordDiffBaseline(dir); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	writeDiffTestFile(t, dir, "ignored.log", "ignored changes are excluded\n")
	baseline, err := os.ReadFile(filepath.Join(diffMetadataDir(filepath.Dir(dir), filepath.Base(dir)), "tree"))
	if err != nil {
		t.Fatal(err)
	}

	var diff bytes.Buffer
	changed, err := showSnapshotDiff(dir, strings.TrimSpace(string(baseline)), &diff)
	if err != nil {
		t.Fatalf("show diff: %v", err)
	}
	if changed || diff.Len() != 0 {
		t.Fatalf("showSnapshotDiff = (%v, %q), want no changes", changed, diff.String())
	}
}

func TestSessionDiffBaselineRequiresSavedBaseline(t *testing.T) {
	dir := t.TempDir()
	if _, err := sessionDiffBaseline(dir, "older-session"); err == nil ||
		!strings.Contains(err.Error(), "no saved diff baseline") {
		t.Fatalf("sessionDiffBaseline error = %v, want missing-baseline error", err)
	}
}

func initDiffTestRepository(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runDiffTestGit(t, dir, "init", "-q")
	runDiffTestGit(t, dir, "config", "user.name", "Agentbox Test")
	runDiffTestGit(t, dir, "config", "user.email", "agentbox-test@example.com")
	return dir
}

func writeDiffTestFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runDiffTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func diffTestGitObjects(t *testing.T, dir string) []string {
	t.Helper()
	objectsDir := filepath.Join(dir, ".git", "objects")
	var files []string
	err := filepath.WalkDir(objectsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(objectsDir, path)
			if err != nil {
				return err
			}
			files = append(files, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
