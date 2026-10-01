#!/bin/bash
# Builds a complete cerOS disk (docs/os-image.md §1, §6): GPT with ESP,
# slots A and B (both hold the image), empty config and data partitions.
# Runs inside the ceros-build container, privileged (loop devices).
#
#   make-disk.sh <build-dir> <version> <out.img> [<size-GiB>]
set -euo pipefail

b=$1 version=$2 out=$3 size=${4:-8}
roothash=$(sed -E 's/.*"roothash":"([0-9a-f]+)".*/\1/' "$b/verity.json")
off=$(sed -E 's/.*"hash_offset":([0-9]+).*/\1/' "$b/verity.json")

rm -f "$out"
truncate -s "${size}G" "$out"
sgdisk -Z "$out" >/dev/null
sgdisk -n1:1M:+256M -t1:ef00 -c1:ceros-esp \
       -n2:0:+2G -t2:8300 -c2:ceros-a \
       -n3:0:+2G -t3:8300 -c3:ceros-b \
       -n4:0:+512M -t4:8300 -c4:ceros-config \
       -n5:0:0 -t5:8300 -c5:ceros-data "$out" >/dev/null

# Partition offsets from the table (no loop devices: works in any container).
start() { sgdisk -i "$1" "$out" | awk '/^First sector:/ {print $3}'; }
sectors() { sgdisk -i "$1" "$out" | awk '/^Partition size:/ {print $3}'; }
put() { dd if="$2" of="$out" bs=512 seek="$(start "$1")" conv=notrunc,sparse status=none; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# ESP: FAT32 built in a file, filled with mtools.
truncate -s $(( $(sectors 1) * 512 )) "$tmp/esp"
mkfs.vfat -F 32 -n CEROS-ESP "$tmp/esp" >/dev/null
grub-editenv "$tmp/grubenv" create
grub-editenv "$tmp/grubenv" set ORDER="A B" A_OK=1 B_OK=1 A_TRY=0 B_TRY=0 \
  A_ROOTHASH="$roothash" B_ROOTHASH="$roothash" A_HASHOFFSET="$off" B_HASHOFFSET="$off" \
  A_VERSION="$version" B_VERSION="$version"
export MTOOLS_SKIP_CHECK=1
mmd -i "$tmp/esp" ::/EFI ::/EFI/BOOT ::/EFI/ceros
mcopy -i "$tmp/esp" "$b/BOOTX64.EFI" ::/EFI/BOOT/BOOTX64.EFI
mcopy -i "$tmp/esp" "$b/grub.cfg" ::/EFI/ceros/grub.cfg
mcopy -i "$tmp/esp" "$tmp/grubenv" ::/EFI/ceros/grubenv
put 1 "$tmp/esp"
put 2 "$b/rootfs.img"
put 3 "$b/rootfs.img"
for i in 4 5; do
  label=ceros-config; [ $i = 5 ] && label=ceros-data
  truncate -s $(( $(sectors $i) * 512 )) "$tmp/fs"
  mkfs.ext4 -F -q -L "$label" "$tmp/fs"
  put $i "$tmp/fs"
  rm -f "$tmp/fs"
done
echo "$out: ${size} GiB, both slots $version"
