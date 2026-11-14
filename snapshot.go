package main

import (
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
	fmt.Fprintln(os.Stderr, "+", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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
