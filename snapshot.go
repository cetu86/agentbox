package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// run executes a command with inherited stdio and echoes it to stderr.
func run(name string, args ...string) error {
	return runWithEnv(name, nil, args...)
}

func runWithEnv(name string, overrides map[string]string, args ...string) error {
	fmt.Fprintln(os.Stderr, "+", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if len(overrides) != 0 {
		cmd.Env = environmentWithOverrides(os.Environ(), overrides)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func environmentWithOverrides(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !ok || !overridden {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

// output captures trimmed stdout.
func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func git(dir string, args ...string) error {
	return run("git", append([]string{"-C", dir}, args...)...)
}

func gitOut(dir string, args ...string) (string, error) {
	return output("git", append([]string{"-C", dir}, args...)...)
}

func (c *Config) snapPath(name string) string { return filepath.Join(c.SnapshotDir, name) }

func initializeProjectSubvolume(project string) (string, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	project = filepath.Clean(project)
	if project == string(filepath.Separator) {
		return "", errors.New("refusing to convert the filesystem root into a project subvolume")
	}
	info, err := os.Lstat(project)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("project path %s must be a real directory", project)
	}
	parent := filepath.Dir(project)
	mountPoint, err := isMountPoint(project)
	if err != nil {
		return "", err
	}
	if mountPoint {
		return "", errors.New("project directory must not be a mount point")
	}
	projectRootID, err := btrfsRootID(project)
	if err != nil {
		return "", err
	}
	parentRootID, err := btrfsRootID(parent)
	if err != nil {
		return "", err
	}
	if projectRootID != parentRootID {
		return "", fmt.Errorf("%s is already a btrfs subvolume", project)
	}

	stagingDir, err := os.MkdirTemp(parent, ".agentbox-init-")
	if err != nil {
		return "", fmt.Errorf("create project migration directory: %w", err)
	}
	subvolume := filepath.Join(stagingDir, "project")
	removeStaging := func(cause error) error {
		deleteErr := requestPrivileged(btrfsRequest{Op: "delete", Path: subvolume})
		if deleteErr != nil {
			deleteErr = fmt.Errorf("remove temporary project subvolume: %w", deleteErr)
		}
		removeErr := os.Remove(stagingDir)
		if removeErr != nil {
			removeErr = fmt.Errorf("remove project migration directory: %w", removeErr)
		}
		return errors.Join(cause, deleteErr, removeErr)
	}
	if err := requestPrivileged(btrfsRequest{Op: "create", Path: subvolume}); err != nil {
		_ = os.Remove(stagingDir)
		return "", err
	}
	copyCmd := exec.Command("cp", "-a", "--reflink=auto", "--",
		project+string(filepath.Separator)+".", subvolume+string(filepath.Separator)+".")
	if output, err := copyCmd.CombinedOutput(); err != nil {
		return "", removeStaging(fmt.Errorf("copy project into btrfs subvolume: %w: %s", err, strings.TrimSpace(string(output))))
	}

	backup := filepath.Join(stagingDir, "original")
	if err := os.Rename(project, backup); err != nil {
		return "", removeStaging(fmt.Errorf("move original project to backup: %w", err))
	}
	if err := os.Rename(subvolume, project); err != nil {
		rollbackErr := os.Rename(backup, project)
		if rollbackErr != nil {
			return "", fmt.Errorf("install btrfs project subvolume: %w; original project is preserved at %s: %v", err, backup, rollbackErr)
		}
		return "", removeStaging(fmt.Errorf("install btrfs project subvolume: %w", err))
	}
	return backup, nil
}

func finishProjectSubvolume(project string) (string, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	project = filepath.Clean(project)
	if project == string(filepath.Separator) {
		return "", errors.New("refusing to convert the filesystem root into a project directory")
	}
	info, err := os.Lstat(project)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("project path %s must be a real directory", project)
	}
	parent := filepath.Dir(project)
	mountPoint, err := isMountPoint(project)
	if err != nil {
		return "", err
	}
	if mountPoint {
		return "", errors.New("project directory must not be a mount point")
	}
	projectRootID, err := btrfsRootID(project)
	if err != nil {
		return "", err
	}
	parentRootID, err := btrfsRootID(parent)
	if err != nil {
		return "", err
	}
	if projectRootID == parentRootID {
		return "", nil
	}

	stagingDir, err := os.MkdirTemp(parent, ".agentbox-finish-")
	if err != nil {
		return "", fmt.Errorf("create project finish directory: %w", err)
	}
	copiedProject := filepath.Join(stagingDir, "project")
	copyCmd := exec.Command("cp", "-a", "--reflink=auto", "--", project, copiedProject)
	if output, err := copyCmd.CombinedOutput(); err != nil {
		cleanupErr := os.RemoveAll(stagingDir)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove temporary project copy: %w", cleanupErr)
		}
		return "", errors.Join(
			fmt.Errorf("copy project into a regular directory: %w: %s", err, strings.TrimSpace(string(output))),
			cleanupErr,
		)
	}

	backup := filepath.Join(stagingDir, "original")
	if err := os.Rename(project, backup); err != nil {
		cleanupErr := os.RemoveAll(stagingDir)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove temporary project copy: %w", cleanupErr)
		}
		return "", errors.Join(fmt.Errorf("move project subvolume to backup: %w", err), cleanupErr)
	}
	if err := os.Rename(copiedProject, project); err != nil {
		rollbackErr := os.Rename(backup, project)
		if rollbackErr != nil {
			return "", fmt.Errorf("install regular project directory: %w; original subvolume remains at %s: %v", err, backup, rollbackErr)
		}
		cleanupErr := os.RemoveAll(stagingDir)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove temporary project copy: %w", cleanupErr)
		}
		return "", errors.Join(fmt.Errorf("install regular project directory: %w", err), cleanupErr)
	}
	return backup, nil
}

func isMountPoint(path string) (bool, error) {
	path, err := cleanProjectPath(path)
	if err != nil {
		return false, err
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mount table: %w", err)
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(mountInfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPath := filepath.Clean(unescape.Replace(fields[4]))
		if mountPath == path {
			return true, nil
		}
	}
	return false, nil
}

func btrfsRootID(path string) (string, error) {
	cmd := exec.Command("btrfs", "inspect-internal", "rootid", path)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("check btrfs subvolume status for %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	rootID := strings.TrimSpace(string(output))
	if rootID == "" {
		return "", fmt.Errorf("btrfs returned an empty subvolume ID for %s", path)
	}
	return rootID, nil
}

func (c *Config) requireSnapshot(name string) (string, error) {
	p := c.snapPath(name)
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", fmt.Errorf("no snapshot named %q in %s", name, c.SnapshotDir)
	}
	return p, nil
}

// snapshot creates a writable btrfs snapshot of the project (instant, copy-on-write).
func (c *Config) snapshot(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid name %q (use lowercase letters, digits, . _ -)", name)
	}
	dst := c.snapPath(name)
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("snapshot %q already exists", name)
	}
	if err := os.MkdirAll(c.SnapshotDir, 0o755); err != nil {
		return "", err
	}
	return dst, requestPrivileged(btrfsRequest{Op: "snapshot", Source: c.Project, Path: dst})
}

func (c *Config) deleteSnapshot(name string) error {
	p, err := c.requireSnapshot(name)
	if err != nil {
		return err
	}
	return requestPrivileged(btrfsRequest{Op: "delete", Path: p})
}

func isDirty(dir string) (bool, error) {
	out, err := gitOut(dir, "status", "--porcelain")
	return out != "", err
}

// commitIfDirty commits everything not ignored so a git fetch doesn't lose work.
func commitIfDirty(dir, msg string) error {
	dirty, err := isDirty(dir)
	if err != nil || !dirty {
		return err
	}
	if err := git(dir, "add", "-A"); err != nil {
		return err
	}
	return git(dir, "commit", "-m", msg)
}

// promote swaps the project subvolume for the snapshot, keeping the old one as a backup.
// Unlike a git fetch this also brings back build artifacts and caches.
func (c *Config) promote(name string, force bool) (backup string, err error) {
	snap, err := c.requireSnapshot(name)
	if err != nil {
		return "", err
	}
	if !force {
		if dirty, err := isDirty(c.Project); err != nil {
			return "", err
		} else if dirty {
			return "", fmt.Errorf("%s has uncommitted changes; commit them or use --force", c.Project)
		}
	}
	backup = c.snapPath("_pre-" + name)
	if _, err := os.Stat(backup); err == nil {
		return "", fmt.Errorf("backup %s already exists", backup)
	}
	if err := os.Rename(c.Project, backup); err != nil {
		return "", err
	}
	if err := requestPrivileged(btrfsRequest{Op: "snapshot", Source: snap, Path: c.Project}); err != nil {
		_ = os.Rename(backup, c.Project) // roll back
		return "", err
	}
	return backup, nil
}
