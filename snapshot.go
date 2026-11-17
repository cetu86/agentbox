package main

import (
	"errors"
	"fmt"
	"io"
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

const diffMetadataPrefix = ".agentbox-diff-"

func diffMetadataDir(snapshotDir, name string) string {
	return filepath.Join(snapshotDir, diffMetadataPrefix+name)
}

func recordDiffBaseline(dir string) (resultErr error) {
	snapshotDir, name := filepath.Dir(dir), filepath.Base(dir)
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	metadataDir := diffMetadataDir(snapshotDir, name)
	if err := os.Mkdir(metadataDir, 0o700); err != nil {
		return fmt.Errorf("create session diff metadata: %w", err)
	}
	defer func() {
		if resultErr != nil {
			if cleanupErr := os.RemoveAll(metadataDir); cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete session diff metadata: %w", cleanupErr))
			}
		}
	}()

	objectsDir := filepath.Join(metadataDir, "objects")
	if err := os.MkdirAll(filepath.Join(objectsDir, "info"), 0o700); err != nil {
		return fmt.Errorf("create session diff objects: %w", err)
	}
	repoObjects, err := sessionGitObjects(dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(objectsDir, "info", "alternates"), []byte(repoObjects+"\n"), 0o600); err != nil {
		return fmt.Errorf("configure session diff objects: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "agentbox-baseline-")
	if err != nil {
		return fmt.Errorf("create temporary Git index: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tree, err := gitTreeWithEnvironment(dir, tempDir, map[string]string{"GIT_OBJECT_DIRECTORY": objectsDir})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "tree"), []byte(tree+"\n"), 0o600); err != nil {
		return fmt.Errorf("save session diff baseline: %w", err)
	}
	return nil
}

func sessionDiffBaseline(snapshotDir, name string) (string, error) {
	baselineBytes, err := os.ReadFile(filepath.Join(diffMetadataDir(snapshotDir, name), "tree"))
	if err == nil {
		value := strings.TrimSpace(string(baselineBytes))
		if !isGitObjectID(value) {
			return "", fmt.Errorf("invalid diff baseline for session %q", name)
		}
		return value, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("session %q has no saved diff baseline; start a new session with this version of agentbox", name)
	}
	return "", fmt.Errorf("read diff baseline for session %q: %w", name, err)
}

func isGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func gitTreeWithEnvironment(dir, tempDir string, overrides map[string]string) (string, error) {
	env := make(map[string]string, len(overrides)+1)
	for key, value := range overrides {
		env[key] = value
	}
	env["GIT_INDEX_FILE"] = filepath.Join(tempDir, "index")
	for _, args := range [][]string{
		{"read-tree", "HEAD"},
		{"add", "-A"},
	} {
		if _, err := gitOutputWithEnvironment(dir, env, args...); err != nil {
			return "", err
		}
	}
	return gitOutputWithEnvironment(dir, env, "write-tree")
}

func gitOutputWithEnvironment(dir string, overrides map[string]string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if len(overrides) != 0 {
		cmd.Env = environmentWithOverrides(os.Environ(), overrides)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func sessionGitObjects(dir string) (string, error) {
	objectsDir, err := gitOut(dir, "rev-parse", "--git-path", "objects")
	if err != nil {
		return "", fmt.Errorf("locate session Git objects: %w", err)
	}
	if !filepath.IsAbs(objectsDir) {
		objectsDir = filepath.Join(dir, objectsDir)
	}
	objectsDir, err = filepath.Abs(objectsDir)
	if err != nil {
		return "", fmt.Errorf("resolve session Git object directory: %w", err)
	}
	return objectsDir, nil
}

func showSnapshotDiff(dir, baseline string, output io.Writer) (bool, error) {
	if !isGitObjectID(baseline) {
		return false, errors.New("invalid diff baseline object ID")
	}
	tempDir, err := os.MkdirTemp("", "agentbox-diff-")
	if err != nil {
		return false, fmt.Errorf("create temporary diff data: %w", err)
	}
	defer os.RemoveAll(tempDir)

	objectsDir := filepath.Join(tempDir, "objects")
	if err := os.MkdirAll(filepath.Join(objectsDir, "info"), 0o700); err != nil {
		return false, fmt.Errorf("create temporary Git object directory: %w", err)
	}
	repoObjects, err := sessionGitObjects(dir)
	if err != nil {
		return false, err
	}
	baselineObjects := filepath.Join(diffMetadataDir(filepath.Dir(dir), filepath.Base(dir)), "objects")
	alternates := repoObjects + "\n" + baselineObjects + "\n"
	if err := os.WriteFile(filepath.Join(objectsDir, "info", "alternates"), []byte(alternates), 0o600); err != nil {
		return false, fmt.Errorf("configure temporary Git objects: %w", err)
	}

	env := map[string]string{"GIT_OBJECT_DIRECTORY": objectsDir}
	currentTree, err := gitTreeWithEnvironment(dir, tempDir, env)
	if err != nil {
		return false, fmt.Errorf("snapshot current session files: %w", err)
	}
	if currentTree == baseline {
		return false, nil
	}

	diffEnv := make(map[string]string, len(env)+1)
	for key, value := range env {
		diffEnv[key] = value
	}
	diffEnv["GIT_INDEX_FILE"] = filepath.Join(tempDir, "index")
	cmd := exec.Command("git", "-C", dir, "diff", "--no-ext-diff", "--no-textconv", "--color=auto", baseline, currentTree, "--")
	cmd.Env = environmentWithOverrides(os.Environ(), diffEnv)
	cmd.Stdout, cmd.Stderr = output, os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("show session diff: %w", err)
	}
	return true, nil
}

func (c *Config) deleteSnapshot(name string) error {
	p, err := c.requireSnapshot(name)
	if err != nil {
		return err
	}
	if err := requestPrivileged(btrfsRequest{Op: "delete", Path: p}); err != nil {
		return err
	}
	if validName.MatchString(name) {
		if err := os.RemoveAll(diffMetadataDir(c.SnapshotDir, name)); err != nil {
			return fmt.Errorf("remove session diff metadata: %w", err)
		}
	}
	return nil
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
