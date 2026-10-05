#!/bin/bash
# End-to-end test of the cerOS image in QEMU (docs/os-image.md §4):
#   test/image/test-update.sh <v1> <v2> [<first step>]
# needs dist/ceros-<v1>-amd64.img and dist/ceros-<v2>-amd64.bundle (image/build.sh).
#
# 1. boot v1; the boot is confirmed (TRY=0)
# 2. update to v2 through the update daemon: written to slot B, reboot, v2 healthy, slot confirmed
# 3. rollback command: back to v1 (slot A)
# 4. a v2 whose switchd never gets healthy (switchd masked before the reboot): the daemon rolls back
# 5. a slot that fails verity (corrupted after it was written): the boot loader returns to the old slot
# 6. power loss while slot B is written: v1 boots normally
# 7. a bundle with a changed byte and one signed by an unknown key are rejected
set -euo pipefail
cd "$(dirname "$0")/../.."
PWD_ROOT=$PWD
v1=$1 v2=$2 from=${3:-1}
wd=build/test-image
vm=test/image/vm.py
mkdir -p "$wd"
cp --sparse=always "dist/ceros-$v1-amd64.img" "$wd/disk.img"
rm -f "$wd/vars.fd"
bundle=dist/ceros-$v2-amd64.bundle

say() { echo "=== $*"; }
# step <n>: true when step n runs (steps before the first one are skipped).
step() { [ "${1%b}" -ge "$from" ]; }
run() { $vm run "$wd" "$1" "${2:-120}"; }
boot() { # boot; wait for the CLI
  $vm mark "$wd"
  $vm wait "$wd" 'root@[^ ]*> ' "${1:-240}" >/dev/null
}
fail() { echo "FAIL: $*"; $vm stop "$wd"; exit 1; }
# shellcheck disable=SC2329
cleanup() { $vm stop "$wd"; }
trap cleanup EXIT

running() { run 'sed -n "s/^VERSION=//p" /etc/ceros-release; sed -n "s/.*ceros.slot=\([AB]\).*/\1/p" /proc/cmdline'; }
grubenv() { run 'mkdir -p /run/e && mount -o ro /dev/disk/by-partlabel/ceros-esp /run/e && grep -a "^[AB]_\(OK\|TRY\|VERSION\)\|^ORDER" /run/e/EFI/ceros/grubenv | tr "\n" " "; umount /run/e'; }
# Wait until the update daemon confirmed the running slot.
confirmed() {
  local slot=$1
  for _ in $(seq 60); do
    if grubenv | grep -q "${slot}_TRY=0"; then return 0; fi
    sleep 3
  done
  fail "slot $slot not confirmed: $(grubenv)"
}
# Copy the bundle into the VM over the serial console is too slow; it is put
# on the data partition of the disk image while the VM is off.
put_bundle() {
  local f=$1 name=$2
  docker run --rm --privileged -v "$PWD/$wd":/w -v "$PWD/$(dirname "$f")":/b:ro ceros-build bash -c "
    set -e
    off=\$(sgdisk -i 5 /w/disk.img | awk '/^First sector:/ {print \$3}')
    l=\$(losetup --show -f -o \$((off*512)) /w/disk.img)
    mkdir -p /m && mount \$l /m && mkdir -p /m/lib/ceros/software && cp /b/$(basename "$f") /m/lib/ceros/software/$name
    umount /m; losetup -d \$l"
}
install() { # install the bundle through the update daemon, as switchd does
  run "printf '%s\n' '{\"op\":\"install\",\"bundle\":\"/var/lib/ceros/software/$1\",\"standalone\":true}' | socat -t900 - UNIX-CONNECT:/run/switchd-update/sock" 900
}

put_bundle "$bundle" good.bundle
$vm start "$wd/disk.img" "$wd"
boot

if step 1; then
say "1. boot $v1"
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "not $v1 on slot A: $(running)"
confirmed A
run 'systemctl is-active switchd switchd-update; systemctl --failed --no-legend; ip -br addr | grep -v "^lo\|swstack\|swbr0" || true'
fi

if step 2; then
say "2. update to $v2"
out=$(install good.bundle); echo "$out"
echo "$out" | grep -q "\"version\":\"$v2\"" || fail "install: $out"
boot 300
[ "$(running | tr '\n' ' ')" = "$v2 B " ] || fail "not $v2 on slot B: $(running)"
confirmed B
grubenv | grep -q 'ORDER=B A' || fail "order: $(grubenv)"
run 'cat /config/update/state.json; ls /config/backup'
fi

if step 3; then
say "3. rollback to $v1"
out=$(run "printf '%s\n' '{\"op\":\"rollback\"}' | socat -t60 - UNIX-CONNECT:/run/switchd-update/sock"); echo "$out"
boot 300
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "rollback: not $v1 on slot A: $(running)"
confirmed A
fi

if step 4; then
say "4. $v2 whose switchd never becomes healthy (switchd masked on slot B only)"
# grub.cfg passes the extra arguments only when it boots slot B; the health timeout is shortened to 60 s.
run 'mkdir -p /run/e && mount /dev/disk/by-partlabel/ceros-esp /run/e && cp /run/e/EFI/ceros/grub.cfg /run/e/EFI/ceros/grub.cfg.orig && sed -i "s/^    set part=gpt3$/    set part=gpt3\n    set extra=\"systemd.mask=switchd.service ceros.healthtimeout=60\"/; " /run/e/EFI/ceros/grub.cfg && grep -n "extra" /run/e/EFI/ceros/grub.cfg; umount /run/e'
out=$(install good.bundle); echo "$out"
$vm mark "$wd"
$vm wait "$wd" "cerOS: slot B" 300
$vm wait "$wd" "cerOS: slot A" 400
boot 300
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "no rollback: $(running)"
run 'mkdir -p /run/e && mount /dev/disk/by-partlabel/ceros-esp /run/e && mv /run/e/EFI/ceros/grub.cfg.orig /run/e/EFI/ceros/grub.cfg && umount /run/e'
st=$(run 'cat /config/update/state.json'); echo "$st"
echo "$st" | grep -q 'rolled back: '"$v2"' switchd was not healthy' || fail "no rollback recorded"
grubenv | grep -q 'B_OK=0' || fail "slot B not marked failed: $(grubenv)"

# corrupt <slot partition> <byte offset in the slot>
corrupt() {
  docker run --rm --privileged -v "$PWD/$wd":/w ceros-build bash -c "
    off=\$(sgdisk -i $1 /w/disk.img | awk '/^First sector:/ {print \$3}')
    printf 'XXXXXXXX' | dd of=/w/disk.img bs=1 seek=\$((off*512 + $2)) conv=notrunc status=none"
}
hashoff=$(sed -E 's/.*"hash_offset":([0-9]+).*/\1/' "build/image-$v2/out/verity.json")
fi

if step 5; then
say "5. corrupted slot: dm-verity refuses it, the boot loader returns"
out=$(install good.bundle); echo "$out"
$vm stop "$wd"; sleep 1   # power cut instead of the daemon's reboot: slot B is first in ORDER
# The top level of the hash tree (the block after the verity superblock): every read fails verification.
corrupt 3 $((hashoff + 4096))
$vm start "$wd/disk.img" "$wd"
$vm mark "$wd"
$vm wait "$wd" "cerOS: slot B" 120
$vm wait "$wd" "cerOS: slot A" 400
boot 300
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "not back on A: $(running)"
st=$(run 'cat /config/update/state.json'); echo "$st"
echo "$st" | grep -q 'rolled back' || fail "failure not recorded"
grubenv | grep -q 'B_OK=0' || fail "slot B not marked failed: $(grubenv)"
fi

if step 5b; then
say "5b. slot whose kernel GRUB cannot load: GRUB falls back to the other slot"
out=$(install good.bundle); echo "$out"
$vm stop "$wd"; sleep 1
corrupt 3 0   # the squashfs superblock
$vm start "$wd/disk.img" "$wd"
$vm mark "$wd"
$vm wait "$wd" "cerOS: slot A" 400
boot 300
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "not on A: $(running)"
st=$(run 'cat /config/update/state.json'); echo "$st"
echo "$st" | grep -q 'rolled back' || fail "failure not recorded"
fi

if step 6; then
say "6. power loss while slot B is written"
$vm stop "$wd"; sleep 1
VM_WRITE_BPS=10000000 $vm start "$wd/disk.img" "$wd"   # 10 MB/s: the slot write takes ~25 s
boot
# The write takes ~25 s at 10 MB/s; the power is cut 10 s into it.
run "(printf '%s\n' '{\"op\":\"install\",\"bundle\":\"/var/lib/ceros/software/good.bundle\"}' | socat -t900 - UNIX-CONNECT:/run/switchd-update/sock >/dev/null &); sleep 10; journalctl -u switchd-update -n 2 --no-pager -o cat | cut -c1-100"
$vm stop "$wd"; sleep 1
$vm start "$wd/disk.img" "$wd"
$vm mark "$wd"
$vm wait "$wd" "cerOS: slot A" 120
boot 300
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "not on A after the power loss: $(running)"
grubenv | grep -q 'B_OK=0' || fail "slot B bootable after an interrupted write: $(grubenv)"
fi

if step 7; then
say "7. changed and foreign bundles are rejected"
$vm stop "$wd"; sleep 1
python3 - "$bundle" "$wd/bad.bundle" <<'EOF'
import sys
b = bytearray(open(sys.argv[1], 'rb').read())
b[len(b) // 2] ^= 0x01
open(sys.argv[2], 'wb').write(b)
EOF
(cd apps/switchd && go run . keygen "$PWD_ROOT/$wd/other" 2>/dev/null) || true
img=build/image-$v2/out/rootfs.img
vj=build/image-$v2/out/verity.json
(cd apps/switchd && go run . bundle -o "$PWD_ROOT/$wd/foreign.bundle" -image "$PWD_ROOT/$img" -key "$PWD_ROOT/$wd/other.key" -version "$v2" \
  -roothash "$(sed -E 's/.*"roothash":"([0-9a-f]+)".*/\1/' "$PWD_ROOT/$vj")" -hash-offset "$(sed -E 's/.*"hash_offset":([0-9]+).*/\1/' "$PWD_ROOT/$vj")")
put_bundle "$wd/bad.bundle" bad.bundle
put_bundle "$wd/foreign.bundle" foreign.bundle
$vm start "$wd/disk.img" "$wd"; boot
out=$(install bad.bundle); echo "$out"
echo "$out" | grep -q 'SHA-256\|damaged' || fail "changed bundle accepted"
out=$(install foreign.bundle); echo "$out"
echo "$out" | grep -q 'does not trust' || fail "foreign bundle accepted"
[ "$(running | tr '\n' ' ')" = "$v1 A " ] || fail "changed: $(running)"
say "all image tests passed"
fi

