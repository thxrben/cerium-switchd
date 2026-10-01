# cerOS

cerOS turns Linux machines into a managed Ethernet switch: one Go program, `switchd`, owns the network ports of a
server (or a small ARM board) and configures the kernel's bridge, bonds, VLANs, routing and filters from a
Junos-style configuration. Several machines cabled together form **one virtual chassis** (a stack, like a Junos
Virtual Chassis) that is configured, managed and seen from the outside as a single switch.

The project's priorities, in this order: no dropped frames, then speed; a CLI that never leaves you without access;
every change through commit and confirm; a web interface last.

> Status: under active development and tested in a lab of Debian 13 VMs and physical machines. Not yet used in
> production.

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
* [PLAN.md](PLAN.md): architecture, design decisions and the roadmap.
* [STATUS.md](STATUS.md): the development log: what is done, what is next.
* [tools/wireshark](tools/wireshark): a Wireshark dissector for the stacking protocol.

## Requirements

* Linux with a recent kernel (developed on Debian 13, kernel 6.12) on `amd64`, `arm64` or `arm`.
* `iproute2` (`ip`, `bridge`), `nftables`, `systemd`. switchd takes over the machine's network configuration: it masks
  the operating system's network services (from the next boot) and manages every port itself. Install it on a
  machine you can reach through a console.
* Go (see `go.mod`) to build.

## Building

```
make            # go vet, gofmt check, unit tests, bin/switchd (bin/swcli is a link to it)
make cross      # static binaries for amd64, arm64 and arm in dist/
make package    # dist/ceros-<version>.tar.gz for 'request system software add'
```

## Installing

On the switch (as root):

```
install -m 0755 switchd /usr/local/sbin/switchd
ln -sf /usr/local/sbin/switchd /usr/local/bin/swcli
echo /usr/local/bin/swcli >> /etc/shells
install -m 0644 packaging/switchd.service /etc/systemd/system/switchd.service
systemctl daemon-reload && systemctl enable --now switchd
swcli
```

switchd then keeps its systemd units current (and starts the update daemon `switchd-update` itself). Later versions
are installed with `request system software add <package>` from the CLI, member by member. `lab/deploy.yml` does the
first installation with Ansible.

## Testing

```
go test ./...                      # unit tests (no root, no network changes)
go test -tags lab ./test/lab -v    # integration tests against the lab VMs (lab/README.md)
```

The unit tests never change the network of the machine they run on. The lab tests reconfigure the lab switches and
servers described in `lab/README.md`.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/switchd` | the program: daemon, CLI (`swcli`), `check-config`, `package`, `update-daemon` |
| `internal/schema`, `internal/config`, `internal/model` | configuration schema, parser/formats, typed model and commit checks |
| `internal/commit` | candidates, commit, confirmation, rollback |
| `internal/cli`, `internal/swcli`, `internal/rpc` | the CLI, its client and the session protocol |
| `internal/dataplane` | kernel state: bridge, bonds, VLANs, L3, filters, tunnels, multicast, VXLAN |
| `internal/daemon` | switchd itself: wiring, stack control, MC-LAG, LACP/LLDP/RSTP glue, software updates |
| `internal/stack` | stacking: links, mesh routing, Raft control, PKI and joining |
| `internal/lacp`, `internal/lldp`, `internal/rstp`, `internal/dhcp`, `internal/ntp`, `internal/syslog` | protocols |
| `internal/access`, `internal/osconf` | accounts, SSH, consoles; host name and resolver |
| `internal/software`, `internal/updated` | software packages, installation and the update daemon |
| `internal/diag`, `internal/inventory` | diagnostics; port numbering and hardware facts |
| `packaging` | systemd units |
| `lab`, `test/lab` | the Proxmox test lab (Ansible) and its integration tests |
| `docs` | the specification |
