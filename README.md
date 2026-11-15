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

Initialize from the project directory. For an existing btrfs subvolume, run:

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

Review the generated config at `$XDG_CONFIG_HOME/agentbox/config.json` (or the
platform's user config directory), then build the image:

```sh
./agentbox build
```

The default config uses Podman, the `agentbox:latest` image, and GitHub Copilot
CLI. (Change `"runtime"` to `"docker"` if you use Docker.)
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
| `project` | Absolute path to the host project. |
| `snapshot_dir` | Directory for btrfs snapshots; it must reside on the project's btrfs filesystem, including when it is a separate mount. |
| `runtime` | `podman` or `docker`. |
| `image` | Image used for agent and shell containers. |
| `shm_size` | Maximum container shared-memory size; must be a finite size such as `8g` (Docker and Podman do not support unlimited); the default is `1g`. |
| `mounts` | Extra `host:container[:opts]` volume mounts. `~/` paths are expanded. |
| `env` | Names of host environment variables to pass through when set. The defaults include `GH_TOKEN` and `GITHUB_TOKEN`. |
| `network` | `open` (default; unrestricted container networking), `none`, or `proxy` (domain allowlist). |
| `allowed_domains` | Destination domains allowed when `network` is `proxy`. |
| `proxy_image` | Container image used when `network` is `proxy`. |
| `egress_network` | Runtime network used by the proxy when `network` is `proxy`. |

Do not put token values or other secrets in the config file. Configure
credentials in the host environment and list only their variable names in
`env`.

The `Containerfile` defaults to a Playwright image. Use `--base` when building
to select a compatible base image for your host and project's Playwright
version:

```sh
./agentbox build --base mcr.microsoft.com/playwright:v1.49.0-noble
```

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

Detach from the agent without stopping it by pressing **Ctrl-P, then Ctrl-Q**
(the container runtime's default detach sequence). You can close the terminal
after detaching; the agent container continues running.

Useful follow-up commands:

```sh
./agentbox list
./agentbox attach refactor-api
./agentbox watch refactor-api
./agentbox shell refactor-api
./agentbox path refactor-api
./agentbox resume refactor-api "Continue by fixing the failing test"
```

`list` shows each session's branch, whether its working tree has uncommitted
changes, and the container state (running, exited, or none).

`attach` reconnects your terminal to a running agent. You can interact with it
and detach again with **Ctrl-P, Ctrl-Q**.

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

## Daemon integration tests

The QEMU integration suite boots a minimal Alpine Linux guest, creates a real
btrfs filesystem in a temporary virtual disk, and tests both the daemon
protocol and the CLI workflow. The end-to-end workflow uses a small in-guest runtime fixture to build an
image containing the deterministic dummy agent (no real agent or credentials),
creates an example Git project, and exercises interactive
start/detach/reattach, watch, shell, resume, list, accept, and discard:

```sh
./scripts/qemu-integration.sh
```

The first run downloads Alpine's `v3.22` x86_64 netboot initramfs and
`linux-virt` package to
`$XDG_CACHE_HOME/agentbox/qemu/alpine-v3.22-x86_64/` (normally
`~/.cache/agentbox/qemu/alpine-v3.22-x86_64/`). These are reused on later runs;
the matching kernel and module package is selected from Alpine's package index. The guest
installs btrfs tools, Expect, Git, tmux, and other test utilities at boot. Its
runtime fixture simulates the container runtime interface for session/TTY
workflow tests; it does not provide container isolation or validate Docker or
Podman behavior. The daemon, btrfs snapshots, and agentbox CLI workflow are
real. The host harness requires Bash, QEMU x86_64, `qemu-img`, Go, `curl`,
`cpio`, `tar`, and network access on the first run and while the guest installs
packages. The harness and guest test steps are split across `scripts/qemu/`,
with the test image and project under `examples/`.

## Command reference

```text
agentbox init [--subvolume]
agentbox daemon
agentbox build [--base IMAGE]
agentbox start [--project DIR] [--net proxy|none|open] [--base REF] [--no-branch] NAME [TASK...]
agentbox resume [--project DIR] [--net proxy|none|open] NAME [TASK...]
agentbox attach [--project DIR] NAME
agentbox watch [--project DIR] NAME
agentbox shell [--project DIR] [--net proxy|none|open] NAME
agentbox path [--project DIR] NAME
agentbox list [--project DIR]
agentbox accept [--project DIR] [--promote] [--force] [--keep] NAME
agentbox discard [--project DIR] NAME
agentbox proxy up|down|logs
```

`--project DIR` overrides the configured project path for that invocation.
Common flags must come before positional arguments.
