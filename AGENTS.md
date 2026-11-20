# Agent instructions

## Project

`agentbox` is a Go 1.26+ CLI for managing per-agent btrfs snapshots and running
coding agents inside Podman or Docker containers. `golang.org/x/sys` provides
Linux syscall support. It is pure Go (no CGO).

## Layout

- `main.go` defines CLI dispatch, flag parsing, and command handlers.
- `config.go` defines configuration loading and defaults.
- `snapshot.go` contains Git helpers and snapshot lifecycle coordination.
- `daemon.go` implements the privileged btrfs companion daemon and the
  authenticated Unix-socket client protocol. Keep its operations narrow and
  authorize filesystem paths using the connecting peer's credentials.
- `daemon_qemu_test.go` contains integration tests run by
  `scripts/qemu-integration.sh` inside the Alpine QEMU guest.
- `container.go` builds agent container arguments and manages networking and
  the allowlisting proxy.
- `attach_frame.go` implements the framed attach UI: the container's terminal
  runs on a pty, is emulated with `charmbracelet/x/vt`, and is drawn in a box
  with a titlebar holding a detach button and a shell/agent switch button
  (F9). Detaching or switching kills the attach client, which leaves the
  container running. The shell is a sibling container `agentbox-<name>.shell`
  (see `ensureShell`/`removeShell` in `container.go`); `accept`, `discard`
  and a new `start` remove it.
- `box_ops.go` holds box operations shared by the CLI and the TUI (list,
  create, accept incl. `--merge`, discard, diff, init) and `captureCommands`,
  which redirects command echo/output so the TUI is not corrupted.
- `tui.go` is the project-scoped bubbletea TUI (`agentbox` with no args);
  interactive work (attach, build, configure) runs through `tea.Exec`.
  `tui_wizard.go` is the first-run setup wizard; `wizardChoices` is pure and
  tested via an injectable `projectProbe`.
- `Containerfile` defines the agent image and is embedded by `main.go`; keep
  the embedded file and the build behavior in sync.

## Development

Run the checks before handing off changes:

```sh
go test ./...
go build ./...
```

Run the daemon integration suite when changing daemon protocol or filesystem
authorization behavior:

```sh
./scripts/qemu-integration.sh
```

When changing behavior, add focused tests where possible, especially for
argument construction, config defaults, and filesystem/Git behavior that can
be tested without invoking btrfs or a container runtime.

## Implementation guidance

- Keep CLI behavior and help text in `main.go` aligned with command handlers
  and configuration options.
- Preserve the JSON config field names and documented defaults unless the
  change intentionally updates the user-facing configuration.
- External programs are invoked directly with `os/exec`; do not construct
  shell command strings. Return operational errors to the caller rather than
  silently treating failures as success.
- Keep privileged btrfs work inside `daemon.go`; never add a generic command
  or unrestricted path operation to the daemon protocol. Subvolume creation
  must be limited to a parent directory the requesting user can write.
- Snapshot creation and promotion affect user project data. Preserve their
  safety checks and rollback behavior, and avoid broad or irreversible
  filesystem operations.
- `agentbox init --subvolume` migrates a regular project directory by copying
  it to a new subvolume and retaining the original as a recovery backup.
- Keep btrfs requirements, snapshot filesystem constraints, container mount
  behavior, and network-mode descriptions accurate in `README.md` when those
  behaviors change.
