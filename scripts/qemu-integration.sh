#!/usr/bin/env bash
set -Eeuo pipefail

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ALPINE_VERSION="${AGENTBOX_QEMU_ALPINE_VERSION:-v3.22}"
readonly CACHE_ROOT="${XDG_CACHE_HOME:-"$HOME/.cache"}"
readonly CACHE_DIR="${AGENTBOX_QEMU_CACHE_DIR:-"$CACHE_ROOT/agentbox/qemu/alpine-$ALPINE_VERSION-x86_64"}"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agentbox-qemu.XXXXXXXX")"
readonly QEMU="${QEMU:-qemu-system-x86_64}"
readonly QEMU_IMG="${QEMU_IMG:-qemu-img}"
source "$ROOT/scripts/qemu/lib.sh"

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
mkdir -p "$WORK_DIR/overlay/bin" "$WORK_DIR/overlay/sbin" "$WORK_DIR/overlay/etc/apk" \
	"$WORK_DIR/overlay/qemu/examples/qemu-agent" "$WORK_DIR/overlay/qemu/examples/qemu-project"
cp -a "$ROOT/scripts/qemu/." "$WORK_DIR/overlay/qemu/"
cp -a "$ROOT/examples/qemu-agent/." "$WORK_DIR/overlay/qemu/examples/qemu-agent/"
cp -a "$ROOT/examples/qemu-project/." "$WORK_DIR/overlay/qemu/examples/qemu-project/"
printf 'https://dl-cdn.alpinelinux.org/alpine/%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/%s/community\n' \
	"$ALPINE_VERSION" "$ALPINE_VERSION" >"$WORK_DIR/overlay/etc/apk/repositories"
printf 'root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/bin/sh\n' \
	>"$WORK_DIR/overlay/etc/passwd"
printf 'root:x:0:\nnobody:x:65534:\n' >"$WORK_DIR/overlay/etc/group"
busybox_applets=(cat chmod chown cp grep mkdir mount mv poweroff rm sed sleep su sync)
for applet in "${busybox_applets[@]}"; do
	ln -s /bin/busybox "$WORK_DIR/overlay/bin/$applet"
	ln -s /bin/busybox "$WORK_DIR/overlay/sbin/$applet"
done
network_applets=(ip udhcpc)
for applet in "${network_applets[@]}"; do
	ln -s /bin/busybox "$WORK_DIR/overlay/sbin/$applet"
done
chmod -R a+rX "$WORK_DIR/overlay/qemu"
ln -s /qemu/guest-init.sh "$WORK_DIR/overlay/agentbox-init"

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
