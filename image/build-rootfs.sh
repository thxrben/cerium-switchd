#!/bin/bash
# Builds the cerOS slot image (docs/os-image.md §5): runs inside the
# ceros-build container (image/Dockerfile), privileged.
#
#   build-rootfs.sh <out-dir> <version> <switchd-binary> <keys-dir>
#
# Writes <out-dir>/rootfs.img (squashfs + verity hash tree),
# <out-dir>/verity.json (root hash, hash offset, data size),
# <out-dir>/BOOTX64.EFI and <out-dir>/grub.cfg.
set -euo pipefail

out=$1 version=$2 switchd=$3 keys=$4
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d /var/tmp/ceros-root.XXXXXX)
trap 'rm -rf "$root"' EXIT
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(date +%s)}

pkgs=(
  # kernel, boot, file systems
  linux-image-amd64 systemd systemd-sysv udev dracut cryptsetup-bin e2fsprogs dosfstools kmod
  grub-efi-amd64-bin grub-common gdisk
  firmware-misc-nonfree firmware-realtek firmware-bnx2x firmware-intel-misc firmware-netronome
  # what switchd uses
  iproute2 nftables ethtool openssh-server passwd login util-linux bash procps ca-certificates
  # testing and fault finding
  tcpdump iperf3 mtr-tiny iputils-ping iputils-tracepath traceroute netcat-openbsd socat curl dnsutils
  strace lsof htop pciutils usbutils i2c-tools dmidecode less vim-tiny
)

mmdebstrap --variant=minbase --mode=root --components="main non-free-firmware" \
  --include="$(IFS=,; echo "${pkgs[*]}")" \
  --dpkgopt='path-exclude=/usr/share/man/*' --dpkgopt='path-exclude=/usr/share/doc/*' \
  --dpkgopt='path-include=/usr/share/doc/*/copyright' --dpkgopt='path-exclude=/usr/share/locale/*' \
  --dpkgopt='path-exclude=/usr/share/info/*' \
  --essential-hook='echo "# cerOS: no network configuration by the OS" > "$1/etc/resolv.conf"' \
  trixie "$root" http://deb.debian.org/debian

# Our files.
cp -a "$here/overlay/." "$root/"
mkdir -p "$root/config"
install -D -m 0755 "$switchd" "$root/usr/local/sbin/switchd"
ln -sf /usr/local/sbin/switchd "$root/usr/local/bin/swcli"
ln -sf /usr/local/sbin/switchd "$root/usr/local/bin/cli"
echo /usr/local/bin/swcli >> "$root/etc/shells"
install -D -m 0644 "$here/../packaging/switchd.service" "$root/etc/systemd/system/switchd.service"
install -D -m 0644 "$here/../packaging/switchd-update.service" "$root/etc/systemd/system/switchd-update.service"
mkdir -p "$root/usr/share/ceros/keys"
cp "$keys"/*.pub "$root/usr/share/ceros/keys/"
cp -a "$here/dracut/90ceros" "$root/usr/lib/dracut/modules.d/"
cat > "$root/etc/ceros-release" <<EOF
VERSION=$version
PLATFORM=x86_64-efi
COMPATIBLE=ceros-x86_64
LAYOUT=1
EOF

chroot "$root" /bin/sh -e <<'EOF'
systemctl enable switchd.service switchd-update.service ceros-hostkeys.service
systemctl mask systemd-networkd.service systemd-networkd.socket systemd-networkd-wait-online.service \
  ssh.service ssh.socket apt-daily.timer apt-daily-upgrade.timer e2scrub_all.timer e2scrub_reap.service \
  systemd-firstboot.service systemd-timesyncd.service systemd-machine-id-commit.service 2>/dev/null || true
systemctl disable ssh.service 2>/dev/null || true
# The stable names: GRUB and the installer use these.
kver=$(ls /lib/modules | sort -V | tail -1)
ln -sf "vmlinuz-$kver" /boot/vmlinuz
dracut --force --no-hostonly --kver "$kver" /boot/initrd.img-$kver
ln -sf "initrd.img-$kver" /boot/initrd.img
# Host keys and the machine id live on /config (docs/os-image.md §3).
rm -f /etc/ssh/ssh_host_* /etc/machine-id /var/lib/dbus/machine-id
: > /etc/machine-id
# Root logs in on the consoles without a password until root-authentication is set.
passwd -d root
# No package management in the image.
rm -rf /var/lib/apt/lists/* /var/cache/apt/* /var/cache/debconf/*-old /var/log/*
EOF
# apt and dpkg go last (the chroot steps above use them).
rm -rf "$root"/usr/bin/apt* "$root"/usr/bin/dpkg* "$root"/usr/lib/apt "$root"/etc/apt "$root"/var/lib/apt
rm -rf "$root"/usr/share/doc "$root"/usr/share/man "$root"/usr/share/locale "$root"/usr/share/info

mkdir -p "$out"
rm -f "$out/rootfs.img"
mksquashfs "$root" "$out/rootfs.img" -comp xz -Xbcj x86 -noappend -no-progress -quiet
# Verity: the hash tree follows the squashfs, which is padded to 4 KiB.
size=$(stat -c %s "$out/rootfs.img")
off=$(( (size + 4095) / 4096 * 4096 ))
truncate -s "$off" "$out/rootfs.img"
salt=$(printf '%s' "ceros-$version" | sha256sum | cut -c1-64)
uuid=$(printf '%s' "ceros-$version" | sha256sum | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12}).*/\1-\2-\3-\4-\5/')
roothash=$(veritysetup format "$out/rootfs.img" "$out/rootfs.img" --hash-offset="$off" \
  --salt="$salt" --uuid="$uuid" | awk '/^Root hash:/ {print $3}')
printf '{"roothash":"%s","hash_offset":%d,"data_size":%d}\n' "$roothash" "$off" "$off" > "$out/verity.json"

# The boot loader (the same for both slots; installed on the ESP).
grub-mkimage -O x86_64-efi -o "$out/BOOTX64.EFI" -p /EFI/ceros \
  part_gpt fat squash4 normal linux echo test regexp probe loadenv configfile all_video \
  gfxterm serial terminal sleep efi_gop efi_uga search search_fs_uuid halt reboot
cp "$here/grub/grub.cfg" "$out/grub.cfg"
echo "rootfs.img: $(stat -c %s "$out/rootfs.img") bytes, root hash $roothash"
