package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// commandCapture collects command output while the TUI owns the terminal.
type commandCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *commandCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *commandCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// activeCapture, when set, makes run() collect output instead of using the
// terminal.
var activeCapture atomic.Pointer[commandCapture]

// captureMu serializes captures: the single active slot cannot hold two.
var captureMu sync.Mutex

// captureCommands runs fn with command output captured and returns it.
func captureCommands(fn func() error) (string, error) {
	captureMu.Lock()
	defer captureMu.Unlock()
	capture := &commandCapture{}
	activeCapture.Store(capture)
	defer activeCapture.Store(nil)
	err := fn()
	return capture.String(), err
}

// boxInfo describes one agent box (snapshot) of the project.
type boxInfo struct {
	Name    string
	Branch  string
	Dirty   bool
	Status  string // container status as reported by the runtime; empty if none
	Running bool
}

// listBoxes returns the project's boxes sorted by name.
func (c *Config) listBoxes() ([]boxInfo, error) {
	entries, err := os.ReadDir(c.SnapshotDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var boxes []boxInfo
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), diffMetadataPrefix) {
			continue
		}
		p := c.snapPath(e.Name())
		branch, _ := gitOut(p, "rev-parse", "--abbrev-ref", "HEAD")
		dirty, _ := isDirty(p)
		status, err := c.containerStatus(e.Name())
		if err != nil {
			return nil, err
		}
		boxes = append(boxes, boxInfo{
			Name: e.Name(), Branch: branch, Dirty: dirty,
			Status: status, Running: containerIsRunning(status),
		})
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })
	return boxes, nil
}

// createBox snapshots the project as box name and, unless noBranch is set,
// switches the snapshot to the branch agent/<name>. It does not start the agent.
func (c *Config) createBox(name, base string, noBranch bool) (string, error) {
	status, err := c.containerStatus(name)
	if err != nil {
		return "", err
	}
	if containerIsRunning(status) {
		return "", fmt.Errorf("container for %q is already running; attach to it or choose another name", name)
	}
	if err := c.removeShell(name); err != nil {
		return "", err
	}
	snap, err := c.snapshot(name)
	if err != nil {
		return "", err
	}
	if err := recordDiffBaseline(snap); err != nil {
		return "", fmt.Errorf("record session diff baseline: %w", err)
	}
	if !noBranch {
		a := []string{"switch", "-c", "agent/" + name}
		if base != "" {
			a = append(a, base)
		}
		if err := git(snap, a...); err != nil {
			return "", err
		}
	}
	return snap, nil
}

type acceptOptions struct {
	Promote bool // swap the snapshot in as the project
	Force   bool // with Promote: allow a dirty project directory
	Keep    bool // keep the snapshot afterwards
	Merge   bool // git merge the fetched branch into the project's current branch
}

// acceptBox brings a box's work back into the project and returns a summary.
func (c *Config) acceptBox(name string, o acceptOptions) (string, error) {
	if o.Promote && o.Merge {
		return "", errors.New("--promote and --merge cannot be combined")
	}
	snap, err := c.requireSnapshot(name)
	if err != nil {
		return "", err
	}
	status, err := c.containerStatus(name)
	if err != nil {
		return "", err
	}
	if containerIsRunning(status) {
		return "", fmt.Errorf("container for %q is still running", name)
	}

	var current string
	if o.Merge {
		if current, err = c.mergePreflight(); err != nil {
			return "", err
		}
	}
	if err := c.removeShell(name); err != nil {
		return "", err
	}

	var message string
	if o.Promote {
		backup, err := c.promote(name, o.Force)
		if err != nil {
			return "", err
		}
		message = fmt.Sprintf("promoted; previous project kept at %s (agentbox discard _pre-%s when happy)", backup, name)
	} else {
		branch, err := gitOut(snap, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", err
		}
		if err := commitIfDirty(snap, "agentbox: "+name); err != nil {
			return "", err
		}
		if err := git(c.Project, "fetch", snap, branch+":"+branch); err != nil {
			return "", err
		}
		if o.Merge {
			if err := c.mergeBranch(branch, current); err != nil {
				return "", fmt.Errorf("%w; %s is fetched into %s and the box %q was kept", err, branch, c.Project, name)
			}
			message = fmt.Sprintf("merged %s into %s", branch, current)
		} else {
			message = fmt.Sprintf("fetched %s into %s; merge it with: git merge %s", branch, c.Project, branch)
		}
	}
	if o.Keep {
		return message, nil
	}
	if err := c.removeStoppedContainer(name); err != nil {
		return message, err
	}
	return message, c.deleteSnapshot(name)
}

// mergePreflight checks that the project can take a merge and returns the
// branch that is checked out.
func (c *Config) mergePreflight() (string, error) {
	current, err := gitOut(c.Project, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || current == "" {
		return "", fmt.Errorf("%s is not on a branch (detached HEAD); check one out before merging", c.Project)
	}
	if out, err := gitOut(c.Project, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return "", err
	} else if out != "" {
		return "", fmt.Errorf("%s has uncommitted changes; commit or stash them before merging", c.Project)
	}
	return current, nil
}

// mergeBranch merges branch into the checked-out branch of the project. A
// failed merge is aborted so the project is left as it was.
func (c *Config) mergeBranch(branch, current string) error {
	if branch == "HEAD" || branch == current {
		return fmt.Errorf("nothing to merge: the box is on %q", branch)
	}
	if err := git(c.Project, "merge", "--no-edit", branch); err != nil {
		abortErr := git(c.Project, "merge", "--abort")
		if abortErr != nil {
			return errors.Join(fmt.Errorf("merge of %s failed", branch), fmt.Errorf("abort merge: %w", abortErr))
		}
		return fmt.Errorf("merge of %s failed and was aborted", branch)
	}
	return nil
}

// discardBox stops the box's containers and deletes its snapshot.
func (c *Config) discardBox(name string) error {
	if _, err := c.requireSnapshot(name); err != nil {
		return err
	}
	status, err := c.containerStatus(name)
	if err != nil {
		return err
	}
	if status != "" {
		if err := run(c.Runtime, "rm", "-f", c.containerName(name)); err != nil {
			return err
		}
	}
	if err := c.removeShell(name); err != nil {
		return err
	}
	return c.deleteSnapshot(name)
}

// boxDiff returns the changes made in a box since it was created.
func (c *Config) boxDiff(name string) (string, bool, error) {
	if !validName.MatchString(name) {
		return "", false, fmt.Errorf("invalid session name %q", name)
	}
	snap, err := c.requireSnapshot(name)
	if err != nil {
		return "", false, err
	}
	baseline, err := sessionDiffBaseline(c.SnapshotDir, name)
	if err != nil {
		return "", false, err
	}
	var buf bytes.Buffer
	changed, err := showSnapshotDiff(snap, baseline, &buf)
	return buf.String(), changed, err
}

// initProject registers dir as a project, optionally converting it to a btrfs
// subvolume first. It returns the config file and the migration backup, if any.
func initProject(dir string, subvolume bool) (configFile, backup string, err error) {
	if err := rejectMigrationBackup(dir); err != nil {
		return "", "", err
	}
	if err := validateConfigForRegistration(dir); err != nil {
		return "", "", err
	}
	if subvolume {
		if backup, err = initializeProjectSubvolume(dir); err != nil {
			return "", "", err
		}
	}
	configFile, err = registerProject(dir)
	return configFile, backup, err
}

// imageExists reports whether the configured image is present locally.
func (c *Config) imageExists() bool {
	_, err := output(c.Runtime, "image", "inspect", c.Image)
	return err == nil
}

func projectRoot(dir string) (string, error) {
	root, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return cleanProjectPath(filepath.Clean(root))
}
