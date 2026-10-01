# Build environment for the cerOS image (docs/os-image.md §6).
FROM debian:trixie
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      mmdebstrap ca-certificates squashfs-tools cryptsetup-bin grub-efi-amd64-bin grub-common \
      dosfstools mtools gdisk e2fsprogs fdisk xz-utils zstd \
    && rm -rf /var/lib/apt/lists/*
