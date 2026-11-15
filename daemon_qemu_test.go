package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQEMUDaemonSnapshotAndDelete(t *testing.T) {
	requireQEMUGuest(t)
	source := os.Getenv("AGENTBOX_QEMU_SOURCE")
	destination := os.Getenv("AGENTBOX_QEMU_DESTINATION")

	if err := requestPrivileged(btrfsRequest{Op: "snapshot", Source: source, Path: destination}); err != nil {
		t.Fatalf("snapshot request: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "payload.txt"))
	if err != nil {
		t.Fatalf("read snapshotted file: %v", err)
	}
	if string(content) != "qemu daemon integration\n" {
		t.Fatalf("snapshot content = %q", content)
	}
	if err := requestPrivileged(btrfsRequest{Op: "delete", Path: destination}); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted subvolume still exists (stat error: %v)", err)
	}
}

func TestQEMUDaemonCreateSubvolume(t *testing.T) {
	requireQEMUGuest(t)
	path := os.Getenv("AGENTBOX_QEMU_CREATE_PATH")
	if err := requestPrivileged(btrfsRequest{Op: "create", Path: path}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "client-owned.txt"), []byte("writable\n"), 0o600); err != nil {
		t.Fatalf("client cannot write to created subvolume: %v", err)
	}
	if err := requestPrivileged(btrfsRequest{Op: "delete", Path: path}); err != nil {
		t.Fatalf("delete created subvolume: %v", err)
	}
}

func TestQEMUProjectDirectoryMigrationCreatesSubvolume(t *testing.T) {
	requireQEMUGuest(t)
	parent := os.Getenv("AGENTBOX_QEMU_PROJECT_PARENT")
	project := filepath.Join(parent, "migration-project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	const contents = "preserved project content\n"
	if err := os.WriteFile(filepath.Join(project, "payload.txt"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := initializeProjectSubvolume(project)
	if err != nil {
		t.Fatalf("initialize project subvolume: %v", err)
	}
	t.Cleanup(func() {
		_ = requestPrivileged(btrfsRequest{Op: "delete", Path: project})
		_ = os.RemoveAll(filepath.Dir(backup))
	})
	for _, path := range []string{project, backup} {
		got, err := os.ReadFile(filepath.Join(path, "payload.txt"))
		if err != nil {
			t.Fatalf("read migrated content from %s: %v", path, err)
		}
		if string(got) != contents {
			t.Fatalf("content at %s = %q, want %q", path, got, contents)
		}
	}
}

func TestQEMUDaemonRejectsPathsWithoutClientAccess(t *testing.T) {
	requireQEMUGuest(t)

	for _, test := range []struct {
		name        string
		source      string
		dest        string
		checkAbsent bool
	}{
		{
			name:        "inaccessible source",
			source:      os.Getenv("AGENTBOX_QEMU_PRIVATE_SOURCE"),
			dest:        os.Getenv("AGENTBOX_QEMU_DENIED_DESTINATION"),
			checkAbsent: true,
		},
		{
			name:   "inaccessible destination parent",
			source: os.Getenv("AGENTBOX_QEMU_SOURCE"),
			dest:   os.Getenv("AGENTBOX_QEMU_PRIVATE_DESTINATION"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requestPrivileged(btrfsRequest{Op: "snapshot", Source: test.source, Path: test.dest})
			if err == nil {
				t.Fatal("snapshot unexpectedly succeeded without client access")
			}
			if test.checkAbsent {
				if _, statErr := os.Lstat(test.dest); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("denied destination exists (stat error: %v)", statErr)
				}
			}
		})
	}

	privateCreate := os.Getenv("AGENTBOX_QEMU_PRIVATE_CREATE_PATH")
	if err := requestPrivileged(btrfsRequest{Op: "create", Path: privateCreate}); err == nil {
		t.Fatal("subvolume creation unexpectedly succeeded in an inaccessible directory")
	}
}

func TestQEMUDaemonRejectsUnsupportedOperation(t *testing.T) {
	requireQEMUGuest(t)

	conn, err := net.Dial("unix", daemonSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(map[string]string{"op": "exec", "path": "/tmp"}); err != nil {
		t.Fatal(err)
	}
	var response daemonResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, "unsupported operation") {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func requireQEMUGuest(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTBOX_QEMU_GUEST") != "1" {
		t.Skip("requires the QEMU integration guest")
	}
	if os.Geteuid() == 0 {
		t.Fatal("integration client must run as an unprivileged user")
	}
	for _, key := range []string{
		"AGENTBOX_QEMU_SOURCE",
		"AGENTBOX_QEMU_DESTINATION",
		"AGENTBOX_QEMU_PRIVATE_SOURCE",
		"AGENTBOX_QEMU_DENIED_DESTINATION",
		"AGENTBOX_QEMU_PRIVATE_DESTINATION",
		"AGENTBOX_QEMU_CREATE_PATH",
		"AGENTBOX_QEMU_PRIVATE_CREATE_PATH",
		"AGENTBOX_QEMU_PROJECT_PARENT",
	} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s is required", key)
		}
	}
}
