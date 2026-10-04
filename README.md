# cerOS

cerOS turns Linux machines into a managed Ethernet switch: one Go program, `switchd`, owns the network ports of a
server (or a small ARM board) and configures the kernel's bridge, bonds, VLANs, routing and filters from a
Junos-style configuration. Several machines cabled together form **one virtual chassis** (a stack, like a Junos
Virtual Chassis) that is configured, managed and seen from the outside as a single switch.

The project's priorities, in this order: no dropped frames, then speed; a CLI that never leaves you without access;
every change through commit and confirm; management by CLI or by an orchestrator through a REST API (no web
interface).

> Status: under active development and tested in a lab of VMs and physical machines. Not yet used in production.

## Features

* **Junos-like CLI** (`swcli`): operational and configuration mode, `set`/`delete`/`edit`, `show | compare`,
  `commit check`, `commit confirmed` with automatic rollback, revisions and `rollback <n>`, shared, private and
  exclusive candidates, `| display set|json`, completion and `?` help. Sessions survive restarts of switchd.
* **Hitless apply**: only what changed is touched, without link flaps, and a port never passes through a state that
  combines old and new permissions.
* **Switching**: access and trunk ports, native VLANs, VLAN and port MTUs (jumbo frames), static and LACP bundles,
  storm control, MAC limits, flow control, interface ranges with wildcards, port mirroring.
* **Virtual chassis**: up to 16 members over direct stacking cables (any topology; a ring survives a cable cut),
  mutual TLS between members, Raft for the configuration, BFD-style failure detection, client traffic between members
  in VXLAN stack tunnels, maintenance mode (drain a member without losing traffic), rolling software updates through
  the stack with an update daemon that rolls back on its own.
* **MC-LAG**: a bundle with ports on two members is an MC-LAG: one LACP partner for the device, split horizon, MAC and
  multicast group synchronisation, minority handling.
* **RSTP**: the whole stack is one RSTP bridge; port states survive restarts.
* **IGMP/MLD snooping**, on by default, never dropping traffic for lack of a querier.
* **VXLAN** to remote VTEPs: the stack is one VTEP with head-end replication and remote MACs shared between members.
* **Routing**: irb interfaces, routed ports and subinterfaces, routing instances (VRFs), static routes, DHCP client;
  the switch's own addresses are protected.
* **Management**: a chassis management address that follows the master, a management routing instance, the CLI over
  SSH (its own sshd) and on serial/display consoles, local users with classes, syslog (UDP/TCP/TLS), NTP, DNS, LLDP
  (the stack is one system).
* **Diagnostics**: `show system bottlenecks` (PCIe links, queues, interrupts, rings, offloads, drops),
  `show system limits`, `show system offload`.

## Documentation

* [docs/config-reference.md](docs/config-reference.md): the specification of the configuration and the commands.
  The code follows it; where they disagree, that is a bug.
* [docs/stack-protocol.md](docs/stack-protocol.md): the stacking protocol (wire format, TLS, Raft, stack tunnels).
* [docs/os-image.md](docs/os-image.md): the operating system image: disk layout, file systems, signed A/B updates
  and automatic rollback.
* [PLAN.md](PLAN.md): architecture, design decisions and the roadmap.
* [STATUS.md](STATUS.md): the development log: what is done, what is next.
* [tools/wireshark](tools/wireshark): a Wireshark dissector for the stacking protocol.

## Requirements

cerOS runs as a **firmware image** (docs/os-image.md): a read-only Debian 13 system with switchd, the tools it needs
and a set of network test tools, and nothing that configures the network by itself (no DHCP client, no network
manager). The disk holds two system slots (the running version and the previous one as a backup), a configuration
partition and a data partition. Updates are signed bundles; a version that does not come up returns to the previous
one by itself.

* x86_64 with UEFI (servers, mini PCs, virtual machines with OVMF), a disk of at least 8 GB, and at least two network
  ports (one for management, one or more switched). arm64 follows.
* Building: Go (see `go.mod`) and docker.

## Building

```
make            # go vet, gofmt check, unit tests, every program into bin/
make swcli      # one program only (switchd, swcli, switchd-update, cer-<daemon>)
make image      # dist/ceros-<version>-amd64.bundle (signed update) and dist/ceros-<version>-amd64.img (disk, DISK=8 GiB)
```

The bundle is signed with `CEROS_SIGNING_KEY` (a file made by `switchd keygen <name>`). Without it the development key
in `image/keys/dev.key` signs it, and only images that trust `image/keys/dev.pub` accept such bundles. A release build
sets `CEROS_SIGNING_KEY` to the release key and `CEROS_TRUSTED_KEYS` to a directory with the release public keys only.

## Installing

Write the disk image to the switch's disk (or a USB stick, or use it as a VM disk):

```
dd if=dist/ceros-<version>-amd64.img of=/dev/<disk> bs=4M conv=fsync
```

At the first boot the switch has no configuration: every port is down and the serial console (115200 baud) and the
screen log in as root into the CLI. Configure a management address (`set interfaces <port> management`, reference
5.3.4) and `system root-authentication`, then commit. Later versions are installed with
`request system software add <bundle>` from the CLI, member by member (reference 3.6).

## Testing

```
go test ./...                      # unit tests (no root, no network changes)
go test -tags lab ./test/lab -v    # integration tests against the lab VMs (lab/README.md)
test/image/test-update.sh <v1> <v2> # the image in QEMU: update, rollback, power loss, bad bundles
```

The unit tests never change the network of the machine they run on. The lab tests reconfigure the lab switches and
servers described in `lab/README.md`.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/switchd` | the switch daemon, and the tools `check-config`, `bundle`, `keygen`, `verify-bundle` |
| `cmd/swcli`, `cmd/switchd-update` | the CLI client (login shell); the update daemon |
| `cmd/cer-*` | the protocol and service daemons (reference 1.9), each built on its own |
| `cmd/rtest` | runs the routing protocols without a switch (test/interop) |
| `pkg/` | reusable libraries: no imports from `internal/` (checked by `test/layout`): `ipc` (calls and state topics between programs), `journal`, `sdnotify`, `netdev` (devices, teams, bridge, routes), and the protocols `lacp`, `rstp`, `lldp`, `bfd`, `ospf`, `rib`, `dhcp`, `ntp`, `syslog` |
| `internal/svc`, `internal/daemonkit`, `internal/supervise` | the service protocol between switchd and the daemons, the daemons' common main, switchd's supervisor |
| `internal/mclag`, `internal/stp`, `internal/ribd`, `internal/bfdd` | the logic of cer-mclagd, cer-rstpd, cer-ribd and cer-bfdd |
| `test/daemons`, `test/layout` | every daemon against a fake switchd; the layering rules |
| `internal/schema`, `internal/config`, `internal/model` | configuration schema, parser/formats, typed model and commit checks |
| `internal/commit` | candidates, commit, confirmation, rollback |
| `internal/cli`, `internal/swcli`, `internal/rpc`, `internal/rpcserver` | the CLI, its client, the session protocol and its server |
| `internal/dataplane` | kernel state: bridge, bonds, VLANs, L3, filters, tunnels, multicast, VXLAN |
| `internal/daemon` | switchd itself: wiring, stack control, MC-LAG, LACP/LLDP/RSTP glue, software updates |
| `internal/stack` | stacking: links, mesh routing, Raft control, PKI and joining |
| `internal/lacp`, `internal/lldp`, `internal/rstp`, `internal/dhcp`, `internal/ntp`, `internal/syslog` | protocols |
| `internal/access`, `internal/osconf` | accounts, SSH, consoles; host name and resolver |
| `internal/software`, `internal/updated` | signed bundles, boot state and slots; the update daemon |
| `internal/diag`, `internal/inventory` | diagnostics; port numbering and hardware facts |
| `packaging` | systemd units |
| `image`, `test/image` | the operating system image (build in docker, GRUB, initramfs) and its QEMU tests |
| `lab`, `test/lab` | the Proxmox test lab (Ansible) and its integration tests |
| `docs` | the specification |
