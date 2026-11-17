# agentbox

`agentbox` is a Linux CLI for running coding agents in disposable containers
against copy-on-write btrfs snapshots of a project. Each agent gets its own
working tree and Git branch, while the original project remains untouched
until you choose to bring the work back.

The CLI can use Podman or Docker and builds an agent image from the embedded
`Containerfile`. By default, agents run GitHub Copilot CLI with normal
container network access and no proxy or domain filtering.

## Requirements

- Linux with btrfs and `btrfs-progs`; the project must be on a btrfs
  filesystem. Use `agentbox init --subvolume` to copy a regular project
  directory into a subvolume; that migration also requires `cp` with
  `--reflink=auto` support.
- Go 1.22 or later to build the CLI.
- Podman or Docker, configured for the current user.
- A Git project.
- Root access to start the companion daemon.
- Network access to pull the container image and, in proxy mode, the proxy
  image.

## Build and configure

Build the executable from this directory:

```sh
go build -o agentbox .
```

Start the privileged btrfs companion daemon in a separate terminal (or
configure it as a system service):

```sh
sudo ./agentbox daemon
```

The daemon listens on `/run/agentbox/agentbox.sock`. The socket uses Unix
peer credentials to identify each client. Local users can connect, but each
request is authorized as that peer's UID and groups. The daemon verifies
access to snapshot sources and targets, and only creates subvolumes in a
directory where the client has create access. It accepts only specific
subvolume create, snapshot, and delete operations, not arbitrary commands or
btrfs arguments. Keep the daemon running while using `agentbox`; stop it with
Ctrl-C or SIGTERM.

Initialize each project from its project directory. The config file is
per-project, so initializing one project does not modify any other project's
settings. Commands select the config for the current directory or its nearest
configured parent; use `--project DIR` to select a project explicitly.

For an existing btrfs subvolume, run:

```sh
./agentbox init
```

For a regular directory on a btrfs filesystem, run `init --subvolume` instead
of `init`. It copies the project into a sibling subvolume, switches it into
place, and keeps the original at `.agentbox-init-*/original` for recovery.
The copy may take time and needs enough free space; inspect the new project
before removing the backup:

```sh
./agentbox init --subvolume
```

`init` prints the path to this project's config under
`$XDG_CONFIG_HOME/agentbox/projects/` (or the platform's user config
directory). Configure settings interactively, then build the image:

```sh
./agentbox configure
./agentbox build
```

The full-screen menu groups settings by project, agent, container, network,
and credentials.
Use the arrow keys and Enter to navigate or edit fields. Press `s` to save and
exit, or `q`/Ctrl-C to discard. Multiline settings such as the agent command
use one value per line; Ctrl-S applies those edits. Press `q` or Ctrl-C to
exit; if anything changed, agentbox asks whether to save, discard, or return
to the menu. Use `agentbox config` as a shorter alias, or
`agentbox configure --project DIR` to configure another registered project.
Set `ACCESSIBLE=1` to use the screen-reader-friendly prompt mode instead of
the full-screen menu.
In **Container → Language toolchains**, use Space to select any combination of
Java (OpenJDK 21), Node.js, Go, Rust, and C/C++ (GCC/G++, CMake, Ninja, GDB,
and pkg-config). The image build installs all selected toolchains; selecting
none keeps the existing Playwright-based image.

Each project's default config uses Podman, the `agentbox:latest` image, and
GitHub Copilot CLI. (Change `"runtime"` to `"docker"` if you use Docker.)
It stores snapshots in `.agent-snaps` next to the project. That directory
must reside on the same btrfs filesystem as the project: if `.agent-snaps` is a
separate mount, the mounted filesystem must be the project's btrfs filesystem.

The `agent_cmd` array configures the command run in the container. The
placeholder `{task}` is replaced with the task text:

```json
"agent_cmd": ["copilot", "--allow-all-tools", "-i", "{task}"]
```

Adjust the command and flags for your installed agent CLI. The default grants
Copilot CLI permission to use all tools; review the CLI's permission options
and choose settings appropriate for your project.

Other useful configuration fields:

| Field | Purpose |
| --- | --- |
| `project` | Absolute path to this project's btrfs subvolume. |
| `snapshot_dir` | Directory for this project's btrfs snapshots; it must reside on the same btrfs filesystem, including when it is a separate mount. |
| `runtime` | `podman` or `docker`. |
| `image` | Image used for agent and shell containers. |
| `toolchains` | Optional list of toolchains to add to the agent image: `java`, `nodejs`, `go`, `rust`, `cpp`, and `custom` for a custom base Containerfile. |
| `custom_containerfile` | Optional path used when `custom` is selected in `toolchains`. It builds a custom base image, then agentbox adds the selected built-in toolchains and its common agent setup. Relative paths are resolved from the project directory; the Containerfile's directory is its build context. |
| `shm_size` | Maximum container shared-memory size; must be a finite size such as `8g` (Docker and Podman do not support unlimited); the default is `1g`. |
| `mounts` | Extra `host:container[:opts]` volume mounts. `~/` paths are expanded. |
| `env` | Names of host environment variables to pass through when set. The defaults include `GH_TOKEN` and `GITHUB_TOKEN`. |
| `github_token` | Optional GitHub personal access token for Copilot. When set, it overrides host `GH_TOKEN` and `GITHUB_TOKEN` for agent and shell containers. |
| `network` | `open` (default; unrestricted container networking), `none`, or `proxy` (domain allowlist). |
| `allowed_domains` | Destination domains allowed when `network` is `proxy`. |
| `proxy_image` | Container image used when `network` is `proxy`. |
| `egress_network` | Runtime network used by the proxy when `network` is `proxy`. |

Set a Copilot token in the TUI's **Credentials** menu or in the project's
`github_token` field. It is stored as plaintext in that project's JSON config,
which agentbox writes with owner-only permissions (directory `0700`, file
`0600`). Keep that file private and never commit or share it. The token is
passed to the container runtime through its process environment, not as a
command-line argument. If `github_token` is empty, the existing host
environment-variable passthrough continues to work.

## Finish with a project

After accepting or discarding all agent snapshots, run this from the project
directory:

```sh
./agentbox finish
```

`finish` refuses while the project's snapshot directory contains entries. It
copies the project into a regular directory, switches it into place, retains
the former subvolume at `.agentbox-finish-*/original` for recovery, and removes
that project's config file. Inspect the new directory before manually deleting
the retained backup subvolume.

The built-in Containerfile defaults to a Playwright image. `agentbox build`
adds the selected language toolchains to it. Use `--base` when building to
select a compatible Playwright base image for your host:

```sh
./agentbox build --base mcr.microsoft.com/playwright:v1.49.0-noble
```

You may select `custom` together with built-in toolchains and enter a custom
base Containerfile path in the TUI. Its build context is the directory
containing that file. The custom base must be Debian/Ubuntu-compatible and
provide Node.js and npm; agentbox adds its shared packages and Copilot CLI
after building the custom base. `--base` cannot be combined with a custom base
Containerfile.

## Run an agent

Start a new agent session with a unique, lowercase name and a task:

```sh
./agentbox start refactor-api "Refactor the API client and run its tests"
```

This creates a btrfs snapshot, creates the Git branch `agent/refactor-api` by
default, then launches the configured agent in a persistent interactive
container. Agentbox attaches your terminal so you can see its progress and
answer questions. The snapshot is mounted read/write at the project's
original absolute path inside the container. The container's home directory
is temporary; the snapshot is where project changes are persisted.
For the default Copilot command, agentbox seeds Copilot's temporary config with
the project path in its `trustedFolders` list, so Copilot trusts the project on
each launch without persisting its home directory.

Detach from the agent without stopping it by pressing `Ctrl+Space`, then
`Ctrl+Q`. Agentbox configures this sequence because Copilot uses Ctrl-P.
You can close the terminal after detaching; the agent container continues
running.

Useful follow-up commands:

```sh
./agentbox list
./agentbox attach refactor-api
./agentbox watch refactor-api
./agentbox shell refactor-api
./agentbox path refactor-api
./agentbox diff refactor-api
./agentbox resume refactor-api "Continue by fixing the failing test"
```

`list` shows each session's branch, whether its working tree has uncommitted
changes, and the container state (running, exited, or none).

`attach` reconnects your terminal to a running agent. You can interact with it
and detach again with `Ctrl+Space`, then `Ctrl+Q`.

`watch` follows the agent's output without attaching stdin, so it is useful
for watching progress from another terminal while someone else is attached.
It can also show the output of an exited container.

`shell` opens an interactive Bash in the agent's existing container. If that
container has stopped, it starts a new shell container on the same snapshot,
so you can inspect or edit the agent's files without starting the agent again.

`path` prints the snapshot's host filesystem path. This is useful for opening
the agent's working tree in a host editor, for example:

```sh
code "$(./agentbox path refactor-api)"
```

`diff` shows the session's changes from the files present when it started,
including committed, staged, unstaged, and untracked non-ignored files. It does
not accept changes, stop the container, or modify the session, so you can review
the diff and resume the same agent to request further changes.

`resume` starts the configured agent again in a new container using the same
snapshot and branch; it does not create another snapshot or branch. It removes
the previous container if it has exited, then attaches your terminal to the
new run. Pass a new task to continue or redirect the work. Use `shell` instead
when you want an interactive terminal rather than the agent.

When the work is ready, stop the container and choose how to bring it back:

```sh
./agentbox accept refactor-api
```

By default, `accept` commits dirty snapshot changes, fetches the snapshot's
branch into the main project, and prints the merge command. It then deletes
the snapshot. Use `--keep` to retain it:

```sh
./agentbox accept --keep refactor-api
```

Alternatively, promote the entire snapshot (including build artifacts and
caches) to become the project directory:

```sh
./agentbox accept --promote refactor-api
```

Promotion refuses to replace a dirty project unless `--force` is supplied.
It keeps the previous project as `_pre-refactor-api` in the snapshot
directory. Inspect the result before deleting that backup.

To remove an agent container and snapshot without accepting the work:

```sh
./agentbox discard refactor-api
```

`discard` deletes the snapshot and its changes.

## Network modes

The default `open` mode gives the agent container normal runtime networking,
with no proxy or domain filtering. To opt into `proxy` mode, set `network` to
`proxy` in the config or pass `--net proxy`. It attaches the agent container to
an internal container network and routes HTTP(S) requests through Squid, which
allows only the domains in `allowed_domains`. Start, stop, or inspect the proxy
with:

```sh
./agentbox proxy up
./agentbox proxy logs
./agentbox proxy down
```

You can override the configured mode on a command:

```sh
./agentbox start --net none offline-task "Work without network access"
./agentbox start --net open online-task "Work with unrestricted container networking"
```

`none` disables container networking. The proxy allowlist is a
domain-level restriction, not a complete security boundary. An agent has
write access to its mounted snapshot; review its changes before accepting
them, and only run agents and tool configurations you trust.

## Shell completion

Enable shell completion for `bash` or `zsh`:

```sh
# Bash
./agentbox completion bash | sudo tee /etc/bash_completion.d/agentbox

# Zsh
./agentbox completion zsh | sudo tee /usr/share/zsh/site-functions/_agentbox
```

Or add to your shell configuration file:

```sh
# Bash (~/.bashrc)
eval "$(agentbox completion bash)"

# Zsh (~/.zshrc)
eval "$(agentbox completion zsh)"
```

Completion includes command names, flags, flag values, and dynamic session
names. The `--base` flag completes Git branch names; `--project` completes
directory paths; `--net` completes network modes (`open`, `none`, `proxy`).

## Daemon integration tests

The Go-based QEMU integration suite boots an Alpine Linux guest, creates a real
btrfs filesystem in a temporary virtual disk, and tests the daemon protocol
and CLI workflows. It runs separate Docker and Podman end-to-end cases in
addition to a lightweight runtime-fixture test. The Docker case starts a
Docker daemon with overlay2 storage; the Podman case uses vfs storage. Both
build a minimal local test image containing a deterministic dummy agent (no
real coding agent or credentials), then exercise interactive
start, client disconnect and reattach, watch, shell, resume, list, accept, and
discard:

```sh
./scripts/qemu-integration.sh
```

The first run downloads Alpine's `v3.22` x86_64 netboot initramfs and
`linux-virt` package to
`$XDG_CACHE_HOME/agentbox/qemu/alpine-v3.22-x86_64/` (normally
`~/.cache/agentbox/qemu/alpine-v3.22-x86_64/`). These are reused on later runs;
the matching kernel and module package is selected from Alpine's package
index. The guest installs btrfs tools, Git, tmux, Docker, and Podman at boot.
Each real-runtime case builds its test image from `examples/qemu-agent/`; it
does not use the production image or contact an agent service. The host
requires Go, QEMU x86_64, `qemu-img`, `cpio`, and network access on the first
run and while the guest installs packages and pulls the Alpine base image. The
shell entry point only starts the Go runner; the guest shell script bootstraps
Alpine. Test orchestration, runtime fixtures, and interactive PTY checks are
implemented in Go under `scripts/qemu/` and `qemu_workflow_test.go`.

## Command reference

```text
agentbox init [--subvolume]
agentbox configure [--project DIR]
agentbox finish [--project DIR]
agentbox daemon
agentbox build [--base IMAGE]
agentbox start [--project DIR] [--net proxy|none|open] [--base REF] [--no-branch] NAME [TASK...]
agentbox resume [--project DIR] [--net proxy|none|open] NAME [TASK...]
agentbox attach [--project DIR] NAME
agentbox watch [--project DIR] NAME
agentbox shell [--project DIR] [--net proxy|none|open] NAME
agentbox path [--project DIR] NAME
agentbox diff [--project DIR] NAME
agentbox list [--project DIR]
agentbox accept [--project DIR] [--promote] [--force] [--keep] NAME
agentbox discard [--project DIR] NAME
agentbox proxy up|down|logs
```

`--project DIR` overrides the configured project path for that invocation.
Common flags must come before positional arguments.
