#!/bin/sh
# The root is the slot through dm-verity; it appears as /dev/mapper/root.
if getarg ceros.slot= >/dev/null; then
    root=block:/dev/mapper/root
    rootok=1
    /sbin/initqueue --settled --unique --name ceros-verity /sbin/ceros-verity
fi
