#!/bin/sh
set -eu

echo "running daemon protocol integration suite"
export AGENTBOX_QEMU_GUEST=1
export AGENTBOX_QEMU_SOURCE=/mnt/test/source
export AGENTBOX_QEMU_DESTINATION=/mnt/test/allowed/snapshot
export AGENTBOX_QEMU_PRIVATE_SOURCE=/mnt/test/private-source
export AGENTBOX_QEMU_DENIED_DESTINATION=/mnt/test/allowed/denied-source
export AGENTBOX_QEMU_PRIVATE_DESTINATION=/mnt/test/private-dest/denied-destination
export AGENTBOX_QEMU_CREATE_PATH=/mnt/test/allowed/created-subvolume
export AGENTBOX_QEMU_PRIVATE_CREATE_PATH=/mnt/test/private-dest/denied-subvolume
export AGENTBOX_QEMU_PROJECT_PARENT=/mnt/test/allowed

if ! su nobody -s /bin/bash -c 'exec /agentbox-integration.test -test.run "^TestQEMU" -test.v'; then
	cat /tmp/agentbox-daemon.log
	exit 1
fi

[ ! -e /mnt/test/allowed/denied-source ] || {
	echo "denied snapshot source created a destination" >&2
	exit 1
}
[ ! -e /mnt/test/private-dest/denied-destination ] || {
	echo "denied snapshot destination was created" >&2
	exit 1
}
[ ! -e /mnt/test/private-dest/denied-subvolume ] || {
	echo "denied subvolume creation left a destination" >&2
	exit 1
}
