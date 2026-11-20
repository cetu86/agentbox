package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestQEMUWorkflow(t *testing.T) {
	if os.Getenv("AGENTBOX_QEMU_GUEST") != "1" {
		t.Skip("requires the QEMU integration guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("workflow integration must run as root")
	}

	const root = "/mnt/test/workflow"
	project := filepath.Join(root, "project")
	snapshotDir := filepath.Join(root, ".agent-snaps")
	runtimeState := filepath.Join(root, "runtime")
	configHome := filepath.Join(root, "config")
	gitConfig := filepath.Join(root, "gitconfig")

	for key, value := range map[string]string{
		"AGENTBOX_TEST_RUNTIME_STATE": runtimeState,
		"XDG_CONFIG_HOME":             configHome,
		"GIT_CONFIG_GLOBAL":           gitConfig,
		"HOME":                        "/root",
	} {
		if err := os.Setenv(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runQEMUCommand(t, "btrfs", "subvolume", "create", project)
	copyQEMUProject(t, "/qemu/examples/qemu-project", project)
	runQEMUCommand(t, "git", "-C", project, "init", "-q")
	runQEMUCommand(t, "git", "-C", project, "config", "user.name", "Agentbox QEMU Test")
	runQEMUCommand(t, "git", "-C", project, "config", "user.email", "agentbox-qemu@example.invalid")
	runQEMUCommand(t, "git", "-C", project, "add", ".")
	runQEMUCommand(t, "git", "-C", project, "commit", "-qm", "initial project")
	if err := os.WriteFile(gitConfig, []byte("[safe]\n\tdirectory = *\n[user]\n\tname = Agentbox QEMU Test\n\temail = agentbox-qemu@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runQEMUCommandDir(t, project, "/agentbox", "init")
	projectConfigPath, err := configPath(project)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{
  "project": %q,
  "snapshot_dir": %q,
  "runtime": "/qemu/runtime",
  "image": "agentbox-qemu-dummy",
  "agent_cmd": ["/qemu/runtime", "{task}"],
  "shm_size": "64m",
  "network": "none"
}
`, project, snapshotDir)
	if err := os.WriteFile(projectConfigPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	runQEMUCommandDir(t, project, "/agentbox", "build")

	t.Log("checking interactive start, watch, shell, attach, list, and accept")
	fmt.Println("QEMU workflow: start interactive agent")
	start := startQEMUCommandPTY(t, project, "/agentbox", "start", "interactive", "interactive")
	start.waitFor(t, "DUMMY_AGENT_READY")
	fmt.Println("QEMU workflow: respond and detach")
	start.write(t, "approve staging\n")
	start.waitFor(t, "DUMMY_AGENT_ANSWER=approve staging")
	start.write(t, "\x1b[21~")
	start.waitExit(t)

	fmt.Println("QEMU workflow: inspect, watch, and open shell")
	snapshot := filepath.Join(snapshotDir, "interactive")
	if got := strings.TrimSpace(runQEMUCommandDir(t, project, "/agentbox", "path", "interactive")); got != snapshot {
		t.Fatalf("snapshot path = %q, want %q", got, snapshot)
	}
	fmt.Println("QEMU workflow: path checked; checking list")
	if got := runQEMUCommandDir(t, project, "/agentbox", "list"); !strings.Contains(got, "interactive") || !strings.Contains(got, "Up ") {
		t.Fatalf("running session missing from list:\n%s", got)
	}
	fmt.Println("QEMU workflow: list checked; watching logs")
	watchOutput := watchQEMUSession(t, project, "interactive")
	if !strings.Contains(watchOutput, "DUMMY_AGENT_ANSWER=approve staging") {
		t.Fatalf("watch output did not include agent response:\n%s", watchOutput)
	}

	fmt.Println("QEMU workflow: logs checked; starting shell")
	shell := startQEMUCommandPTY(t, project, "/agentbox", "shell", "interactive")
	shell.waitFor(t, "# ")
	shell.write(t, "printf 'DUMMY_SHELL_OK\\n'\n")
	shell.waitFor(t, "DUMMY_SHELL_OK\r\n")
	shell.write(t, "exit\n")
	shell.waitExit(t)

	fmt.Println("QEMU workflow: reattach and accept")
	attach := startQEMUCommandPTY(t, project, "/agentbox", "attach", "interactive")
	attach.waitFor(t, "+ /qemu/runtime attach agentbox-interactive")
	attach.write(t, "finish\n")
	attach.waitFor(t, "DUMMY_AGENT_CONTINUING")
	attach.waitExit(t)
	if got := runQEMUCommandDir(t, project, "/agentbox", "list"); !strings.Contains(got, "interactive") || !strings.Contains(got, "Exited ") {
		t.Fatalf("completed session missing exited state from list:\n%s", got)
	}
	runQEMUCommandDir(t, project, "/agentbox", "accept", "interactive")
	branchResult := runQEMUCommand(t, "git", "-C", project, "show", "agent/interactive:agent-result.txt")
	if !strings.Contains(branchResult, "approved: approve staging") {
		t.Fatalf("accepted Git branch has unexpected result: %q", branchResult)
	}
	requireQEMUPathAbsent(t, snapshot)
	requireQEMUPathAbsent(t, filepath.Join(runtimeState, "agentbox-interactive"))

	t.Log("checking resume and discard")
	fmt.Println("QEMU workflow: start, resume, and discard")
	firstRun := startQEMUCommandPTY(t, project, "/agentbox", "start", "resume-check", "finish")
	firstRun.waitFor(t, "DUMMY_AGENT_FINISHED")
	firstRun.waitExit(t)
	requireQEMURunCount(t, filepath.Join(snapshotDir, "resume-check", "agent-runs"), "1")
	resumedRun := startQEMUCommandPTY(t, project, "/agentbox", "resume", "resume-check", "finish")
	resumedRun.waitFor(t, "DUMMY_AGENT_FINISHED")
	resumedRun.waitExit(t)
	requireQEMURunCount(t, filepath.Join(snapshotDir, "resume-check", "agent-runs"), "2")
	runQEMUCommandDir(t, project, "/agentbox", "discard", "resume-check")
	requireQEMUPathAbsent(t, filepath.Join(snapshotDir, "resume-check"))
	requireQEMUPathAbsent(t, filepath.Join(runtimeState, "agentbox-resume-check"))

	fmt.Println("QEMU workflow: finish project")
	finishOutput := runQEMUCommandDir(t, project, "/agentbox", "finish")
	if !strings.Contains(finishOutput, ".agentbox-finish-") {
		t.Fatalf("finish did not report a retained subvolume backup:\n%s", finishOutput)
	}
	if _, err := os.Stat(projectConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project config still exists after finish: %v", err)
	}
	projectRootID := strings.TrimSpace(runQEMUCommand(t, "btrfs", "inspect-internal", "rootid", project))
	parentRootID := strings.TrimSpace(runQEMUCommand(t, "btrfs", "inspect-internal", "rootid", root))
	if projectRootID != parentRootID {
		t.Fatalf("finished project root ID = %s, parent root ID = %s; expected a regular directory", projectRootID, parentRootID)
	}
}

func TestQEMUDockerWorkflow(t *testing.T) {
	runQEMUContainerRuntimeWorkflow(t, "docker")
}

func TestQEMUPodmanWorkflow(t *testing.T) {
	runQEMUContainerRuntimeWorkflow(t, "podman")
}

func runQEMUContainerRuntimeWorkflow(t *testing.T, runtime string) {
	t.Helper()
	if os.Getenv("AGENTBOX_QEMU_GUEST") != "1" {
		t.Skip("requires the QEMU integration guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("container runtime integration must run as root")
	}

	root := filepath.Join("/mnt/test", runtime+"-workflow")
	project := filepath.Join(root, "project")
	snapshotDir := filepath.Join(root, ".agent-snaps")
	configHome := filepath.Join(root, "config")
	gitConfig := filepath.Join(root, "gitconfig")
	for key, value := range map[string]string{
		"XDG_CONFIG_HOME":   configHome,
		"GIT_CONFIG_GLOBAL": gitConfig,
		"HOME":              "/root",
	} {
		t.Setenv(key, value)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runQEMUCommand(t, "btrfs", "subvolume", "create", project)
	copyQEMUProject(t, "/qemu/examples/qemu-project", project)
	runQEMUCommand(t, "git", "-C", project, "init", "-q")
	runQEMUCommand(t, "git", "-C", project, "config", "user.name", "Agentbox QEMU Test")
	runQEMUCommand(t, "git", "-C", project, "config", "user.email", "agentbox-qemu@example.invalid")
	runQEMUCommand(t, "git", "-C", project, "add", ".")
	runQEMUCommand(t, "git", "-C", project, "commit", "-qm", "initial project")
	if err := os.WriteFile(gitConfig, []byte("[safe]\n\tdirectory = *\n[user]\n\tname = Agentbox QEMU Test\n\temail = agentbox-qemu@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectConfigPath, err := configPath(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{
  "project": %q,
  "snapshot_dir": %q,
  "runtime": %q,
  "image": "agentbox-qemu-dummy:latest",
  "agent_cmd": ["/usr/local/bin/dummy-agent", "{task}"],
  "shm_size": "64m",
  "network": "none"
}
`, project, snapshotDir, runtime)
	if err := os.WriteFile(projectConfigPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	if runtime == "docker" {
		startQEMUDockerDaemon(t, "/tmp/agentbox-docker-data")
	} else {
		storageConfig := filepath.Join(root, "storage.conf")
		storage := "[storage]\ndriver = \"vfs\"\ngraphroot = \"/tmp/agentbox-podman-storage\"\nrunroot = \"/run/containers/storage\"\n"
		if err := os.WriteFile(storageConfig, []byte(storage), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONTAINERS_STORAGE_CONF", storageConfig)
		containersConfig := filepath.Join(root, "containers.conf")
		if err := os.WriteFile(containersConfig, []byte("[engine]\ncgroup_manager = \"cgroupfs\"\nevents_logger = \"file\"\nno_pivot_root = true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONTAINERS_CONF", containersConfig)
		if err := os.MkdirAll("/run/user/0", 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/0")
		if output, err := exec.Command("podman", "info", "--format", "{{.Host.CgroupsVersion}}").CombinedOutput(); err != nil {
			t.Fatalf("Podman cannot initialize in QEMU: %v\n%s", err, output)
		}
	}

	t.Logf("building the test image with real %s", runtime)
	runQEMUCommand(t, runtime, "build", "--network=host", "-t", "agentbox-qemu-dummy:latest", "-f",
		"/qemu/examples/qemu-agent/Containerfile", "/qemu/examples/qemu-agent")

	t.Logf("testing agentbox lifecycle against real %s", runtime)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, args := range [][]string{{"ps", "-a"}, {"logs", "agentbox-interactive"}} {
			output, err := exec.Command(runtime, args...).CombinedOutput()
			t.Logf("%s %s diagnostic: err=%v\n%s", runtime, strings.Join(args, " "), err, output)
		}
	})
	start := startQEMUCommandPTY(t, project, "/agentbox", "start", "interactive", "interactive")
	start.waitFor(t, "DUMMY_AGENT_READY")
	start.write(t, "approve staging\n")
	start.waitFor(t, "DUMMY_AGENT_ANSWER=approve staging")
	start.write(t, "\x1b[21~")
	start.waitExit(t)
	disconnected := startQEMUCommandPTY(t, project, "/agentbox", "attach", "interactive")
	disconnected.waitFor(t, "+ "+runtime+" attach agentbox-interactive")
	disconnected.disconnectClient(t)

	if output := runQEMUCommandDir(t, project, "/agentbox", "list"); !strings.Contains(output, "interactive") || !strings.Contains(output, "Up ") {
		t.Fatalf("%s session stopped when its client disconnected:\n%s", runtime, output)
	}

	snapshot := filepath.Join(snapshotDir, "interactive")
	if got := strings.TrimSpace(runQEMUCommandDir(t, project, "/agentbox", "path", "interactive")); got != snapshot {
		t.Fatalf("%s snapshot path = %q, want %q", runtime, got, snapshot)
	}
	if output := runQEMUCommandDir(t, project, "/agentbox", "list"); !strings.Contains(output, "interactive") || !strings.Contains(output, "Up ") {
		t.Fatalf("%s running session missing from list:\n%s", runtime, output)
	}
	if output := watchQEMUSession(t, project, "interactive"); !strings.Contains(output, "DUMMY_AGENT_ANSWER=approve staging") {
		t.Fatalf("%s watch output missing agent response:\n%s", runtime, output)
	}
	shell := startQEMUCommandPTY(t, project, "/agentbox", "shell", "interactive")
	shell.waitFor(t, "# ")
	shell.write(t, "printf 'DUMMY_SHELL_OK\\n'\n")
	shell.waitFor(t, "DUMMY_SHELL_OK\r\n")
	shell.write(t, "exit\n")
	shell.waitExit(t)

	attach := startQEMUCommandPTY(t, project, "/agentbox", "attach", "interactive")
	attach.waitFor(t, "+ "+runtime+" attach agentbox-interactive")
	attach.write(t, "finish\n")
	attach.waitFor(t, "DUMMY_AGENT_CONTINUING")
	attach.waitExit(t)
	waitQEMUContainerExited(t, runtime, "interactive")
	runQEMUCommandDir(t, project, "/agentbox", "accept", "interactive")
	if output := runQEMUCommand(t, "git", "-C", project, "show", "agent/interactive:agent-result.txt"); !strings.Contains(output, "approved: approve staging") {
		t.Fatalf("%s accepted branch missing agent result: %q", runtime, output)
	}
	requireQEMUPathAbsent(t, snapshot)

	firstRun := startQEMUCommandPTY(t, project, "/agentbox", "start", "resume-check", "finish")
	firstRun.waitFor(t, "DUMMY_AGENT_FINISHED")
	firstRun.waitExit(t)
	waitQEMUContainerExited(t, runtime, "resume-check")
	requireQEMURunCount(t, filepath.Join(snapshotDir, "resume-check", "agent-runs"), "1")
	resumedRun := startQEMUCommandPTY(t, project, "/agentbox", "resume", "resume-check", "finish")
	resumedRun.waitFor(t, "DUMMY_AGENT_FINISHED")
	resumedRun.waitExit(t)
	waitQEMUContainerExited(t, runtime, "resume-check")
	requireQEMURunCount(t, filepath.Join(snapshotDir, "resume-check", "agent-runs"), "2")
	runQEMUCommandDir(t, project, "/agentbox", "discard", "resume-check")
	requireQEMUPathAbsent(t, filepath.Join(snapshotDir, "resume-check"))
}

func waitQEMUContainerExited(t *testing.T, runtime, name string) {
	t.Helper()
	filter := "name=^agentbox-" + name + "$"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status := strings.TrimSpace(runQEMUCommand(t, runtime, "ps", "-a", "--filter", filter, "--format", "{{.Status}}"))
		if !containerIsRunning(status) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s container %q remained running", runtime, name)
}

func startQEMUDockerDaemon(t *testing.T, dataRoot string) {
	t.Helper()
	if err := os.MkdirAll("/var/run", 0o755); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create("/tmp/agentbox-dockerd.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("dockerd",
		"--host=unix:///var/run/docker.sock",
		"--data-root", dataRoot,
		"--exec-root", "/run/docker",
		"--pidfile", "/run/docker.pid",
		"--storage-driver=overlay2",
		"--bridge=none",
		"--iptables=false",
		"--ip-forward=false",
		"--ip-masq=false",
	)
	t.Setenv("DOCKER_RAMDISK", "1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start Docker daemon: %v", err)
	}
	done := make(chan struct{})
	var daemonErr error
	go func() {
		daemonErr = cmd.Wait()
		_ = logFile.Close()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("Docker daemon exited during startup: %v\n%s", daemonErr, readQEMUFile("/tmp/agentbox-dockerd.log"))
		default:
		}
		output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
		if err == nil && strings.TrimSpace(string(output)) != "" {
			return
		}
		lastErr = fmt.Errorf("%v: %s", err, output)
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Docker daemon did not become ready: %v\n%s", lastErr, readQEMUFile("/tmp/agentbox-dockerd.log"))
}

func readQEMUFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func runQEMUCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	return runQEMUCommandDir(t, "", name, args...)
}

func runQEMUCommandDir(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func copyQEMUProject(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy QEMU example project: %v", err)
	}
}

func watchQEMUSession(t *testing.T, project, name string) string {
	t.Helper()
	cmd := exec.Command("/agentbox", "watch", name)
	cmd.Dir = project
	var output synchronizedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agentbox watch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), "DUMMY_AGENT_ANSWER=approve staging") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("stop agentbox watch: %v", err)
	}
	_ = cmd.Wait()
	return output.String()
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func requireQEMURunCount(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dummy-agent run count: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("dummy-agent run count = %q, want %q", got, want)
	}
}

func requireQEMUPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s still exists (stat error: %v)", path, err)
	}
}

type ptyCommand struct {
	master  *os.File
	pid     int
	done    chan struct{}
	mu      sync.Mutex
	output  bytes.Buffer
	waitErr error
}

func startQEMUCommandPTY(t *testing.T, dir, name string, args ...string) *ptyCommand {
	t.Helper()
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open PTY master: %v", err)
	}
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(masterFD)
		t.Fatalf("unlock PTY: %v", err)
	}
	number, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(masterFD)
		t.Fatalf("get PTY number: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		_ = unix.Close(masterFD)
		t.Fatalf("open PTY slave: %v", err)
	}
	// A sized terminal makes agentbox use its framed attach UI.
	if err := unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120}); err != nil {
		_ = slave.Close()
		_ = unix.Close(masterFD)
		t.Fatalf("set PTY size: %v", err)
	}
	termios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		_ = slave.Close()
		_ = unix.Close(masterFD)
		t.Fatalf("read PTY terminal settings: %v", err)
	}
	termios.Iflag &^= unix.IXON | unix.IXOFF
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, termios); err != nil {
		_ = slave.Close()
		_ = unix.Close(masterFD)
		t.Fatalf("disable PTY software flow control: %v", err)
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = unix.Close(masterFD)
		t.Fatalf("start %s: %v", name, err)
	}
	_ = slave.Close()
	session := &ptyCommand{master: os.NewFile(uintptr(masterFD), "pty-master"), pid: cmd.Process.Pid, done: make(chan struct{})}
	go session.readOutput()
	go func() {
		session.waitErr = cmd.Wait()
		close(session.done)
	}()
	t.Cleanup(func() {
		select {
		case <-session.done:
		default:
			_ = cmd.Process.Kill()
			<-session.done
		}
		_ = session.master.Close()
	})
	return session
}

func (p *ptyCommand) readOutput() {
	buffer := make([]byte, 4096)
	for {
		count, err := p.master.Read(buffer)
		if count > 0 {
			p.mu.Lock()
			_, _ = p.output.Write(buffer[:count])
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *ptyCommand) write(t *testing.T, input string) {
	t.Helper()
	if _, err := io.WriteString(p.master, input); err != nil {
		t.Fatalf("write to PTY: %v", err)
	}
}

func (p *ptyCommand) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		output := p.output.String()
		p.mu.Unlock()
		if strings.Contains(output, text) {
			return
		}
		select {
		case <-p.done:
			t.Fatalf("command exited before output %q (err=%v)\n%s", text, p.waitErr, output)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	p.mu.Lock()
	output := p.output.String()
	p.mu.Unlock()
	t.Fatalf("timed out waiting for %q\n%s", text, output)
}

func (p *ptyCommand) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		if p.waitErr != nil {
			p.mu.Lock()
			output := p.output.String()
			p.mu.Unlock()
			t.Fatalf("interactive command failed: %v\n%s", p.waitErr, output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive command did not exit")
	}
}

func (p *ptyCommand) disconnectClient(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(-p.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("disconnect terminal client: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal client did not disconnect")
	}
}
