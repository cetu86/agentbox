package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__dummy-agent" {
		if err := runDummyAgent(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := runRuntime(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runRuntime(args []string) error {
	state := os.Getenv("AGENTBOX_TEST_RUNTIME_STATE")
	if state == "" {
		return errors.New("AGENTBOX_TEST_RUNTIME_STATE is required")
	}
	if len(args) == 0 {
		return errors.New("missing runtime operation")
	}
	switch args[0] {
	case "build":
		return buildImage(state, args[1:])
	case "run":
		return runContainer(state, args[1:])
	case "ps":
		return listContainers(state, args[1:])
	case "attach":
		if len(args) != 2 {
			return errors.New("usage: runtime attach NAME")
		}
		return tmux(state, "attach-session", "-t", args[1])
	case "logs":
		return followLogs(state, args[1:])
	case "exec":
		return execInSession(state, args[1:])
	case "rm":
		return removeContainer(state, args[1:])
	default:
		return fmt.Errorf("unsupported runtime operation %q", args[0])
	}
}

func buildImage(state string, args []string) error {
	var image, containerfile, context string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t", "-f":
			if i+1 >= len(args) {
				return fmt.Errorf("missing value after %s", args[i])
			}
			i++
			if args[i-1] == "-t" {
				image = args[i]
			} else {
				containerfile = args[i]
			}
		default:
			context = args[i]
		}
	}
	if image == "" || containerfile == "" || context == "" {
		return errors.New("build requires -t IMAGE -f CONTAINERFILE CONTEXT")
	}
	if _, err := os.Stat(containerfile); err != nil {
		return fmt.Errorf("read generated Containerfile: %w", err)
	}
	if _, err := os.Stat(filepath.Join(context, "Containerfile")); err != nil {
		return fmt.Errorf("read build context Containerfile: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(state, "images", image), 0o755); err != nil {
		return err
	}
	fmt.Printf("built test image %s\n", image)
	return nil
}

func runContainer(state string, args []string) error {
	var name, workdir, volume, image string
	var interactive bool
	var command []string
	for i := 0; i < len(args); {
		switch args[i] {
		case "--name", "-v", "-w", "--hostname", "--shm-size", "-e", "--tmpfs", "--user", "--network":
			if i+1 >= len(args) {
				return fmt.Errorf("missing value after %s", args[i])
			}
			value := args[i+1]
			switch args[i] {
			case "--name":
				name = value
			case "-v":
				volume = value
			case "-w":
				workdir = value
			}
			i += 2
		case "-d", "-i", "-t", "-it", "--rm":
			if args[i] == "-i" || args[i] == "-t" || args[i] == "-it" {
				interactive = true
			}
			i++
		default:
			image = args[i]
			i++
			command = args[i:]
			i = len(args)
		}
	}
	if image == "" || workdir == "" || volume == "" || len(command) == 0 {
		return errors.New("run requires image, command, working directory, and snapshot mount")
	}
	if _, err := os.Stat(filepath.Join(state, "images", image)); err != nil {
		return fmt.Errorf("test runtime image %q has not been built", image)
	}
	snapshot, target, ok := strings.Cut(volume, ":")
	if !ok || filepath.Clean(target) != filepath.Clean(workdir) {
		return fmt.Errorf("snapshot mount %q does not target working directory %q", volume, workdir)
	}
	if !filepath.IsAbs(snapshot) || !filepath.IsAbs(workdir) {
		return errors.New("snapshot and working directory must be absolute")
	}
	if name == "" {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = snapshot, os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	if !interactive {
		return errors.New("managed test sessions must be interactive")
	}
	if filepath.Base(name) != name || !strings.HasPrefix(name, "agentbox-") {
		return fmt.Errorf("invalid managed test session name %q", name)
	}
	if len(command) != 2 || command[0] != "/qemu/runtime" {
		return fmt.Errorf("unexpected dummy-agent command %q", command)
	}
	if command[1] != "interactive" && command[1] != "finish" {
		return fmt.Errorf("unexpected dummy-agent task %q", command[1])
	}
	dir := filepath.Join(state, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "workdir"), []byte(snapshot+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "log"), nil, 0o600); err != nil {
		return err
	}
	tmuxCommand := fmt.Sprintf("exec /qemu/runtime __dummy-agent %s", command[1])
	if err := tmux(state, "new-session", "-d", "-s", name, "-c", snapshot, tmuxCommand); err != nil {
		return err
	}
	return tmux(state, "pipe-pane", "-o", "-t", name, "cat >> "+filepath.Join(dir, "log"))
}

func listContainers(state string, args []string) error {
	var filter string
	for i := 0; i < len(args); i++ {
		if args[i] == "--filter" && i+1 < len(args) {
			filter = args[i+1]
			i++
		}
	}
	name := strings.TrimSuffix(strings.TrimPrefix(filter, "name=^"), "$")
	if name == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(state, name)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := tmux(state, "has-session", "-t", name); err == nil {
		fmt.Println("Up 1 minute")
	} else {
		fmt.Println("Exited (0) 1 second ago")
	}
	return nil
}

func followLogs(state string, args []string) error {
	if len(args) > 0 && args[0] == "-f" {
		args = args[1:]
	}
	if len(args) != 1 {
		return errors.New("usage: runtime logs -f NAME")
	}
	path := filepath.Join(state, args[0], "log")
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			if _, err := io.WriteString(os.Stdout, line); err != nil {
				return err
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if errors.Is(readErr, io.EOF) {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func execInSession(state string, args []string) error {
	for len(args) > 0 && (args[0] == "-i" || args[0] == "-t" || args[0] == "-it") {
		args = args[1:]
	}
	if len(args) < 2 {
		return errors.New("usage: runtime exec NAME COMMAND")
	}
	name := args[0]
	if filepath.Base(name) != name || !strings.HasPrefix(name, "agentbox-") {
		return fmt.Errorf("invalid session name %q", name)
	}
	workingDir, err := os.ReadFile(filepath.Join(state, name, "workdir"))
	if err != nil {
		return err
	}
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Dir = strings.TrimSpace(string(workingDir))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func removeContainer(state string, args []string) error {
	if len(args) > 0 && args[0] == "-f" {
		args = args[1:]
	}
	if len(args) != 1 {
		return errors.New("usage: runtime rm [-f] NAME")
	}
	name := args[0]
	if filepath.Base(name) != name || !strings.HasPrefix(name, "agentbox-") {
		return fmt.Errorf("invalid session name %q", name)
	}
	_ = tmux(state, "kill-session", "-t", name)
	return os.RemoveAll(filepath.Join(state, name))
}

func tmux(state string, args ...string) error {
	confDir := filepath.Join(state, "tmux")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(confDir, "tmux.conf")
	if _, err := os.Stat(conf); errors.Is(err, os.ErrNotExist) {
		data := "set-option -g status off\n"
		if err := os.WriteFile(conf, []byte(data), 0o600); err != nil {
			return err
		}
	}
	cmd := exec.Command("tmux", append([]string{"-f", conf}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func runDummyAgent(args []string) error {
	cmd := exec.Command("/qemu/dummy-agent", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
