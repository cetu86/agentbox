package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAuthorizedParentForSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot")

	parent, err := openAuthorizedParent(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("authorization probe left entries behind: %v", entries)
	}
}

func TestSnapshotStagingDirectoryIsPrivateAndRemoved(t *testing.T) {
	dir := t.TempDir()
	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	name, staged, err := createStagingDirectory(int(parent.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	info, err := staged.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory mode = %04o, want 0700", info.Mode().Perm())
	}
	if err := removeStagingDirectory(int(parent.Fd()), name, int(staged.Fd())); err != nil {
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("staging directory still exists: %v", err)
	}
}

func TestOpenAuthorizedSubvolume(t *testing.T) {
	dir := t.TempDir()
	subvolume := filepath.Join(dir, "snapshot")
	if err := os.Mkdir(subvolume, 0o700); err != nil {
		t.Fatal(err)
	}

	parent, err := openAuthorizedSubvolume(subvolume)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAuthorizedParentRejectsInvalidTargets(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		path  string
		check func(string) (*os.File, error)
	}{
		{name: "existing destination", path: existing, check: openAuthorizedParent},
		{name: "missing subvolume", path: filepath.Join(dir, "missing"), check: openAuthorizedSubvolume},
		{name: "symlink target", path: link, check: openAuthorizedSubvolume},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, err := test.check(test.path)
			if err == nil {
				_ = parent.Close()
				t.Fatal("expected target to be rejected")
			}
		})
	}
}
