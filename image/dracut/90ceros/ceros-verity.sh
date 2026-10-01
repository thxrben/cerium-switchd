#!/bin/sh
# Opens the slot with dm-verity (initqueue job, repeated until it worked).
. /lib/dracut-lib.sh
[ -b /dev/mapper/root ] && exit 0
part=$(getarg ceros.part=)
dev=/dev/disk/by-partuuid/$part
[ -b "$dev" ] || exit 0
info "cerOS: slot $(getarg ceros.slot=) on $(readlink -f "$dev")"
veritysetup open "$dev" root "$dev" "$(getarg ceros.roothash=)" \
    --hash-offset="$(getarg ceros.hashoffset=)" --panic-on-corruption ||
    die "cerOS: the slot does not match its verity root hash"
