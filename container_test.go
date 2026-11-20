package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func TestContainerStatusReportsUnavailableDockerDaemon(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' 'failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory' >&2\nexit 1\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Runtime: runtimePath}
	_, err := cfg.containerStatus("review")
	if err == nil {
		t.Fatal("expected unavailable Docker daemon error")
	}
	for _, want := range []string{"Docker daemon is not reachable", "start Docker and retry", "connect: no such file or directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("containerStatus error %q does not contain %q", err, want)
		}
	}
}

func TestContainerStatusIncludesRuntimeErrorOutput(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf '%s\\n' 'invalid filter' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Runtime: runtimePath}
	_, err := cfg.containerStatus("review")
	if err == nil {
		t.Fatal("expected runtime error")
	}
	for _, want := range []string{"invalid filter", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("containerStatus error %q does not contain %q", err, want)
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
	if slices.Contains(args, "--entrypoint") {
		t.Fatal("non-Copilot command unexpectedly received the Copilot bootstrap")
	}
}

func TestContainerArgsPretrustCopilotProject(t *testing.T) {
	cfg := &Config{
		Project: "/project",
		Runtime: "podman",
		Image:   "agentbox:test",
		ShmSize: "1g",
		Network: "open",
	}
	args, err := cfg.containerArgs("review", "/snapshots/review", []string{"copilot", "--allow-all-tools"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--entrypoint", "/bin/sh",
		copilotBootstrapScript,
		`{"trustedFolders":["/project"]}`,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("Copilot container args %q missing %q", args, want)
		}
	}
	if got := args[len(args)-2:]; !slices.Equal(got, []string{"copilot", "--allow-all-tools"}) {
		t.Fatalf("bootstrapped Copilot command = %q, want original command and arguments", got)
	}
}

func TestCopilotBootstrapWritesTrustedProjectConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := `{"trustedFolders":["/project"]}`
	command := exec.Command(
		"/bin/sh", "-c", copilotBootstrapScript,
		"agentbox-copilot-bootstrap", config, "true",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run Copilot bootstrap: %v\n%s", err, output)
	}
	got, err := os.ReadFile(filepath.Join(home, ".copilot", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != config {
		t.Fatalf("Copilot config = %q, want %q", got, config)
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

func TestContainerArgsPassConfiguredGitHubTokenWithoutPuttingItInArgv(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-token")
	t.Setenv("GITHUB_TOKEN", "another-host-token")
	cfg := &Config{
		Project:     "/project",
		Runtime:     "podman",
		Image:       "agentbox:test",
		ShmSize:     "1g",
		Network:     "open",
		Env:         []string{"GH_TOKEN", "GITHUB_TOKEN"},
		GitHubToken: "configured-token",
	}
	args, err := cfg.containerArgs("review", "/snapshots/review", []string{"agent"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if !hasContainerEnvArg(args, name) {
			t.Errorf("container args %q do not pass %s", args, name)
		}
	}
	if slices.Contains(args, "configured-token") || slices.Contains(args, "host-token") || slices.Contains(args, "another-host-token") {
		t.Fatalf("token value appeared in runtime arguments: %q", args)
	}
}

func TestContainerArgsKeepHostGitHubTokenPassthrough(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-token")
	cfg := &Config{
		Project: "/project",
		Runtime: "podman",
		Image:   "agentbox:test",
		ShmSize: "1g",
		Network: "open",
		Env:     []string{"GH_TOKEN"},
	}
	args, err := cfg.containerArgs("review", "/snapshots/review", []string{"agent"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContainerEnvArg(args, "GH_TOKEN") {
		t.Fatalf("container args %q do not preserve host GH_TOKEN passthrough", args)
	}
	if slices.Contains(args, "host-token") {
		t.Fatalf("host token value appeared in runtime arguments: %q", args)
	}
}

func TestEnvironmentWithOverridesReplacesHostTokenValues(t *testing.T) {
	got := environmentWithOverrides(
		[]string{"PATH=/bin", "GH_TOKEN=host-token", "GITHUB_TOKEN=old-token"},
		map[string]string{"GH_TOKEN": "configured-token", "GITHUB_TOKEN": "configured-token"},
	)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		count := 0
		for _, entry := range got {
			if strings.HasPrefix(entry, name+"=") {
				count++
				if entry != name+"=configured-token" {
					t.Errorf("%s environment entry = %q", name, entry)
				}
			}
		}
		if count != 1 {
			t.Errorf("%s environment entry count = %d, want 1", name, count)
		}
	}
}

func TestRunContainerRuntimeUsesConfiguredTokenEnvironment(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime")
	outputPath := filepath.Join(dir, "token-env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GH_TOKEN\" \"$GITHUB_TOKEN\" > \"$TOKEN_OUTPUT\"\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_OUTPUT", outputPath)
	t.Setenv("GH_TOKEN", "host-token")
	t.Setenv("GITHUB_TOKEN", "host-github-token")

	cfg := &Config{Runtime: runtimePath, GitHubToken: "configured-token"}
	if err := cfg.runContainerRuntime("run"); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(output), "configured-token\nconfigured-token\n"; got != want {
		t.Fatalf("runtime received token environment %q, want %q", got, want)
	}
}

func TestAttachContainerPreservesOtherRuntimeErrors(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf 'cannot connect\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Runtime: runtimePath}
	if err := cfg.attachContainer("review"); err == nil {
		t.Fatal("expected runtime error to be returned")
	}
}

func hasContainerEnvArg(args []string, name string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && args[i+1] == name {
			return true
		}
	}
	return false
}

func TestShellContainerArgs(t *testing.T) {
	cfg := &Config{Runtime: "podman", Project: "/work/proj", Image: "img", ShmSize: "1g", Network: "open"}
	if got := cfg.shellContainerName("feat"); got != "agentbox-feat.shell" {
		t.Fatalf("shell container name = %q", got)
	}
	args, err := cfg.containerArgsNamed("feat", "/snap/feat", []string{"bash"}, true, cfg.shellContainerName("feat"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-d -i -t --name agentbox-feat.shell", "-v /snap/feat:/work/proj", "--hostname feat"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if args[len(args)-2] != "img" || args[len(args)-1] != "bash" {
		t.Errorf("args should end with image and bash: %q", joined)
	}
}
