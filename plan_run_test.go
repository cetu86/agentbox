package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func markerSnap(t *testing.T) string {
	t.Helper()
	snap := t.TempDir()
	if err := os.Mkdir(filepath.Join(snap, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return snap
}

func writeMarker(t *testing.T, snap, name, text string) {
	t.Helper()
	if err := os.WriteFile(markerPath(snap, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadTaskMarker(t *testing.T) {
	snap := markerSnap(t)
	if res, err := readTaskMarker(snap); err != nil || res != (taskResult{}) {
		t.Fatalf("no marker: %+v, %v", res, err)
	}

	writeMarker(t, snap, doneMarkerName, "  added the thing\n")
	res, err := readTaskMarker(snap)
	if err != nil || !res.Done || res.Blocked || res.Summary != "added the thing" {
		t.Fatalf("done marker: %+v, %v", res, err)
	}

	// An empty file still counts as done.
	writeMarker(t, snap, doneMarkerName, "")
	if res, _ := readTaskMarker(snap); !res.Done || res.Summary != "" {
		t.Fatalf("empty done marker: %+v", res)
	}

	// Blocked wins over done.
	writeMarker(t, snap, blockedMarkerName, "no network")
	res, _ = readTaskMarker(snap)
	if res.Done || !res.Blocked || res.Summary != "no network" {
		t.Fatalf("blocked marker: %+v", res)
	}
}

func TestReadTaskMarkerTruncates(t *testing.T) {
	snap := markerSnap(t)
	writeMarker(t, snap, doneMarkerName, strings.Repeat("x", maxMarkerBytes*3))
	res, err := readTaskMarker(snap)
	if err != nil || !res.Done {
		t.Fatalf("%+v, %v", res, err)
	}
	if len(res.Summary) > maxMarkerBytes+8 || !strings.HasSuffix(res.Summary, "…") {
		t.Fatalf("summary not truncated: %d bytes", len(res.Summary))
	}
}

func TestClearTaskMarkers(t *testing.T) {
	snap := markerSnap(t)
	if err := clearTaskMarkers(snap); err != nil {
		t.Fatalf("clearing nothing: %v", err)
	}
	writeMarker(t, snap, doneMarkerName, "x")
	writeMarker(t, snap, blockedMarkerName, "y")
	if err := clearTaskMarkers(snap); err != nil {
		t.Fatal(err)
	}
	if res, _ := readTaskMarker(snap); res != (taskResult{}) {
		t.Fatalf("markers remain: %+v", res)
	}
}

func TestMarkersDoNotAppearInGitStatus(t *testing.T) {
	project := initDiffTestRepository(t)
	commitTestFile(t, project, "a.txt", "x\n", "base")
	writeMarker(t, project, doneMarkerName, "done")
	if out, _ := gitOut(project, "status", "--porcelain"); out != "" {
		t.Fatalf("marker visible to git: %q", out)
	}
}

func TestBuildTaskPrompt(t *testing.T) {
	p := testPlan(t)
	p.Tasks[2].Title = "Combine"
	p.Tasks[0].Title = "Part A"
	prompt := buildTaskPrompt(p, p.task("c"), "/work/proj")

	for _, want := range []string{
		`"demo"`, "Combine (c)", "do c",
		"Already finished", "- Part A (a)", "- b",
		"/work/proj/.git/agentbox-done", "/work/proj/.git/agentbox-blocked",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// d is unrelated to c's parents, so it is listed as another agent's scope.
	if !strings.Contains(prompt, "Stay out of their scope") || !strings.Contains(prompt, "- d") {
		t.Errorf("other tasks missing:\n%s", prompt)
	}

	first := buildTaskPrompt(p, p.task("a"), "/work/proj")
	if strings.Contains(first, "Already finished") {
		t.Errorf("task without dependencies mentions finished work:\n%s", first)
	}
}

func TestBuildFeedbackPrompt(t *testing.T) {
	p := testPlan(t)
	prompt := buildFeedbackPrompt(p, p.task("a"), "/work/proj", "  please add tests\n")
	for _, want := range []string{"do a", "sent it back", "please add tests", "/work/proj/.git/agentbox-done"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestTaskConfigNetworkOverride(t *testing.T) {
	base := &Config{Network: "proxy"}
	if got := base.taskConfig(&planTask{ID: "a"}); got.Network != "proxy" {
		t.Fatalf("network = %q", got.Network)
	}
	if got := base.taskConfig(&planTask{ID: "a", Net: "none"}); got.Network != "none" {
		t.Fatalf("network = %q", got.Network)
	}
	if base.Network != "proxy" {
		t.Fatal("taskConfig modified the original")
	}
}

func TestAgentCommandCarriesMultilinePrompt(t *testing.T) {
	cfg := &Config{AgentCmd: []string{"copilot", "-i", "{task}"}}
	p := testPlan(t)
	prompt := buildTaskPrompt(p, p.task("a"), "/p")
	got := cfg.agentCommand(prompt)
	if len(got) != 3 || got[2] != prompt {
		t.Fatalf("agentCommand = %q", got)
	}
}

func TestUnresolvedFilesAndResolverPrompt(t *testing.T) {
	repo := initDiffTestRepository(t)
	commitTestFile(t, repo, "f.txt", "base\n", "base")
	base := currentTestBranch(t, repo)
	runDiffTestGit(t, repo, "switch", "-c", "other")
	commitTestFile(t, repo, "f.txt", "other\n", "other")
	runDiffTestGit(t, repo, "switch", base)
	commitTestFile(t, repo, "f.txt", "mine\n", "mine")

	if files, err := unresolvedFiles(repo); err != nil || len(files) != 0 {
		t.Fatalf("clean tree: %v, %v", files, err)
	}
	if err := git(repo, "merge", "--no-edit", "other"); err == nil {
		t.Fatal("expected a conflict")
	}
	files, err := unresolvedFiles(repo)
	if err != nil || len(files) != 1 || files[0] != "f.txt" {
		t.Fatalf("unresolved = %v, %v", files, err)
	}

	p := testPlan(t)
	prompt := buildResolverPrompt(p, p.task("a"), "/work/proj", files)
	for _, want := range []string{"plan/demo", "- f.txt", "git commit --no-edit", "do a", "/work/proj/.git/agentbox-done"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("resolver prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestMaxParallelDefault(t *testing.T) {
	cfg := &Config{Project: "/p"}
	applyConfigDefaults(cfg)
	if cfg.MaxParallel != defaultMaxParallel {
		t.Fatalf("MaxParallel = %d", cfg.MaxParallel)
	}
	cfg = &Config{Project: "/p", MaxParallel: 5}
	applyConfigDefaults(cfg)
	if cfg.MaxParallel != 5 {
		t.Fatalf("MaxParallel = %d", cfg.MaxParallel)
	}
}
