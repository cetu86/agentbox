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
- Go 1.26 or later to build the CLI.
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
After it completes, change to the installed project directory before running
other agentbox commands; the shell may still be located in the retained
original. Agentbox refuses to initialize a directory inside a migration
backup. The copy may take time and needs enough free space; inspect the new
project before removing the backup:

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
In **Container → Toolchains and tools**, use Space to select any combination of
Java (OpenJDK 21), Node.js, Go, Rust, and C/C++ (GCC/G++, CMake, Ninja, GDB,
and pkg-config), plus the Vim editor, the less pager and Playwright. The image
build installs everything selected. Playwright (selected by default) builds on
the official Playwright image with the browsers for the **Playwright version**
you set; without it the image is a plain Ubuntu with Node.js.

Each project's default config uses Podman, the `agentbox:latest` image, and
GitHub Copilot CLI. (Change `"runtime"` to `"docker"` if you use Docker.)
It stores snapshots in `.agent-snaps` next to the project. That directory
must reside on the same btrfs filesystem as the project: if `.agent-snaps` is a
separate mount, the mounted filesystem must be the project's btrfs filesystem.

The `agent_cmd` array configures the command run in the container. The
placeholder `{task}` is replaced with the task text:

```json
"agent_cmd": ["copilot", "--allow-all", "-i", "{task}"]
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
| `toolchains` | Optional list of toolchains to add to the agent image: `java`, `nodejs`, `go`, `rust`, `cpp`, `vim`, `less`, `playwright`, and `custom` for a custom base Containerfile. Defaults to `["playwright"]`; an explicit `[]` selects none. |
| `playwright_version` | Exact Playwright version (default `1.49.0`) used by the `playwright` toolchain to pick `mcr.microsoft.com/playwright:v<version>-noble`. It must equal the version your project resolves (pin `@playwright/test` without a caret), or Playwright will ask to download browsers. It cannot be combined with `custom`. |
| `custom_containerfile` | Optional path used when `custom` is selected in `toolchains`. It builds a custom base image, then agentbox adds the selected built-in toolchains and its common agent setup. Relative paths are resolved from the project directory; the Containerfile's directory is its build context. |
| `max_parallel` | Plan tasks whose agents run at the same time; the default is `3`. |
| `shm_size` | Maximum container shared-memory size; must be a finite size such as `8g` (Docker and Podman do not support unlimited); the default is `1g`. |
| `mount_templates` | Selected cache mount templates (`gradle`, `maven`, `npm`, `pip`) mapping host caches such as `~/.gradle/caches` into `/home/agent`; host directories are created if missing. Selectable in `agentbox config`. |
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

With the `playwright` toolchain, `agentbox build` uses the Playwright image for
`playwright_version` as the base and adds the other selected toolchains to it.
Use `--base` when building to override the base image for your host:

```sh
./agentbox build --base mcr.microsoft.com/playwright:v1.49.0-noble
```

`agentbox versions` starts a throwaway container from the built image and prints
the installed versions of git, Node.js, Copilot CLI, the language toolchains
and the bundled Playwright browser revisions, next to the configured Playwright
version.

You may select `custom` together with built-in toolchains and enter a custom
base Containerfile path in the TUI. Its build context is the directory
containing that file. The custom base must be Debian/Ubuntu-compatible and
provide Node.js and npm; agentbox adds its shared packages and Copilot CLI
after building the custom base. `--base` cannot be combined with a custom base
Containerfile.

## Interactive TUI

Run `agentbox` with no arguments in a terminal to open a project-scoped TUI
that lists the project's agent boxes (name, state, branch, changes) and
refreshes every two seconds. Select a box and press:

| Key | Action |
| --- | --- |
| `enter` | attach to the agent (shell/detach buttons work as in `attach`) |
| `n` | new box: asks for a name and an optional task, then starts and attaches |
| `s` | open the box's persistent shell |
| `v` | view the diff |
| `a` | accept (fetch the branch, delete the snapshot) |
| `m` | accept and `git merge` the branch into the checked-out branch |
| `p` | accept by promoting the snapshot |
| `d` | discard (asks for confirmation) |
| `x` | stop the container |
| `P` | open the plan screen (see below) |
| `b` / `c` | build the image / open the configuration TUI |
| `q` | quit |

The plan screen shows `.agentbox/plan.json` grouped by parallel wave, with each
task's state, `⚡auto` flag and running agent. Keys: `r` start/pause the
orchestrator (starts ready tasks up to `max_parallel`, collects finished
ones), `⏎` attach to a task's agent, `v` its diff, `a` approve and merge into
`plan/<name>`, `x` reject (feedback is sent back to the agent; empty feedback
blocks the task), `d` mark done (also overrules a blocked task), `t` retry a blocked task, `u` toggle
auto-approve (do this before starting; the task's work is then merged without
your review), `f` merge the completed plan into the current branch after
a diffstat confirmation.

Without a plan, the plan screen offers `n` (describe a goal; a planner agent
is started and attached), `⏎` (re-attach) and `c` (collect the plan after the
planner reported done). The same from the CLI: `agentbox plan "goal"`, then
`agentbox plan collect`. An invalid plan is sent back to the planner. Review
the collected plan (`agentbox plan check`, or `u` to set auto-approve) before
running it.

Accept, merge and promote are refused while the agent is running. Merging is
always explicit and confirmed. It requires the branch to be checked out
and its tracked files to be clean, and it runs `git merge --abort` if the merge
conflicts; the box is then kept so you can resolve it by hand.

In a directory that is not an agentbox project yet, the TUI starts a setup
wizard instead. It checks for a Git repository with commits (offering
`git init`; it never commits for you), a container runtime, a btrfs
filesystem and the daemon, offers to convert a regular directory into a
subvolume (the original is kept as a backup, and this needs
`sudo agentbox daemon`), writes the default configuration (or opens the
configuration TUI), offers to build the image, and then lands on the empty
box list.

## Plans: automated task orchestration

A plan is a machine-readable task graph in `.agentbox/plan.json` (commit it).
agentbox runs each task as its own box, starts independent tasks in parallel
(up to `max_parallel`), and you sign off on every task.

```json
{
  "name": "login",
  "tasks": [
    {"id": "tests", "title": "Login tests", "prompt": "Write failing tests for ..."},
    {"id": "impl", "prompt": "Make the tests pass ...", "depends_on": ["tests"]},
    {"id": "docs", "prompt": "Document the login flow ..."}
  ]
}
```

Fields: `name` and `id` are lowercase box-style names; `depends_on` lists task
ids (no cycles); optional `title`, `net` (`proxy`, `none`, `open`) and
`auto_approve`.

Workflow:

1. `agentbox plan "goal"` (or `n` on the TUI plan screen) starts a planner
   agent that proposes the plan. Talk it through, then `agentbox plan collect`
   validates and saves it. Invalid plans are sent back to the planner.
2. Review the plan: `agentbox plan check`, or the TUI plan screen (`P`).
   Press `u` on a task to auto-approve it: its work is then merged without
   your review, so use it only for tasks you trust.
3. `agentbox plan run` (or `r` in the TUI) starts every ready task, i.e. one
   whose dependencies are all accepted. Each task box starts from the
   integration branch `plan/<name>`, so it contains all approved work. Attach
   to any task to supervise it (`agentbox attach <id>`, or `⏎` in the TUI).
4. Agents are told to write a done marker (`.git/agentbox-done`) when they
   finish, or a blocked marker with a reason. A task moves to review when its
   marker appears. An agent that exits without a marker is flagged; mark it
   done by hand (`agentbox plan done <id>`, `d`).
5. `approve` merges the task into `plan/<name>`. If the merge conflicts, a
   resolver agent is started once on the conflict; if it fails, the task is
   blocked. `reject -m "feedback"` restarts the agent with your feedback;
   without feedback the task is blocked. A blocked or failed task also blocks
   its dependents until you `retry` it.
6. When every task is accepted, `agentbox plan finish` (or `f`) merges
   `plan/<name>` into your current branch after you confirm the diffstat, then deletes the merged `plan/<name>`
   branch and the plan's execution state (`plan.json` stays).

Verification is up to you: if a result needs testing, make "write the tests"
its own task before the implementation and review those tests first. The
state (`.agentbox/plan-state.json`) is git-ignored and survives restarts;
Ctrl-C on `plan run` stops the loop without losing progress. The done marker
is a request in the prompt, not a guarantee, hence the manual controls.

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

On an interactive terminal the agent's terminal is shown inside a box with a
titlebar. Detach from the agent without stopping it by clicking the `Detach`
button in the top right corner of the titlebar or pressing `F10`. The box is drawn by
agentbox itself, with no native libraries. The titlebar button needs mouse
reporting. When the program in the box does not use the mouse (a plain shell),
drag with the left button to select text: agentbox highlights it and copies it
to your clipboard through the terminal (OSC 52; your terminal must allow it).
Clipboard writes (OSC 52) that programs in the box make, such as Copilot's copy of a
selection, are passed on to your terminal.
Typing clears the highlight. Programs that do use the mouse get it forwarded,
and `Shift` held while dragging still selects natively in most terminals, (drag and motion
tracking and both SGR and legacy encodings are supported). Because the runtime does not replay what the
agent printed before you attached, agentbox recovers the agent's terminal modes
(alternate screen, mouse tracking) from the container log on every attach. If stdin or stdout is not a terminal, or the
terminal is smaller than 20x5, the runtime's plain attach is used. You can
close the terminal after detaching; the agent container continues
running.

The titlebar also has a `Shell` button (`F9`) left of `Detach`. It detaches the agent view and
shows a Bash shell on the same snapshot instead; the button then reads `Agent`
and switches back. The shell runs in its own persistent container
(`agentbox-NAME.shell`), so switching back to the agent only detaches it:
running commands and shell state are kept until you return. The shell is
killed when the box ends (`accept` or `discard`, or when a new box with the
same name is started). Exiting the shell with `exit` removes it and returns to
the agent. Switching is not available with the plain attach fallback.

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
It also requires the configured container runtime to be reachable; if not,
agentbox reports the runtime error.

`attach` reconnects your terminal to a running agent. You can interact with it
and detach again with the titlebar button or `F10`.

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

To fetch and also merge the branch into the project's checked-out branch,
use `--merge` (it cannot be combined with `--promote`). A conflicting merge
is aborted and the box and branch are kept. After a successful merge the
merged branch is deleted from the project:

```sh
./agentbox accept --merge refactor-api
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
agentbox                                  (TUI; needs a terminal)
agentbox init [--subvolume]
agentbox configure [--project DIR]
agentbox finish [--project DIR]
agentbox daemon
agentbox build [--base IMAGE]
agentbox versions [--project DIR]         (tool versions resolved in the built image)
agentbox start [--project DIR] [--net proxy|none|open] [--base REF] [--no-branch] NAME [TASK...]
agentbox resume [--project DIR] [--net proxy|none|open] NAME [TASK...]
agentbox attach [--project DIR] NAME
agentbox watch [--project DIR] NAME
agentbox shell [--project DIR] [--net proxy|none|open] NAME
agentbox path [--project DIR] NAME
agentbox diff [--project DIR] NAME
agentbox list [--project DIR]
agentbox accept [--project DIR] [--promote | --merge] [--force] [--keep] NAME
agentbox discard [--project DIR] NAME
agentbox plan ["GOAL" | new GOAL | collect | check | status | run [--once] | approve ID | reject ID [-m MSG] | done ID | retry ID | finish]
agentbox proxy up|down|logs
```

`--project DIR` overrides the configured project path for that invocation.
Common flags must come before positional arguments.
