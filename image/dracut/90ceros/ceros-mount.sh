#!/bin/sh
# Mounts the configuration and data partitions and prepares /etc
# (docs/os-image.md §3). Runs before the switch to the root file system.
. /lib/dracut-lib.sh
getarg ceros.slot= >/dev/null || return 0

slotdev=$(readlink -f "/dev/disk/by-partuuid/$(getarg ceros.part=)")
name=${slotdev##*/}
disk=$(readlink -f "/sys/class/block/$name/..")
disk=${disk##*/}
part() {
    for p in /sys/class/block/"$disk"/"$disk"*; do
        [ "$(cat "$p/partition" 2>/dev/null)" = "$1" ] && echo "/dev/${p##*/}" && return
    done
}
cfgdev=$(part 4)
datadev=$(part 5)

check_fs() {
    e2fsck -p "$1" >/dev/null 2>&1
    [ $? -lt 4 ] || e2fsck -y "$1" >/dev/null 2>&1
}

# Data: logs and state; formatted again when it cannot be mounted.
mkdir -p /run/ceros/data
check_fs "$datadev"
if ! mount -t ext4 -o nodev,nosuid "$datadev" /run/ceros/data; then
    warn "cerOS: the data partition $datadev cannot be mounted: formatting it again"
    mkfs.ext4 -F -q -L ceros-data "$datadev" && mount -t ext4 -o nodev,nosuid "$datadev" /run/ceros/data
    mkdir -p /run/ceros/data/lib/ceros && echo "data partition formatted at boot" > /run/ceros/data/lib/ceros/data-formatted
fi
# What the image has under /var (directories, defaults) is kept on the data partition.
cp -an "$NEWROOT/var/." /run/ceros/data/ 2>/dev/null
mkdir -p /run/ceros/data/home /run/ceros/data/root /run/ceros/data/log/journal /run/ceros/data/lib/ceros
[ -e /run/ceros/data/root/.profile ] || cp -an "$NEWROOT/root/." /run/ceros/data/root/ 2>/dev/null
chmod 0700 /run/ceros/data/root
mount --move /run/ceros/data "$NEWROOT/var"

# Configuration: repaired, or formatted and filled from the copy on the data partition.
check_fs "$cfgdev"
if ! mount -t ext4 -o nodev,nosuid,noexec "$cfgdev" "$NEWROOT/config"; then
    warn "cerOS: the configuration partition $cfgdev cannot be mounted: formatting it again"
    mkfs.ext4 -F -q -L ceros-config "$cfgdev" && mount -t ext4 -o nodev,nosuid,noexec "$cfgdev" "$NEWROOT/config"
    if [ -f "$NEWROOT/var/lib/ceros/config-copy.tar" ]; then
        tar -xf "$NEWROOT/var/lib/ceros/config-copy.tar" -C "$NEWROOT/config" &&
            warn "cerOS: the configuration was restored from the copy on the data partition"
    fi
fi
c=$NEWROOT/config
[ -f "$c/layout" ] || echo 1 > "$c/layout"
mkdir -p "$c/switchd" "$c/ssh" "$c/update" "$c/backup"
chmod 0700 "$c/switchd" "$c/ssh" "$c/update" "$c/backup"
if [ ! -s "$c/machine-id" ]; then
    tr -d '-' < /proc/sys/kernel/random/uuid > "$c/machine-id.new" && mv "$c/machine-id.new" "$c/machine-id"
fi

# /etc: the image's, with changes in memory only.
mkdir -p /run/ceros/etc/upper /run/ceros/etc/work
mount -t overlay overlay -o "lowerdir=$NEWROOT/etc,upperdir=/run/ceros/etc/upper,workdir=/run/ceros/etc/work" "$NEWROOT/etc"
mkdir -p "$NEWROOT/var/lib/switchd"
mount --bind "$c/switchd" "$NEWROOT/var/lib/switchd"
mount --bind "$c/machine-id" "$NEWROOT/etc/machine-id"
mount --bind "$NEWROOT/var/home" "$NEWROOT/home"
mount --bind "$NEWROOT/var/root" "$NEWROOT/root"
