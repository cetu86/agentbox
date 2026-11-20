package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlannerPrompt(t *testing.T) {
	got := buildPlannerPrompt("add a login page", "/work/proj")
	for _, want := range []string{
		"add a login page",
		"/work/proj/.agentbox/plan.json",
		"/work/proj/.git/agentbox-done",
		"/work/proj/.git/agentbox-blocked",
		"depends_on", "Do NOT implement", "auto_approve",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestBuildPlannerFeedbackPrompt(t *testing.T) {
	got := buildPlannerFeedbackPrompt("goal x", "/p", "duplicate task id \"a\"")
	for _, want := range []string{"goal x", "duplicate task id", "rejected as invalid", "/p/.git/agentbox-done"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestPlannerGoalRoundTrip(t *testing.T) {
	snap := markerSnap(t)
	if err := os.WriteFile(plannerGoalPath(snap), []byte("my goal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readPlannerGoal(snap); got != "my goal" {
		t.Fatalf("goal = %q", got)
	}
}

func TestStartPlannerRefusals(t *testing.T) {
	cfg := &Config{Project: t.TempDir()}
	if err := cfg.startPlanner("  "); err == nil {
		t.Error("empty goal accepted")
	}
	if err := savePlan(cfg.Project, testPlan(t)); err != nil {
		t.Fatal(err)
	}
	err := cfg.startPlanner("goal")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanStateIsGitIgnored(t *testing.T) {
	root := t.TempDir()
	if err := savePlanState(root, newPlanState()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, planDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{planStateName, planLockName} {
		if !strings.Contains(string(data), f) {
			t.Errorf(".gitignore lacks %s: %s", f, data)
		}
	}
}

// Every plan subcommand in the usage text must appear in the README.
func TestReadmeDocumentsPlanSubcommands(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(planUsage, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(line, "Usage") || strings.HasPrefix(line, "Attach") || line[0] != ' ' || strings.HasPrefix(line, "                ") {
			continue
		}
		if !strings.Contains(string(readme), f[0]) {
			t.Errorf("README does not mention plan subcommand %q", f[0])
		}
	}
}
