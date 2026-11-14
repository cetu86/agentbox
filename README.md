# agentbox

`agentbox` is a Linux CLI for running coding agents in disposable containers
against copy-on-write btrfs snapshots of a project. Each agent gets its own
working tree and Git branch, while the original project remains untouched
until you choose to bring the work back.

The CLI can use Podman or Docker and builds an agent image from the embedded
`Containerfile`. By default, agents run GitHub Copilot CLI with network access
restricted through an allowlisting proxy.

## Requirements

- Linux with btrfs and `btrfs-progs`; the project must be a btrfs subvolume.
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

Run `agentbox init` from the project directory to create the default
configuration at `$XDG_CONFIG_HOME/agentbox/config.json` (or the platform's
user config directory). Review the generated file before building the image:

```sh
./agentbox init
./agentbox build
```

Start the privileged btrfs companion daemon in a separate terminal (or
configure it as a system service):

```sh
sudo ./agentbox daemon
```

The daemon listens on `/run/agentbox/agentbox.sock`. The socket uses Unix
peer credentials to identify each client. Local users can connect, but each
request is authorized as that peer's UID and groups. For snapshots, the
daemon verifies access to the source and create access to the destination's
parent; for deletion, it verifies access to the target and delete access to
its parent. It accepts only snapshot and subvolume-delete operations, not
arbitrary commands or btrfs arguments. Keep the daemon running while using
`agentbox`; stop it with Ctrl-C or SIGTERM.

The default config uses Podman, the `agentbox:latest` image, and GitHub Copilot
CLI. It stores snapshots in `.agent-snaps` next to the project. The snapshot
directory must be on the same btrfs filesystem as the project. Change
`"runtime"` to `"docker"` if you use Docker.

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
| `snapshot_dir` | Directory for btrfs snapshots; keep it on the project's btrfs filesystem. |
| `runtime` | `podman` or `docker`. |
| `image` | Image used for agent and shell containers. |
| `shm_size` | Container shared-memory size; the default is `1g`. |
| `mounts` | Extra `host:container[:opts]` volume mounts. `~/` paths are expanded. |
| `env` | Names of host environment variables to pass through when set. The defaults include `GH_TOKEN` and `GITHUB_TOKEN`. |
| `network` | `proxy` (default), `none`, or `open`. |
| `allowed_domains` | Destination domains allowed by the proxy. |
| `proxy_image` | Container image for the Squid allowlisting proxy. |
| `egress_network` | Runtime network used by the proxy to reach outside networks. |

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
default, then launches the configured agent in a container. The snapshot is
mounted read/write at the project's original absolute path inside the
container. The container's home directory is temporary; the snapshot is where
project changes are persisted.

Useful follow-up commands:

```sh
./agentbox list
./agentbox shell refactor-api
./agentbox path refactor-api
./agentbox resume refactor-api "Continue by fixing the failing test"
```

`resume` reuses an existing snapshot and does not create another branch.
`shell` opens Bash in a running agent container, or starts a fresh container
with Bash if the agent container is no longer running.

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

The default `proxy` mode attaches the agent container to an internal container
network and routes HTTP(S) requests through Squid, which allows only the
domains in `allowed_domains`. Start, stop, or inspect the proxy with:

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

`none` disables container networking. `open` gives the container normal
runtime networking and bypasses the allowlist. The proxy allowlist is a
domain-level restriction, not a complete security boundary. An agent has
write access to its mounted snapshot; review its changes before accepting
them, and only run agents and tool configurations you trust.

## Daemon integration tests

The daemon integration suite boots a minimal Alpine Linux guest under QEMU,
creates a real btrfs filesystem in a temporary virtual disk, and exercises
snapshot, delete, denied-path, and unsupported-operation requests against the
running daemon:

```sh
./scripts/qemu-integration.sh
```

The first run downloads Alpine's `v3.22` x86_64 netboot initramfs and
`linux-virt` package to `$XDG_CACHE_HOME/agentbox/qemu/` (normally
`~/.cache/agentbox/qemu/`). These are reused on later runs; the matching
kernel and module package is selected from Alpine's package index. Alpine's
`btrfs-progs` package is installed inside the guest at boot. The harness
requires Bash, QEMU x86_64, `qemu-img`, Go, `curl`, `cpio`, `tar`, and network
access on the first run and while the guest installs its packages.

## Command reference

```text
agentbox init
agentbox daemon
agentbox build [--base IMAGE]
agentbox start [--project DIR] [--net proxy|none|open] [--base REF] [--no-branch] NAME [TASK...]
agentbox resume [--project DIR] [--net proxy|none|open] NAME [TASK...]
agentbox shell [--project DIR] [--net proxy|none|open] NAME
agentbox path [--project DIR] NAME
agentbox list [--project DIR]
agentbox accept [--project DIR] [--promote] [--force] [--keep] NAME
agentbox discard [--project DIR] NAME
agentbox proxy up|down|logs
```

`--project DIR` overrides the configured project path for that invocation.
Common flags must come before positional arguments.
