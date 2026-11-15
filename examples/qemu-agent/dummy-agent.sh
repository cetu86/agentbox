#!/bin/sh
set -eu

task="${1:-}"
runs=0
if [ -f agent-runs ]; then
	runs="$(cat agent-runs)"
fi
runs=$((runs + 1))
printf '%s\n' "$runs" >agent-runs
printf 'DUMMY_AGENT_RUN=%s TASK=%s\n' "$runs" "$task"

case "$task" in
interactive)
	echo "DUMMY_AGENT_READY: should this change be approved?"
	IFS= read -r answer || exit 1
	printf 'DUMMY_AGENT_ANSWER=%s\n' "$answer"
	IFS= read -r _ || exit 1
	echo "DUMMY_AGENT_CONTINUING"
	printf 'approved: %s\n' "$answer" >agent-result.txt
	;;
finish)
	echo "DUMMY_AGENT_FINISHED"
	printf 'completed run %s\n' "$runs" >agent-result.txt
	sleep 1
	;;
*)
	echo "unexpected dummy task: $task" >&2
	exit 2
	;;
esac
