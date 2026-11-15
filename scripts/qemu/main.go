package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const defaultAlpineVersion = "v3.22"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentbox QEMU integration:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	version := envOr("AGENTBOX_QEMU_ALPINE_VERSION", defaultAlpineVersion)
	cacheDir := os.Getenv("AGENTBOX_QEMU_CACHE_DIR")
	if cacheDir == "" {
		cacheRoot := os.Getenv("XDG_CACHE_HOME")
		if cacheRoot == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			cacheRoot = filepath.Join(home, ".cache")
		}
		cacheDir = filepath.Join(cacheRoot, "agentbox", "qemu", "alpine-"+version+"-x86_64")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	baseURL := "https://dl-cdn.alpinelinux.org/alpine/" + version
	if err := fetchCached(filepath.Join(cacheDir, "initramfs-virt"), baseURL+"/releases/x86_64/netboot/initramfs-virt"); err != nil {
		return err
	}
	apkIndexURL := baseURL + "/main/x86_64/APKINDEX.tar.gz"
	index, err := httpGet(apkIndexURL)
	if err != nil {
		return fmt.Errorf("download Alpine package index: %w", err)
	}
	kernelVersion, err := alpineKernelVersion(index)
	if err != nil {
		return err
	}
	kernelAPK := "linux-virt-" + kernelVersion + ".apk"
	kernelAPKPath := filepath.Join(cacheDir, kernelAPK)
	if err := fetchCached(kernelAPKPath, baseURL+"/main/x86_64/"+kernelAPK); err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "agentbox-qemu-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	agentboxPath := filepath.Join(workDir, "agentbox")
	testPath := filepath.Join(workDir, "agentbox-integration.test")
	runtimePath := filepath.Join(workDir, "runtime")
	dummyAgentPath := filepath.Join(workDir, "dummy-agent")
	if err := build(root, agentboxPath, ".", false); err != nil {
		return err
	}
	if err := build(root, testPath, ".", true); err != nil {
		return err
	}
	if err := build(root, runtimePath, "./scripts/qemu/runtime", false); err != nil {
		return err
	}
	if err := build(root, dummyAgentPath, "./scripts/qemu/dummy-agent", false); err != nil {
		return err
	}

	overlay := filepath.Join(workDir, "overlay")
	if err := assembleOverlay(root, workDir, overlay, agentboxPath, testPath, runtimePath, dummyAgentPath, kernelAPKPath, version); err != nil {
		return err
	}
	initramfs, err := os.Create(filepath.Join(workDir, "initramfs"))
	if err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(cacheDir, "initramfs-virt"),
		filepath.Join(workDir, "overlay.cpio"),
	} {
		file, err := os.Open(path)
		if err != nil {
			initramfs.Close()
			return err
		}
		_, copyErr := io.Copy(initramfs, file)
		closeErr := file.Close()
		if copyErr != nil {
			initramfs.Close()
			return copyErr
		}
		if closeErr != nil {
			initramfs.Close()
			return closeErr
		}
	}
	if err := initramfs.Close(); err != nil {
		return err
	}

	diskPath := filepath.Join(workDir, "test-disk.qcow2")
	if err := runCommand("", envOr("QEMU_IMG", "qemu-img"), "create", "-q", "-f", "qcow2", diskPath, "2G"); err != nil {
		return fmt.Errorf("create QEMU test disk: %w", err)
	}
	serialPath := filepath.Join(workDir, "serial.log")
	qemuArgs := []string{
		"-machine", "accel=tcg",
		"-m", "2048",
		"-smp", "2",
		"-kernel", filepath.Join(workDir, "vmlinuz-virt"),
		"-initrd", filepath.Join(workDir, "initramfs"),
		"-append", "console=ttyS0,115200 rdinit=/agentbox-init panic=1",
		"-drive", "file=" + diskPath + ",if=virtio,format=qcow2",
		"-netdev", "user,id=net0",
		"-device", "virtio-net-pci,netdev=net0",
		"-display", "none",
		"-serial", "file:" + serialPath,
		"-monitor", "none",
		"-no-reboot",
	}
	timeout, err := qemuTimeout()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	qemu := exec.CommandContext(ctx, envOr("QEMU", "qemu-system-x86_64"), qemuArgs...)
	if err := qemu.Run(); err != nil && ctx.Err() == nil {
		printFile(serialPath)
		return fmt.Errorf("QEMU exited unsuccessfully: %w", err)
	}
	serial, err := os.ReadFile(serialPath)
	if err != nil {
		return fmt.Errorf("read QEMU serial output: %w", err)
	}
	_, _ = os.Stdout.Write(serial)
	if ctx.Err() != nil {
		return fmt.Errorf("QEMU did not finish within %s", timeout)
	}
	if !strings.Contains(string(serial), "AGENTBOX_QEMU_RESULT=PASS") {
		return errors.New("guest did not report a passing integration run")
	}
	fmt.Printf("QEMU integration tests passed (Alpine image cached in %s)\n", cacheDir)
	return nil
}

func repositoryRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("locate QEMU integration runner source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", ".."))
	if err != nil {
		return "", err
	}
	return root, nil
}

func build(root, output, target string, tests bool) error {
	args := []string{"build", "-o", output, target}
	if tests {
		args = []string{"test", "-c", "-o", output, target}
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"GOOS":        "linux",
		"GOARCH":      "amd64",
		"CGO_ENABLED": "0",
	})
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w\n%s", target, err, outputBytes)
	}
	return nil
}

func assembleOverlay(root, workDir, overlay, agentbox, testBinary, runtimeBinary, dummyAgent, kernelAPK, alpineVersion string) error {
	for _, dir := range []string{
		"bin", "sbin", "etc/apk", "lib", "qemu/examples/qemu-project", "qemu/examples/qemu-agent",
	} {
		if err := os.MkdirAll(filepath.Join(overlay, dir), 0o755); err != nil {
			return err
		}
	}
	if err := extractKernel(kernelAPK, filepath.Join(workDir, "kernel")); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(workDir, "kernel", "boot", "vmlinuz-virt"), filepath.Join(workDir, "vmlinuz-virt"), 0o644); err != nil {
		return err
	}
	if err := copyTree(filepath.Join(workDir, "kernel", "lib", "modules"), filepath.Join(overlay, "lib", "modules")); err != nil {
		return err
	}
	for _, file := range []struct {
		source string
		target string
	}{
		{agentbox, filepath.Join(overlay, "agentbox")},
		{testBinary, filepath.Join(overlay, "agentbox-integration.test")},
		{runtimeBinary, filepath.Join(overlay, "qemu", "runtime")},
		{dummyAgent, filepath.Join(overlay, "qemu", "dummy-agent")},
		{filepath.Join(root, "scripts", "qemu", "guest-init.sh"), filepath.Join(overlay, "qemu", "guest-init.sh")},
	} {
		if err := copyFile(file.source, file.target, 0o755); err != nil {
			return err
		}
	}
	if err := copyTree(filepath.Join(root, "examples", "qemu-project"), filepath.Join(overlay, "qemu", "examples", "qemu-project")); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(root, "examples", "qemu-agent", "Containerfile"), filepath.Join(overlay, "qemu", "examples", "qemu-agent", "Containerfile"), 0o644); err != nil {
		return err
	}
	if err := copyFile(dummyAgent, filepath.Join(overlay, "qemu", "examples", "qemu-agent", "dummy-agent"), 0o755); err != nil {
		return err
	}
	repositories := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/%s/community\n", alpineVersion, alpineVersion)
	if err := os.WriteFile(filepath.Join(overlay, "etc", "apk", "repositories"), []byte(repositories), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(overlay, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/bin/sh\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(overlay, "etc", "group"), []byte("root:x:0:\nnobody:x:65534:\n"), 0o644); err != nil {
		return err
	}
	if err := addBusyboxLinks(overlay); err != nil {
		return err
	}
	if err := os.Symlink("/qemu/guest-init.sh", filepath.Join(overlay, "agentbox-init")); err != nil {
		return err
	}
	return makeNewcArchive(overlay, filepath.Join(workDir, "overlay.cpio"))
}

func addBusyboxLinks(overlay string) error {
	for _, name := range []string{
		"cat", "chmod", "chown", "cp", "grep", "mkdir", "mount", "mv", "poweroff",
		"rm", "sed", "sleep", "su", "sync",
	} {
		for _, dir := range []string{"bin", "sbin"} {
			if err := os.Symlink("/bin/busybox", filepath.Join(overlay, dir, name)); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		}
	}
	for _, name := range []string{"ip", "udhcpc"} {
		if err := os.Symlink("/bin/busybox", filepath.Join(overlay, "sbin", name)); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

func makeNewcArchive(dir, destination string) error {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if relative == "." {
			paths = append(paths, ".")
		} else {
			paths = append(paths, "./"+filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return err
	}
	cmd := exec.Command("cpio", "--quiet", "-o", "-H", "newc")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	cmd.Stdout = file
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	closeErr := file.Close()
	if runErr != nil {
		return fmt.Errorf("create initramfs overlay archive: %w", runErr)
	}
	return closeErr
}

func extractKernel(apk, destination string) error {
	file, err := os.Open(apk)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.ToSlash(header.Name), "./")
		if name != "boot/vmlinuz-virt" && !strings.HasPrefix(name, "lib/modules/") {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, reader)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func alpineKernelVersion(index []byte) (string, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(index)))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if header.Name != "APKINDEX" {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return "", err
		}
		for _, entry := range strings.Split(string(data), "\n\n") {
			var packageName, version string
			for _, line := range strings.Split(entry, "\n") {
				if strings.HasPrefix(line, "P:") {
					packageName = strings.TrimPrefix(line, "P:")
				} else if strings.HasPrefix(line, "V:") {
					version = strings.TrimPrefix(line, "V:")
				}
			}
			if packageName == "linux-virt" && version != "" {
				return version, nil
			}
		}
	}
	return "", errors.New("linux-virt package not found in Alpine package index")
}

func fetchCached(path, url string) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(os.Stderr, "Downloading %s\n", url)
	data, err := httpGet(url)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		response, err := client.Get(url)
		if err == nil {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				body, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if readErr != nil {
					return nil, readErr
				}
				if closeErr != nil {
					return nil, closeErr
				}
				return body, nil
			}
			lastErr = fmt.Errorf("GET %s: %s", url, response.Status)
			_ = response.Body.Close()
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

func copyFile(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
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
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func runCommand(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func printFile(path string) {
	data, err := os.ReadFile(path)
	if err == nil {
		_, _ = os.Stdout.Write(data)
	}
}

func qemuTimeout() (time.Duration, error) {
	value := os.Getenv("AGENTBOX_QEMU_TIMEOUT")
	if value == "" {
		return 600 * time.Second, nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid AGENTBOX_QEMU_TIMEOUT %q", value)
	}
	return timeout, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func withEnv(env []string, values map[string]string) []string {
	out := make([]string, 0, len(env)+len(values))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := values[key]; !replace {
			out = append(out, entry)
		}
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}
