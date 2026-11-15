package main

import (
	"slices"
	"testing"
)

func TestContainerIsRunning(t *testing.T) {
	for _, test := range []struct {
		status string
		want   bool
	}{
		{status: "Up 2 minutes", want: true},
		{status: "running", want: true},
		{status: "Exited (0) 3 seconds ago", want: false},
		{status: "", want: false},
	} {
		if got := containerIsRunning(test.status); got != test.want {
			t.Errorf("containerIsRunning(%q) = %v, want %v", test.status, got, test.want)
		}
	}
}

func TestContainerArgsManagedAgentKeepsInteractiveContainer(t *testing.T) {
	cfg := &Config{
		Project: "/project",
		Runtime: "podman",
		Image:   "agentbox:test",
		ShmSize: "1g",
		Network: "open",
	}
	args, err := cfg.containerArgs("review", "/snapshots/review", []string{"agent", "task"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-d", "-i", "-t", "--name", "agentbox-review"} {
		if !slices.Contains(args, want) {
			t.Errorf("managed container args %q missing %q", args, want)
		}
	}
	if slices.Contains(args, "--rm") {
		t.Fatal("managed container must remain after the agent exits")
	}
}

func TestContainerArgsShellIsTemporary(t *testing.T) {
	cfg := &Config{
		Project: "/project",
		Runtime: "podman",
		Image:   "agentbox:test",
		ShmSize: "1g",
		Network: "open",
	}
	args, err := cfg.containerArgs("review", "/snapshots/review", []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--rm", "-it"} {
		if !slices.Contains(args, want) {
			t.Errorf("temporary shell args %q missing %q", args, want)
		}
	}
	if slices.Contains(args, "-d") {
		t.Fatal("temporary shell must stay attached")
	}
}
