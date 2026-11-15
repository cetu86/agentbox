#!/bin/sh
set -eu

state_root="${AGENTBOX_TEST_RUNTIME_STATE:?}"
subcommand="${1:-}"
shift || true

session_exists() {
	tmux -f /qemu/tmux.conf has-session -t "$1" 2>/dev/null
}

case "$subcommand" in
build)
	image=
	file=
	context=
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-t)
			image="$2"
			shift 2
			;;
		-f)
			file="$2"
			shift 2
			;;
		*)
			context="$1"
			shift
			;;
		esac
	done
	[ -n "$image" ] && [ -f "$file" ] && [ -f "$context/Containerfile" ] || {
		echo "test runtime build received invalid image or Containerfile" >&2
		exit 1
	}
	image_dir="$state_root/images/$image"
	mkdir -p "$image_dir"
	cp /qemu/examples/qemu-agent/dummy-agent.sh "$image_dir/dummy-agent"
	chmod 0755 "$image_dir/dummy-agent"
	printf 'built test image %s using %s\n' "$image" "$file"
	;;
ps)
	name=
	while [ "$#" -gt 0 ]; do
		if [ "$1" = "--filter" ]; then
			name="${2#name=^}"
			name="${name%?}"
			shift 2
		else
			shift
		fi
	done
	[ -n "$name" ] || exit 0
	dir="$state_root/$name"
	[ -d "$dir" ] || exit 0
	if session_exists "$name"; then
		printf 'Up 1 minute\n'
	else
		printf 'Exited (0) 1 second ago\n'
	fi
	;;
run)
	name=
	workdir=
	volume=
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--name)
			name="$2"
			shift 2
			;;
		-v)
			volume="$2"
			shift 2
			;;
		-w)
			workdir="$2"
			shift 2
			;;
		--hostname|--shm-size|-e|--tmpfs|--user|--network)
			shift 2
			;;
		-d|-i|-t|-it|--rm)
			shift
			;;
		*)
			image="$1"
			shift
			break
			;;
		esac
	done
	[ -n "${image:-}" ] && [ -x "$state_root/images/$image/dummy-agent" ] || {
		echo "test runtime image was not built: ${image:-<missing>}" >&2
		exit 1
	}
	[ -n "$workdir" ] || {
		echo "test runtime did not receive a working directory" >&2
		exit 1
	}
	[ -n "$volume" ] || {
		echo "test runtime did not receive the snapshot mount" >&2
		exit 1
	}
	snapshot="${volume%%:*}"
	mount_target="${volume#*:}"
	[ "$mount_target" = "$workdir" ] || {
		echo "test runtime mount does not match working directory" >&2
		exit 1
	}
	if [ -z "$name" ]; then
		cd "$snapshot"
		exec "$@"
	fi
	dir="$state_root/$name"
	mkdir -p "$dir"
	printf '%s\n' "$snapshot" >"$dir/workdir"
	: >"$dir/log"
	program="$1"
	shift
	[ "$program" = /usr/local/bin/dummy-agent ] || {
		echo "unexpected test runtime command: $program" >&2
		exit 1
	}
	[ "$#" -eq 1 ] || {
		echo "test runtime expects one dummy-agent task" >&2
		exit 1
	}
	task="$1"
	case "$task" in
	interactive|finish) ;;
	*)
		echo "unexpected dummy-agent task: $task" >&2
		exit 1
		;;
	esac
	tmux -f /qemu/tmux.conf new-session -d -s "$name" -c "$snapshot" \
		"sleep 0.2; exec '$state_root/images/$image/dummy-agent' '$task'"
	tmux -f /qemu/tmux.conf pipe-pane -o -t "$name" "cat >> '$dir/log'"
	;;
attach)
	exec tmux -f /qemu/tmux.conf attach-session -t "$1"
	;;
logs)
	while [ "$#" -gt 0 ] && [ "$1" = "-f" ]; do
		shift
	done
	dir="$state_root/$1"
	[ -f "$dir/log" ] || exit 1
	exec tail -f "$dir/log"
	;;
exec)
	[ "$#" -ge 3 ] || exit 1
	name="$2"
	shift 2
	dir="$state_root/$name"
	[ -f "$dir/workdir" ] || exit 1
	cd "$(cat "$dir/workdir")"
	exec "$@"
	;;
rm)
	[ "${1:-}" = "-f" ] && shift
	name="${1#agentbox-}"
	tmux -f /qemu/tmux.conf kill-session -t "$name" 2>/dev/null || true
	rm -rf -- "$state_root/$1"
	;;
*)
	echo "unsupported test runtime operation: $subcommand" >&2
	exit 1
	;;
esac
