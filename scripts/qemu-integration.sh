#!/usr/bin/env bash
set -Eeuo pipefail

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ALPINE_VERSION="${AGENTBOX_QEMU_ALPINE_VERSION:-v3.22}"
readonly CACHE_ROOT="${XDG_CACHE_HOME:-"$HOME/.cache"}"
readonly CACHE_DIR="${AGENTBOX_QEMU_CACHE_DIR:-"$CACHE_ROOT/agentbox/qemu/alpine-$ALPINE_VERSION-x86_64"}"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agentbox-qemu.XXXXXXXX")"
readonly QEMU="${QEMU:-qemu-system-x86_64}"
readonly QEMU_IMG="${QEMU_IMG:-qemu-img}"

cleanup() {
	rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

required_tools=("$QEMU" "$QEMU_IMG" awk cpio curl go tar timeout)
for tool in "${required_tools[@]}"; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'required tool not found: %s\n' "$tool" >&2
		exit 1
	fi
done

mkdir -p -- "$CACHE_DIR"

fetch_cached() {
	local name="$1" url="$2" target="$CACHE_DIR/$1" temporary
	if [[ ! -s "$target" ]]; then
		printf 'Downloading Alpine %s %s into %s\n' "$ALPINE_VERSION" "$name" "$CACHE_DIR" >&2
		temporary="$(mktemp "$CACHE_DIR/.${name}.XXXXXX")"
		if ! curl --fail --location --retry 3 --output "$temporary" "$url"; then
			rm -f -- "$temporary"
			return 1
		fi
		mv -f -- "$temporary" "$target"
	fi
}

readonly BASE_URL="https://dl-cdn.alpinelinux.org/alpine/$ALPINE_VERSION/releases/x86_64/netboot"
fetch_cached initramfs-virt "$BASE_URL/initramfs-virt"

readonly MAIN_REPO="https://dl-cdn.alpinelinux.org/alpine/$ALPINE_VERSION/main/x86_64"
KERNEL_VERSION="$(
	curl --fail --location --silent --show-error "$MAIN_REPO/APKINDEX.tar.gz" |
		tar -xzO APKINDEX |
		awk 'BEGIN { RS = "" } !found {
			for (i = 1; i <= NF; i++) {
				if ($i == "P:linux-virt") {
					for (j = i + 1; j <= NF; j++) {
						if ($j ~ /^V:/) {
							sub(/^V:/, "", $j)
							print $j
							found = 1
							break
						}
					}
				}
			}
		}'
)"
if [[ -z "$KERNEL_VERSION" ]]; then
	echo "could not determine the Alpine linux-virt package version" >&2
	exit 1
fi
readonly KERNEL_APK="linux-virt-$KERNEL_VERSION.apk"
fetch_cached "$KERNEL_APK" "$MAIN_REPO/$KERNEL_APK"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$WORK_DIR/agentbox" "$ROOT"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$WORK_DIR/agentbox-integration.test" "$ROOT"

mkdir -p "$WORK_DIR/overlay" "$WORK_DIR/kernel"
tar --warning=no-unknown-keyword -xzf "$CACHE_DIR/$KERNEL_APK" \
	-C "$WORK_DIR/kernel" boot/vmlinuz-virt lib/modules
install -m 0644 "$WORK_DIR/kernel/boot/vmlinuz-virt" "$WORK_DIR/vmlinuz-virt"
mkdir -p "$WORK_DIR/overlay/lib"
cp -a "$WORK_DIR/kernel/lib/modules" "$WORK_DIR/overlay/lib/"
install -m 0755 "$WORK_DIR/agentbox" "$WORK_DIR/overlay/agentbox"
install -m 0755 "$WORK_DIR/agentbox-integration.test" "$WORK_DIR/overlay/agentbox-integration.test"
mkdir -p "$WORK_DIR/overlay/bin" "$WORK_DIR/overlay/sbin" "$WORK_DIR/overlay/etc/apk"
printf 'https://dl-cdn.alpinelinux.org/alpine/%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/%s/community\n' \
	"$ALPINE_VERSION" "$ALPINE_VERSION" >"$WORK_DIR/overlay/etc/apk/repositories"
printf 'root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/bin/sh\n' \
	>"$WORK_DIR/overlay/etc/passwd"
printf 'root:x:0:\nnobody:x:65534:\n' >"$WORK_DIR/overlay/etc/group"
busybox_applets=(cat chmod chown cp grep mkdir mount mv poweroff sed sleep su sync)
for applet in "${busybox_applets[@]}"; do
	ln -s /bin/busybox "$WORK_DIR/overlay/bin/$applet"
	ln -s /bin/busybox "$WORK_DIR/overlay/sbin/$applet"
done
network_applets=(ip udhcpc)
for applet in "${network_applets[@]}"; do
	ln -s /bin/busybox "$WORK_DIR/overlay/sbin/$applet"
done
cat >"$WORK_DIR/overlay/agentbox-init" <<'GUEST_INIT'
#!/bin/sh
set -eu

export PATH=/sbin:/bin:/usr/sbin:/usr/bin
export HOME=/root

fail() {
	echo "AGENTBOX_QEMU_RESULT=FAIL: $*" >/dev/console
	/sbin/poweroff -f
	exit 1
}

exec >/dev/console 2>&1
echo "agentbox QEMU integration guest starting"

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /run /tmp /mnt/test
mount -t devpts devpts /dev/pts
chmod 1777 /tmp

modprobe virtio_blk 2>/dev/null || true
ip link set lo up
modprobe virtio_net 2>/dev/null || true
ip link set eth0 up || fail "QEMU network interface eth0 is unavailable"
udhcpc -n -q -t 10 -T 2 -i eth0 || fail "could not configure network for Alpine packages"
mkdir -p /lib/apk/db
apk --initdb add --no-cache bash btrfs-progs || fail "could not install Bash and btrfs tools"
modprobe btrfs || fail "could not load btrfs kernel module"

disk=/dev/vda
tries=0
while [ ! -b "$disk" ] && [ "$tries" -lt 20 ]; do
	sleep 1
	tries=$((tries + 1))
done
[ -b "$disk" ] || fail "QEMU test disk not found"
mkfs.btrfs -f "$disk" || fail "could not create btrfs test filesystem"
mount -t btrfs "$disk" /mnt/test || fail "could not mount btrfs test filesystem"

btrfs subvolume create /mnt/test/source || fail "could not create source subvolume"
mkdir -p /mnt/test/allowed /mnt/test/private-dest
mkdir -p /mnt/test/private-source
printf 'qemu daemon integration\n' >/mnt/test/source/payload.txt
chown -R nobody:nobody /mnt/test/source /mnt/test/allowed
chmod 0700 /mnt/test/private-source /mnt/test/private-dest

/agentbox daemon >/tmp/agentbox-daemon.log 2>&1 &
daemon_pid=$!
tries=0
while [ ! -S /run/agentbox/agentbox.sock ] && [ "$tries" -lt 20 ]; do
	kill -0 "$daemon_pid" 2>/dev/null || fail "daemon exited: $(cat /tmp/agentbox-daemon.log)"
	sleep 1
	tries=$((tries + 1))
done
[ -S /run/agentbox/agentbox.sock ] || fail "daemon socket was not created"

export AGENTBOX_QEMU_GUEST=1
export AGENTBOX_QEMU_SOURCE=/mnt/test/source
export AGENTBOX_QEMU_DESTINATION=/mnt/test/allowed/snapshot
export AGENTBOX_QEMU_PRIVATE_SOURCE=/mnt/test/private-source
export AGENTBOX_QEMU_DENIED_DESTINATION=/mnt/test/allowed/denied-source
export AGENTBOX_QEMU_PRIVATE_DESTINATION=/mnt/test/private-dest/denied-destination
set +e
su nobody -s /bin/bash -c 'exec /agentbox-integration.test -test.run "^TestQEMU" -test.v'
test_status=$?
set -e
if [ "$test_status" -ne 0 ]; then
	cat /tmp/agentbox-daemon.log
	fail "Go integration tests failed (status $test_status)"
fi
[ ! -e /mnt/test/allowed/denied-source ] || fail "denied snapshot source created a destination"
[ ! -e /mnt/test/private-dest/denied-destination ] || fail "denied snapshot destination was created"

kill -TERM "$daemon_pid"
wait "$daemon_pid" || fail "daemon did not shut down cleanly"
echo "AGENTBOX_QEMU_RESULT=PASS"
sync
/sbin/poweroff -f
GUEST_INIT
chmod 0755 "$WORK_DIR/overlay/agentbox-init"

(cd "$WORK_DIR/overlay" && find . -print | cpio --quiet -o -H newc) >"$WORK_DIR/overlay.cpio"
cat "$CACHE_DIR/initramfs-virt" "$WORK_DIR/overlay.cpio" >"$WORK_DIR/initramfs"
"$QEMU_IMG" create -q -f qcow2 "$WORK_DIR/test-disk.qcow2" 512M

qemu_args=(
	-machine accel=tcg \
	-m 1024 \
	-smp 2 \
	-kernel "$WORK_DIR/vmlinuz-virt" \
	-initrd "$WORK_DIR/initramfs" \
	-append "console=ttyS0,115200 rdinit=/agentbox-init panic=1" \
	-drive "file=$WORK_DIR/test-disk.qcow2,if=virtio,format=qcow2" \
	-netdev user,id=net0 \
	-device virtio-net-pci,netdev=net0 \
	-display none \
	-serial "file:$WORK_DIR/serial.log" \
	-monitor none \
	-no-reboot
)
if timeout "${AGENTBOX_QEMU_TIMEOUT:-180}" "$QEMU" "${qemu_args[@]}"; then
	qemu_status=0
else
	qemu_status=$?
fi

if [[ -f "$WORK_DIR/serial.log" ]]; then
	cat -- "$WORK_DIR/serial.log"
fi
if (( qemu_status != 0 )); then
	printf 'QEMU exited with status %d\n' "$qemu_status" >&2
	exit "$qemu_status"
fi
if ! awk 'index($0, "AGENTBOX_QEMU_RESULT=PASS") == 1 { found = 1 } END { exit !found }' "$WORK_DIR/serial.log"; then
	echo "QEMU guest did not report a passing integration run" >&2
	exit 1
fi
echo "QEMU daemon integration tests passed (Alpine image cached in $CACHE_DIR)"
