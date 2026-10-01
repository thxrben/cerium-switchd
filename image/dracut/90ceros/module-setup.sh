#!/bin/bash
# dracut module: the cerOS root (docs/os-image.md §2, §3): dm-verity on the
# slot, the configuration and data partitions, the volatile /etc.

check() { return 0; }

depends() { echo rootfs-block dm; }

installkernel() {
    hostonly='' instmods dm-verity squashfs overlay ext4 vfat nls_cp437 nls_iso8859_1 \
        virtio_blk virtio_scsi virtio_pci nvme ahci sd_mod sr_mod usb_storage uas xhci_pci ehci_pci mmc_block sdhci_pci
}

install() {
    inst_multiple veritysetup e2fsck mke2fs mkfs.ext4 tar readlink cp mount umount mkdir cat
    inst_hook cmdline 90 "$moddir/ceros-cmdline.sh"
    inst_hook pre-pivot 50 "$moddir/ceros-mount.sh"
    inst_script "$moddir/ceros-verity.sh" /sbin/ceros-verity
}
