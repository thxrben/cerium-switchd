# cerOS operating system image

cerOS ships as **firmware**: a read-only operating system image built for the switch. The configuration is the only
thing an operator changes. There is no package manager, no DHCP client and no network manager: nothing configures a
network interface except switchd. Updates replace the whole image (kernel, libraries, tools, switchd) and are
**signed**. Every switch keeps the previous image as a **backup it can boot from**, and returns to it by itself when
a new image does not come up.

This document is the design of the image: disk layout, boot, file systems, the update and its automatic rollback.
The user-visible commands are in the configuration reference (3.6 software updates, 3.5 operational commands).

Platform: **x86_64 with UEFI** (servers, mini PCs, Proxmox/QEMU with OVMF). Other platforms (arm64 UEFI) follow the
same design later.

## 1. Disk layout

One disk, GPT, at least 8 GB. The partitions are always the same numbers, so the boot loader can find them without
looking them up:

| # | GPT name | Size | File system | Content |
|---|---|---|---|---|
| 1 | `ceros-esp` | 256 MiB | FAT32 | the boot loader (GRUB), its configuration and the boot state (`grubenv`) |
| 2 | `ceros-a` | 2 GiB | squashfs + dm-verity hash tree | slot A: a complete system (kernel, initramfs, root file system) |
| 3 | `ceros-b` | 2 GiB | squashfs + dm-verity hash tree | slot B: a complete system |
| 4 | `ceros-config` | 512 MiB | ext4 | the configuration and the switch's identity |
| 5 | `ceros-data` | rest | ext4 | logs, state, received software: may be lost |

* **Slots** are written whole, as one image, and only by the update daemon. One slot is *active* (it runs), the other
  is the *backup*: the previous version, complete and bootable. A slot is never modified while it runs.
* The **configuration** partition is small and is written rarely, with every write atomic (write, fsync, rename). It
  holds what makes the switch *this* switch: the committed configurations, the stack keys and member id, the SSH host
  keys, the machine id. Both slots use it.
* The **data** partition holds what can be rebuilt: logs, caches, switchd's run-time state, received software
  bundles, crash reports. A damaged data file system is formatted again at boot (and that is logged), so a broken
  data partition never stops the switch.

## 2. Boot

```
UEFI firmware → GRUB (ceros-esp) → picks slot A or B (§4) → kernel + initramfs of that slot
  → initramfs: dm-verity on the slot, mounts config and data, prepares /etc → systemd → switchd
```

* GRUB reads the kernel (`/boot/vmlinuz`) and initramfs (`/boot/initrd.img`) from **inside the slot's squashfs**, so
  a slot is one self-contained image.
* GRUB passes to the kernel: the slot (`ceros.slot=A`), the partition UUIDs of the slot and the ESP (the disk is
  identified by them, never by a device name such as `/dev/sda`), and the slot's **verity root hash** and hash
  offset (written into the boot state when the slot was installed, from the signed bundle).
* The initramfs opens the slot with **dm-verity** (`panic-on-corruption`): every block of the root file system is
  checked against the hash tree when it is read. A changed or damaged block stops the system (kernel panic, then a
  reboot after 5 seconds), and the next boot takes the other slot (§4). The root file system is mounted read-only.
* Console: serial (`ttyS0`, 115200) and the screen (`tty0`). The GRUB menu shows for 1 second and lists both slots,
  so a person at the console can always start the backup by hand.
* The initramfs never stops at a rescue shell (`rd.shell=0 rd.emergency=reboot`): a failure there reboots, and the
  try counts.
* A **hardware watchdog** (systemd `RuntimeWatchdogSec=30s`, `RebootWatchdogSec=5min`) and `panic=5` +
  `panic_on_oops` make sure a hang always ends in a reboot. The next boot then counts it as a failed try (§4).
  On a machine without a watchdog device (many VMs) switchd turns systemd's watchdog off at start (over D-Bus) and
  logs it: systemd would otherwise retry opening the device about 100 times a second. Proxmox: add a watchdog
  device (`watchdog: model=i6300esb,action=reset`) to keep the protection.

## 3. File systems

| Mount point | Source | Options | Content |
|---|---|---|---|
| `/` | the slot through dm-verity (`/dev/mapper/root`) | squashfs, read-only | the image |
| `/etc` | overlay: lower = `/etc` of the image, upper = tmpfs | read-write, **not kept** | generated at every boot (see below) |
| `/config` | `ceros-config` | ext4, `nodev,nosuid,noexec` | configuration and identity (§3.1) |
| `/var/lib/switchd` | bind of `/config/switchd` | | switchd's state: committed configurations, stack keys and member id, Raft log, port numbers |
| `/etc/ssh/ssh_host_*_key[.pub]` | binds of `/config/ssh/…` | | the SSH host keys |
| `/etc/machine-id` | bind of `/config/machine-id` | | the machine id |
| `/var` | `ceros-data` | ext4, `nodev,nosuid,noexec` | received software (`/var/lib/ceros/software`), crash reports |
| `/home`, `/root` | binds of `/var/home`, `/var/root` | | home directories (CLI history) |
| `/tmp`, `/run` | tmpfs | | `/run/log/journal`: the journal (below) |
| `/boot/efi` | `ceros-esp` | FAT, mounted only while the update daemon changes the boot state | |

**Logs are kept in memory.** The journal is volatile (`Storage=volatile`, in `/run/log/journal` on tmpfs, at most
64 MB in files of 8 MB, the oldest dropped first), so logging never writes to the boot medium (USB sticks and small
flash wear out). `cer-syslogd` forwards the journal to the configured syslog servers (config reference 1.9, 5.1),
which is where logs are kept. The log of a boot ends with it; crash reports are still written to `/var`.

**No swap.** The image has no swap partition, swap file or zram; `swap.target` is masked and the build fails when
the image brings a swap (fstab entry, swap unit, zram generator). switchd turns off any swap that appears anyway
(config reference 1.9).

**`/etc` is not kept across a reboot.** Every boot starts with the image's `/etc`, and switchd writes what the
configuration says: host name, `/etc/hosts`, `resolv.conf`, the accounts of `system login user` and
`system root-authentication`, its own SSH server and console units. The configuration is the only source of truth,
and a new image never inherits stale files from an older one. (A kept `/etc` would hide the new image's files behind
old copies.) What must survive is on `/config` and is bound in.

### 3.1 The configuration partition

```
/config/
  layout                  version of this layout (for later changes)
  switchd/                /var/lib/switchd: committed revisions (config/), candidate, pending confirmation,
                          stack keys and Raft state (stack/), port numbers, accounts
  ssh/                    SSH host keys (created at the first boot)
  machine-id              created at the first boot
  update/state.json       the update in progress (§4.3)
  backup/<version>/       switchd/config/ as it was before the update away from <version> (the last two are kept)
```

* At the first boot (empty partition), the initramfs creates the machine id and the SSH host keys, and the
  configuration is the factory default (no configuration: every port down, the console logs in as root into the CLI).
* `/var/lib/ceros/config-copy.tar` (data partition) is a copy of `/config` that the update daemon refreshes after every
  change of `/config/switchd`. A configuration file system that cannot be repaired (`fsck -y` fails) is formatted
  again and filled from that copy.
* `request system zeroize` clears the configuration and data partitions (the factory default) and reboots.
  `request system storage cleanup` clears the data partition only.

## 4. Choosing the slot, and automatic rollback

The boot state is a GRUB environment file on the ESP (`/EFI/ceros/grubenv`, 1024 bytes, changed in place):

| Variable | Meaning |
|---|---|
| `ORDER` | the slots in the order they are tried, e.g. `A B` |
| `A_OK`, `B_OK` | `1`: the slot holds a complete, verified image; `0`: it must not be booted (failed or being written) |
| `A_TRY`, `B_TRY` | `1`: the slot was started and has not yet confirmed that it works |
| `A_ROOTHASH`, `A_HASHOFFSET`, `B_…` | the slot's verity root hash and hash offset |
| `A_VERSION`, `B_VERSION` | the version in the slot (shown in the boot menu) |

**GRUB** takes the first slot in `ORDER` with `OK=1` and `TRY=0`, sets its `TRY=1` and saves that **before** it starts
the slot. If no slot qualifies (both tried), it sets both `TRY` to 0 and takes the first slot in `ORDER` with `OK=1`.
This way the switch always boots something, and a missing or damaged `grubenv` means `ORDER="A B"`, both OK.

**The running system confirms the boot**: the update daemon sets its slot's `TRY=0` once switchd runs (it answers on
its socket with the version of the slot). A slot that panics, fails verity, hangs (watchdog) or loses power before
it confirms keeps `TRY=1`. The next boot then takes the next slot in `ORDER`, the backup. Boots without an update are
confirmed the same way, so a backup is taken whenever the active slot no longer starts.

### 4.1 Update bundle

`ceros-<version>-amd64.bundle` is an uncompressed tar file with, in this order:

1. `manifest.json`: version, build time, architecture (`amd64`), platform (`x86_64-efi`), `compatible`
   (`ceros-x86_64`), the image's size and SHA-256, its verity root hash and hash offset, and the oldest version it can
   update from (`min_from`, optional).
2. `manifest.sig`: an **Ed25519 signature** of the exact bytes of `manifest.json`, as base64 with the signing key's id
   (`<key id> <signature>`).
3. `rootfs.img`: the slot image (squashfs, then the verity hash tree).

The signature and the manifest are checked **before** anything is written: the signing key must be one the
**running** image trusts (`/usr/share/ceros/keys/*.pub`). The image is hashed while it is written. Nothing overrides a
missing or wrong signature (`force` doesn't either).

**What is signed, and what is checked when.**

| What | Protected by | Checked |
|---|---|---|
| `manifest.json` (version, platform, `min_from`, image size and SHA-256, verity root hash and hash offset) | the Ed25519 signature | before anything is written |
| `rootfs.img` (kernel, initramfs, every program and library, the hash tree) | its SHA-256 in the signed manifest | while it is read from the bundle; again from the disk after writing the slot (read back past the page cache) |
| every block of the running root file system | dm-verity, with the root hash from the signed manifest | at every read (a program, a library, a kernel module); a wrong block stops the system (§2) |
| the slot's root hash and hash offset | written into the boot state from the signed manifest | by the initramfs, which opens the slot with them |

**Not signed or verified:** GRUB and its configuration, the boot state (`grubenv`, which carries the root hashes),
the kernel command line, and the kernel and initramfs as GRUB loads them (GRUB reads them from the slot without the
hash tree). There is no UEFI Secure Boot. So the scheme protects against damaged media and against installing
unsigned software, but not against someone who can write to the disk directly. Secure Boot with a signed kernel
image that carries its root hash would close that; it is not part of this release.

**Nothing runs from the writable partitions:** `/config` and `/var` (with `/home` and `/root`) are mounted `noexec`, so
every program that runs comes from the verified image.

**Keys.** Release images trust the release keys only. Images built for development (`make image DEV=1`) also trust
the development key in the repository. A new release key is introduced by a release that trusts both the old and the
new one.

### 4.2 Installing on one member

The stack update (reference 3.6) hands the verified bundle to the member's **update daemon** (switchd-update), which:

1. checks the signature, `compatible`, `min_from`, and that the bundle is not the version of the active slot;
2. marks the backup slot `OK=0` (it is about to be overwritten) and writes the image into it while hashing it, then
   reads it back and compares the SHA-256 (a bad disk shows up here, not at boot);
3. mounts the new image (through dm-verity, read-only) and runs **its** `switchd check-config` on the active
   configuration;
4. copies the committed configurations (`/config/switchd/config`) to `/config/backup/<running version>/` and writes
   `/config/update/state.json` (`from`, `to`, the slot, the time, the newest configuration revision, whether the
   switch is alone or a member of a virtual chassis);
5. writes the boot state: the new slot's root hash, offset, version, `OK=1`, `TRY=0`, and puts it first in `ORDER`;
6. reboots the member. (The stack update drained it before: maintenance mode, 5.2 of the reference.)

Up to step 5, the old slot is untouched and first in `ORDER`, so a power loss or error at any point leaves the
switch on the old version. The boot state is written last, in one write of one block.

### 4.3 After the reboot

The new slot starts. GRUB set its `TRY=1`. The update daemon finds the update in `state.json`:

* **Healthy** (switchd runs the new version, has applied the stack's configuration and is current in the stack,
  every member port of the configuration is present) within **5 minutes**: the daemon confirms the slot (`TRY=0`),
  ends the update (`state.json` → done) and switchd leaves maintenance mode. The old version stays in the backup
  slot.
* **Not healthy** within 5 minutes, or switchd fails to start 3 times in a row: the daemon **rolls back**: the new
  slot gets `OK=0`, the old one goes first in `ORDER`, and the member reboots into the old version.
* **The new slot does not even get that far** (kernel panic, verity error, hang): the watchdog or the panic
  reboots it, GRUB sees `TRY=1` and starts the old slot.

When the old slot comes back after a failed update, its update daemon finds `state.json` with an update that did not
finish. It records the failure (`show system software`: `rolled back: <reason>`, syslog), and the stack update
stops. On a switch that is alone (not a virtual chassis), if nothing was committed while the new version ran
(the revision in `state.json` is still the newest), `/config/backup/<old version>/` is put back before switchd
starts, so what the new version converted on reading is gone. Otherwise the configuration is kept: versions read
the stored configurations of the previous release (reference 3.6), and a member of a virtual chassis takes the
stack's configuration from the master as after every restart.

`request system software rollback` boots the backup slot: the same as an update, except that nothing is written but
the boot state. The backup slot must have `OK=1`.

### 4.4 Failures and what happens

| Failure | Result |
|---|---|
| Wrong signature, wrong platform, damaged download | rejected before anything is written |
| Power loss while the backup slot is written | the active slot is untouched and first in `ORDER`; the backup slot has `OK=0` |
| Error in the read-back check or the configuration check | the backup slot stays `OK=0`; the update stops on this member |
| New kernel panics, verity error, hang | `TRY=1` stays; the next boot starts the old slot |
| GRUB cannot read the new slot (damaged squashfs, no kernel) | GRUB marks it `OK=0` and starts the other slot in the same boot |
| New switchd not healthy within 5 minutes | the daemon rolls back and reboots |
| The new version damages the configuration | the old slot puts the backup copy back (alone), or takes the stack's configuration |
| `grubenv` unreadable | GRUB uses `ORDER="A B"`, both OK; `show system software` and `show system version` say `boot state: missing` / `damaged` / `unreadable` |
| A slot's partition cannot be read (disk failing, removed) | `show system software` and `show system version` show the slot as `unreadable: …` (read from the disk directly, at most 5 s) |
| The disk stops answering | every read and write fails at its deadline (§7); an alarm tells the CLI sessions; nothing hangs |
| Configuration file system damaged | `fsck -y`; else formatted and filled from the copy on the data partition |
| Data file system damaged | formatted again, logs lost, the switch works |

## 5. Contents of the image

Debian 13 (trixie), built with mmdebstrap (minimal base), then:
* kernel and boot: `linux-image-amd64`, the NIC firmware packages (`firmware-misc-nonfree`, `firmware-bnx2x`,
  `firmware-realtek`, `firmware-netronome`…), `systemd`, `udev`, `dracut` (initramfs), `cryptsetup-bin`
  (`veritysetup`), `e2fsprogs`, `dosfstools`, `grub-efi-amd64-bin` (to install the boot loader), `kmod`;
* what switchd uses: `iproute2` (`ip`, `bridge`, `tc`, `ip vrf exec`), `nftables`, `ethtool`, `openssh-server`
  (switchd's CLI SSH server), `passwd`/`login`/`util-linux` (accounts, `runuser`), `bash` (`start shell`);
* tools for testing and finding faults: `tcpdump`, `iperf3`, `mtr-tiny`, `iputils-ping`, `iputils-tracepath`,
  `traceroute`, `netcat-openbsd`, `socat`, `curl`, `dnsutils`, `strace`, `lsof`, `htop`, `procps`, `pciutils`,
  `usbutils`, `i2c-tools`, `dmidecode`, `less`, `vim-tiny`.

Not in the image: DHCP clients, ifupdown, NetworkManager, systemd-networkd (masked), systemd-resolved,
systemd-timesyncd (switchd has its own NTP client), rsyslog (switchd forwards syslog), cron, apt and dpkg (removed at
the end of the build), documentation and locales. The OS's own SSH server (`ssh.service`) is not enabled: the CLI SSH
server (`system services ssh`, reference 5.1) is the only way in over the network, and `start shell` leads on to a
Linux shell.

`/etc/ceros-release` names the version, the platform and the layout version.

Files the system needs at fixed paths are part of the image, because its root is read-only: the kernel's STP helper
`/sbin/bridge-stp` (it hands switchd's bridge to cer-rstpd; a test keeps it identical to what cer-rstpd expects), and
the tmpfiles entry for sshd's `/run/sshd`.

## 6. Building and installing

* `make image` builds the image in a Debian 13 container (the host needs only docker). The steps are mmdebstrap,
  `mksquashfs` (zstd), `veritysetup format`, the manifest, and the signature with `CEROS_SIGNING_KEY` (the
  development key by default). It writes `dist/ceros-<version>-amd64.bundle` and `dist/ceros-<version>-amd64.img`,
  a complete disk (both slots hold the version) for virtual machines or `dd` onto a disk. Builds are reproducible:
  `SOURCE_DATE_EPOCH` is the commit time.
* **Installer**: the disk image on a USB stick boots as usual. `request system software install-disk <disk>`
  (console, super-user, asks for confirmation naming the disk and its size) partitions the target disk, writes the
  ESP, both slots and an empty configuration, and then the switch boots from it.

## 7. Hanging devices

switchd and the cer- daemons never wait without limit for the kernel, a disk or a tool. Linux cannot cancel a
system call that waits inside the kernel (a dying disk, a NIC driver that holds a lock), so every such call runs with
a **deadline** and the caller goes on with an error when it passes:

| Calls | Deadline |
|---|---|
| file and disk access (configuration store, state files, bundles, the boot state) | 10 s per operation |
| one read, write or sync of a slot, a bundle or a USB stick (4 MiB: slots are synced after every 4 MiB, bundles are written back to the disk 4 MiB at a time before they are renamed) | 60 s |
| netlink requests, ethtool ioctls, `/sys` and `/proc` | 5 s |
| system tools (`nft`, `ip`, `bridge`, `systemctl` …; killed with everything they started) | 10 s (account tools 1 min, `veritysetup` 1 min, a new image's configuration check 2 min) |
| reading a slot's own headers for `show system software` / `show system version` | 5 s |

A call that misses its deadline is **stuck** on its resource (a disk, `/config`, `/var`, the kernel's network
configuration lock `rtnl`, a tool). Further calls on that resource fail at once instead of piling up, until the stuck
call returns. switchd raises an **alarm**: a log entry, a notice to every CLI session
(`member 2: ALARM: the kernel's network configuration (rtnl lock; usually a NIC driver that hangs) does not answer
(link add ae1, since 12:04:31) …`), and a list in `show system processes`. A commit that needs it fails with the
reason. The alarm clears when the device answers again. **Nothing reboots**: the member keeps forwarding with what is
set already.

switchd itself has a systemd **watchdog** (60 s) that it feeds only while its loops make progress (data plane, stack
control, daemon supervisor): a loop that stops (a bug, a deadlock) gets switchd restarted, and the daemons keep
running. A hanging device does not trigger it, because every call returns at its deadline. The cer- daemons have the
same watchdog (10 s). The libraries are `pkg/hwio` (deadlines, stuck resources, file functions), `pkg/sysexec`
(tools) and `pkg/nlx` (netlink).
