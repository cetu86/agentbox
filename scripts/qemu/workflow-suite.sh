#!/bin/sh
set -eu

echo "starting CLI workflow suite with its in-guest runtime fixture"
export AGENTBOX_TEST_RUNTIME_STATE=/mnt/test/workflow/runtime
mkdir -p "$AGENTBOX_TEST_RUNTIME_STATE/images"
mkdir -p /mnt/test/workflow
btrfs subvolume create /mnt/test/workflow/project
cp -a /qemu/examples/qemu-project/. /mnt/test/workflow/project/
cd /mnt/test/workflow/project
git config --global --add safe.directory '*'
git init -q
git config --global user.name "Agentbox QEMU Test"
git config --global user.email "agentbox-qemu@example.invalid"
git add .
git commit -qm "initial project"

export XDG_CONFIG_HOME=/mnt/test/workflow/config
/agentbox init
grep -q '"project": "/mnt/test/workflow/project"' "$XDG_CONFIG_HOME/agentbox/config.json" || {
	echo "agentbox init did not configure the project path" >&2
	exit 1
}
cat >"$XDG_CONFIG_HOME/agentbox/config.json" <<'CONFIG'
{
  "project": "/mnt/test/workflow/project",
  "snapshot_dir": "/mnt/test/workflow/.agent-snaps",
  "runtime": "/qemu/runtime.sh",
  "image": "agentbox-qemu-dummy",
  "agent_cmd": ["/usr/local/bin/dummy-agent", "{task}"],
  "shm_size": "64m",
  "network": "none"
}
CONFIG
/agentbox build

echo "testing interactive start, watch, shell, attach, list, and accept"
expect /qemu/expect/start-interactive.exp
snapshot=$(/agentbox path interactive)
[ "$snapshot" = /mnt/test/workflow/.agent-snaps/interactive ] || {
	echo "unexpected snapshot path: $snapshot" >&2
	exit 1
}
/agentbox list | grep -q 'interactive.*Up ' || {
	echo "running session missing from agentbox list" >&2
	exit 1
}
if timeout 3 /agentbox watch interactive >/tmp/agentbox-watch.log 2>&1; then
	echo "agentbox watch unexpectedly exited while the agent was waiting" >&2
	exit 1
else
	watch_status=$?
	[ "$watch_status" -eq 124 ] || {
		cat /tmp/agentbox-watch.log
		echo "agentbox watch exited with unexpected status $watch_status" >&2
		exit 1
	}
fi
grep -q "DUMMY_AGENT_ANSWER=approve staging" /tmp/agentbox-watch.log || {
	cat /tmp/agentbox-watch.log
	echo "agentbox watch did not show agent output" >&2
	exit 1
}
expect /qemu/expect/shell.exp interactive
expect /qemu/expect/attach-finish.exp interactive
/agentbox list | grep -q 'interactive.*Exited ' || {
	echo "completed session missing exited state in agentbox list" >&2
	exit 1
}
/agentbox accept interactive
git -C /mnt/test/workflow/project show agent/interactive:agent-result.txt |
	grep -q "approved: approve staging" || {
	echo "accepted agent result is missing from the project Git history" >&2
	exit 1
}
[ ! -e "$snapshot" ] || {
	echo "accepted snapshot still exists" >&2
	exit 1
}
[ ! -e "$AGENTBOX_TEST_RUNTIME_STATE/agentbox-interactive" ] || {
	echo "accepted agent container still exists" >&2
	exit 1
}

echo "testing resume and discard"
/agentbox start resume-check finish
grep -qx '1' "$(/agentbox path resume-check)/agent-runs" || {
	echo "initial dummy run count is incorrect" >&2
	exit 1
}
/agentbox resume resume-check finish
grep -qx '2' "$(/agentbox path resume-check)/agent-runs" || {
	echo "resumed dummy run did not reuse the existing snapshot" >&2
	exit 1
}
/agentbox discard resume-check
[ ! -e /mnt/test/workflow/.agent-snaps/resume-check ] || {
	echo "discard did not remove the snapshot" >&2
	exit 1
}
[ ! -e "$AGENTBOX_TEST_RUNTIME_STATE/agentbox-resume-check" ] || {
	echo "discard did not remove the agent container" >&2
	exit 1
}
