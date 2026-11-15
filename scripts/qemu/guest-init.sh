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
mkdir -p /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || true

modprobe virtio_blk 2>/dev/null || true
ip link set lo up
modprobe virtio_net 2>/dev/null || true
ip link set eth0 up || fail "QEMU network interface eth0 is unavailable"
udhcpc -n -q -t 10 -T 2 -i eth0 || fail "could not configure network for Alpine packages"
mkdir -p /lib/apk/db
apk --initdb add --no-cache bash btrfs-progs coreutils expect git tmux || fail "could not install QEMU integration tools"
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
mkdir -p /mnt/test/allowed /mnt/test/private-dest /mnt/test/private-source
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

/qemu/daemon-suite.sh || fail "daemon integration suite failed"
/qemu/workflow-suite.sh || fail "CLI workflow integration suite failed"

kill -TERM "$daemon_pid"
wait "$daemon_pid" || fail "daemon did not shut down cleanly"
echo "AGENTBOX_QEMU_RESULT=PASS"
sync
/sbin/poweroff -f
