# cerOS (Cerium) — Linux HA switch (plan)

Goal: turn ordinary Linux boxes (any arch, any NIC) into a stack of HA-capable L2
switches with MC-LAG, VXLAN, a Junos-like CLI (SSH + serial), and a REST API for an external orchestrator (no web UI, decided 2026-10-04).

## 1. Key decisions

| Topic | Decision | Why |
|---|---|---|
| Language | **Go** (single static binary) | Cross-compiles to amd64/arm64/armv7/riscv64 with no runtime deps; mature netlink (`vishvananda/netlink`), SSH (`x/crypto/ssh`), Raft (`hashicorp/raft`), TLS libraries. |
| Data plane | **Linux kernel**: VLAN-aware bridge, bonds, vxlan, macsec | Works on *every* NIC. Where the NIC/driver supports switchdev/offload, the kernel offloads automatically. We never touch hardware directly, so mixed hardware is fine. |
| Control of kernel | netlink only (no shelling out to `ip`) | Fast, transactional-ish, event driven (we subscribe to link/FDB changes). |
| LACP | **own userspace LACP** (AF_PACKET), bond in non-LACP mode, members toggled by the LACP state machine | Linux `802.3ad` bonding lets us set `ad_actor_system` but not the port numbers. Two MC-LAG peers would announce colliding port IDs. Userspace LACP gives full control, the same approach SONiC/Cumulus take. |
| Cluster/stack state | **Raft** (hashicorp/raft) across all stack members | Gives one committed config and one source of truth, with leader election for the "stack master". |
| Secure inter-switch | **mTLS 1.3** for all control-plane traffic (own stack CA, auto-enrolment with a join token). Data plane: see §6. | |
| Config format | Hierarchical tree, Junos `set`-style + curly-brace display, stored as versioned JSON in Raft | Supports commit, rollback N, compare, and `commit confirmed`. |

## 2. Architecture

```
               ┌────────────────────── one node ──────────────────────┐
  ssh ─► sshd ─┤                                                      │
  ttyS*/USB ───┤─► swcli (login shell) ──unix socket/gRPC──► switchd  │
  browser ─────┼──────────────── HTTPS (REST + UI) ────────► switchd  │
               │                                             │        │
               │  switchd modules:                           │        │
               │   config engine ◄──► raft store ◄── mTLS ──► peers   │
               │   reconciler ──netlink──► kernel (bridge/bond/vxlan) │
               │   lacpd  ─ AF_PACKET on member ports                 │
               │   mclagd ─ peer protocol (mTLS) + FDB sync           │
               │   vxlan-ctl ─ remote VTEP/MAC distribution           │
               │   syslog forwarder, metrics, health                  │
               └──────────────────────────────────────────────────────┘
```

* **switchd**: one daemon per node, runs as root (CAP_NET_ADMIN + CAP_NET_RAW).
* **swcli**: thin client. Used as login shell for SSH and serial users and as a
  local command. All logic lives in switchd, so CLI, web and API behave the same.
* **Reconciler**: desired state (committed config for *this* member) is diffed
  against the actual kernel state and only the difference is applied. Also runs
  after link events and restarts, so the node is self-healing and idempotent.

## 3. Stacking model (Junos Virtual-Chassis-like)

**Three separate planes** (see docs/config-reference.md §1.5):

| Plane | Ports | Carries |
|---|---|---|
| Data | switch ports (e.g. 10G), VXLAN | client traffic only, no IP on any port |
| Stacking | dedicated stacking ports, direct 1:1 cables in a ring | Raft/config, state, MC-LAG sync, RSTP relay, BFD (EtherType 0x88b5, untagged), and client traffic between members in stack tunnels (VXLAN over a hidden internal instance; decided 2026-09-30) |
| Management | IP on any VLAN (IRB-like) or a dedicated port, VRF `mgmt_ceros` | administration only: SSH, web, ping, syslog, NTP, DNS, updates |

* Every switch is a **member** with an ID (1–16). Interfaces are named `<member>/<linux-ifname>`, plus
  stack-global `ae<N>`. `interface-range` (member-range, wildcards) handles large and hot-plugged port sets.
* **Stacking ports** are designated locally (`request virtual-chassis vc-port set pic-slot <card> port <port>`, Junos VC syntax), because a switch needs them before it
  has any configuration. Each stacking link runs a reliable L2 stream (seq/ack/retransmit/fragmentation, exposed as a
  `net.Conn`) with **TLS 1.3 mTLS** on top, plus IP-less **BFD** for fast failure detection.
* **Topology**: chain, ring or mesh. Members exchange link-state (adjacency) over the stacking plane, and messages to
  non-adjacent members are relayed hop by hop along the shortest live path. Each hop is TLS-protected, and all
  members are authenticated stack members.
* The stacking plane **never** listens on data ports. Stacking-EtherType frames on access, trunk or VXLAN ports are
  client traffic and are switched normally. Tested explicitly.
* Config and CLI/web work from any member. Writes go through the Raft leader over the stacking plane, and each
  member applies only its own part of the tree.
* Joining: a new switch with designated stacking ports announces itself on them. `request virtual-chassis member add <id>
  token <t>` authorises it → it gets a certificate from the stack CA → it joins Raft (≤7 voters, the rest non-voting).
* **Why Raft, and why a voter limit.** The stack needs one agreed configuration and must never let two halves of a
  split stack both accept commits (split brain). That requires a majority vote, which is what Raft does; it is
  used as a library (hashicorp/raft) rather than written by hand. Raft only guards configuration changes and the
  choice of the stack leader: the data plane, MC-LAG and LACP never wait for it. Voters: every member up to 7
  (always an odd number); beyond 7, extra members are non-voting replicas that receive everything but do not
  vote. More voters make every commit wait for more members and do not improve availability much: 7 voters
  survive 3 simultaneous failures. When a voter fails permanently, a replica is promoted automatically (possible
  while a majority of voters is alive), spread across different stacking links where possible.
* Raft needs a majority to commit. With 2 members, a third lightweight **witness** is recommended (e.g. a small board
  with 2 NICs, cabled with stacking links to both). Without quorum, the data plane **keeps running** on the last
  committed config and only config changes are blocked.

## 4. Features and implementation

### 4.1 L2 switching
* One bridge `br0` per member, `vlan_filtering=1`, access/trunk/native VLAN per port.
* MAC ageing, static MACs, per-port MAC limits, storm control (tc police), BPDU guard.
* Loop prevention: **RSTP (802.1w)** plus BPDU guard/root guard/edge ports (see 4.10).

### 4.2 Jumbo frames
* **Per port**: native (`mtu 9216` → netdev MTU on port/bond/member ports).
* **Per VLAN**: the Linux bridge has no per-VLAN MTU. We enforce it with
  nftables `bridge` family rules (`vlan id X meta length > N drop`) plus a counter.
  The effective limit is min(port, VLAN).
* MTU uses the Junos convention (frame size incl. the 14-byte Ethernet header, default 1514).
* Commit-time validation catches mismatched MTUs (stacking ports < largest data MTU + 58,
  VXLAN underlay < overlay + 50, etc.).

### 4.3 MC-LAG
* The pair is 2 members of one stack. The **stack tunnel** between them replaces a peer-link (no extra cable, decided
  2026-09-30); the stacking protocol carries MAC sync, state and consistency. Two-member split: both keep forwarding
  at all costs; with 3+ members the minority part holds its legs.
* Both peers announce the same LACP system ID/priority and disjoint port-number ranges, so the partner sees one LAG.
* **MAC sync**: switchd watches netlink FDB events. MACs learned on an MC-LAG bond are sent to the peer and installed
  as static FDB entries on the same bond there. Ageing is coordinated.
* **Split horizon**: traffic arriving from the peer's tunnel must not leave on an MC-LAG bond whose peer leg is up
  (nft rule). Third members' broadcast/multicast is delivered by one member per bundle (primary while its leg is up).
* **Failure matrix**: config reference §5.6.
* Consistency checks (VLANs, MTU, LACP params). A mismatch takes the secondary's leg down with a reason in `show mclag`.

### 4.4 VXLAN
* One kernel `vxlan` device per member in *vnifilter* (single-device, many VNIs) mode.
  Mapping `vlan X ↔ vni Y` uses bridge VLAN tunnel info.
* **Full mesh with control plane** (EVPN-like, over our mTLS channel):
  every member announces its VTEP IP and local MACs per VNI. Peers install head-end
  replication entries (`00:00:00:00:00:00 dst <vtep>`) and remote MACs, with learning off.
  This avoids flood-and-learn and gives instant convergence. Flood-and-learn stays available as an option.
* An MC-LAG pair can use an **anycast VTEP** IP so remote members see one VTEP.
* External, non-stack VTEP peers can be configured statically.

### 4.5 Management
* Per member: **one IP interface in VRF `mgmt`**, attached to any VLAN of the bridge (IRB-like) or to a dedicated,
  non-switched port. Static IPv4/IPv6 and/or DHCPv4, with a default gateway per family.
* A dedicated 1G management port is just an access port in the mgmt VLAN, or a dedicated port.
* No routing between mgmt and data. SSH, web, syslog, NTP and DNS bind into VRF `mgmt`. Routing between VLANs
  (irb interfaces in the default instance) is Phase 4b.
* Without a `management` block, the host's existing network config is left untouched (safe first install).

### 4.6 CLI (Junos-like)
* Operational mode: `show interfaces [terse|extensive]`, `show ethernet-switching table`,
  `show vlans`, `show lacp interfaces`, `show mclag`, `show vxlan`, `show virtual-chassis`,
  `show system alarms`, `monitor interface`, `request system reboot member N`, …
* Configuration mode: `configure [private|exclusive]`, `set`, `delete`, `edit`, `up`, `top`,
  `show`, `show | compare`, `show | display set`, `commit`, `commit check`,
  `commit and-quit`, `commit confirmed N`, `commit comment "…"`, `rollback N`, `exit`.
* Tab completion and `?` help are driven by the same schema that validates config.
* **The CLI must never fail**: parsing and completion run server-side in switchd with panic recovery per command,
  and parsers are fuzz-tested. swcli degrades gracefully (it reconnects and explains) when switchd is unavailable.
* Pipes: `| match`, `| except`, `| count`, `| no-more`, `| display set|json`.
* Users/classes defined in config (`system login user …`). They are synced to local
  accounts (with swcli as shell) plus SSH keys.

### 4.7 Access paths
* **SSH**: see question 1.
* **Serial**: switchd auto-detects `ttyS*`, `ttyUSB*`, `ttyACM*` (configurable
  list/baud under `system ports console`) and starts `agetty` on them via a
  systemd template unit. Login → swcli.

### 4.8 REST API (no web UI; decided 2026-10-04)
* No web interface on the switch. An orchestrator running elsewhere manages single switches and virtual chassis
  through a REST API: one API per virtual chassis (on the master, `cme` address) or per single switch.
* HTTPS in the management instance (configured or temporary self-signed certificate with pin), the users and classes
  of the CLI, everything the CLI can do with the same semantics (Phase 18).
* `/healthz`, `/readyz`, `/metrics` (Prometheus) for monitoring.

### 4.9 Syslog
* `system syslog host <ip> [port N] [transport udp|tcp|tls] [facility …] [severity …]`.
* RFC 5424, buffered with retry for TCP/TLS, source in mgmt VRF. Also a local ring buffer
  (`show log`) and journald.

### 4.10 RSTP
* **Own RSTP implementation in Go**. The bridge runs with `stp_state=2` (user mode)
  and switchd sends/receives BPDUs via AF_PACKET and sets port states via netlink.
  Why not `mstpd`: an MC-LAG pair has to act as **one logical STP bridge**, and
  mstpd cannot do that. Owning the implementation makes it possible:
  * Both peers use the same bridge ID (derived from the shared MC-LAG system MAC).
  * The primary peer computes the STP state for MC-LAG ports and syncs it to the
    secondary. The secondary relays BPDUs received on its MC-LAG leg to the primary.
  * The peer link is never blocked.
  * On peer loss, the survivor continues with the same bridge ID, so the rest of the network sees no topology change.
* Per-port: `edge`, `bpdu-guard`, `root-guard`, `cost`, `priority`. Per-bridge: priority, timers.
* Later option: MSTP (per-VLAN-group instances), if needed.

### 4.11 Hardware acceleration and drop watchdog
Priority order: **1. no dropped frames, 2. speed.** Offload is used wherever the hardware can do it, but never in
a way that can drop traffic when the hardware misbehaves:
* **Never `skip_sw`**: tc rules (mirroring, storm control, per-VLAN MTU filters) are always installed so that
  the kernel offloads them when possible (`in_hw`) and keeps the software path otherwise. The same applies to
  bridge FDB/VLAN offload via switchdev: the kernel falls back to software for anything the ASIC rejects.
* **Capability probe** per NIC at startup and on hot-plug: driver, switchdev support, `ethtool -k` features
  (TSO/GSO/GRO, checksum, rx/tx VLAN offload, UDP tunnel (VXLAN) offload, MACsec offload, hw-tc-offload),
  ring sizes, max MTU, and the number of queues.
* **Tuning for no drops**: RX/TX rings at max, RSS across all queues, IRQ affinity spread, optional pause frames,
  GRO on. Each tuning step is recorded, so it can be reverted.
* **Runtime offload watchdog** (every few seconds): reads kernel and driver counters (rx_missed, rx_fifo, rx_crc,
  rx/tx_dropped, `ethtool -S` drop/discard/error counters) and tc/FDB `in_hw`/`not_in_hw` state.
  If drops or errors rise and correlate with an offload feature (for example checksum errors after enabling
  rx-checksum, or VXLAN offload errors), the feature is **disabled on that port** (software fallback) and an
  alarm is raised (`show system alarms`, syslog). The change is kept until an operator clears it.
  Pure capacity drops (rx_missed at line rate) raise an alarm with a hint instead (rings, queues, CPU).
* Config: `system offload mode auto|disable`, per-interface `offload disable`, and `system offload watchdog { interval; threshold }`.
* Future: XDP/eBPF fast path for 25G+/100G, with the same "fall back instead of drop" rule.

### 4.12 Port mirroring (analyzer)
* Junos syntax: `forwarding-options analyzer <name> input ingress|egress interface <if>` / `output interface <if>`.
* Implementation: tc `clsact` + `matchall` + `mirred egress mirror`. It is offloaded to the ASIC when supported
  (switchdev), with the software path otherwise (no skip_sw).
* Optional VLAN-based input (`input vlan <v>`, via a flower match on the VLAN ID).
* Commit checks: the output port must not be an input port. A warning is shown if the output port also carries switched traffic.

### 4.13 Commit semantics (safety)
* `commit check` validates the candidate (schema, cross-references, per-member hardware limits such as max MTU)
  and prints errors (which block the commit) and warnings (which don't).
* **Every commit must be confirmed** (default `system commit confirmation required`, timeout 10 min,
  configurable): a `commit` applies the change and starts a rollback timer. `commit` (again, with no changes) or `confirm`
  makes it permanent. If it isn't confirmed in time, switchd **automatically reverts** to the last confirmed revision.
  The pending state is persisted, so a reboot or crash during the window also reverts.
* A failed apply on any member causes an immediate automatic revert, whatever the timer says.
* Later phases add post-commit health checks (mgmt still reachable, stack peers up, MC-LAG healthy). If a check fails
  inside the window, the commit is reverted automatically.

### 4.14 Hitless reconfiguration (no link flaps, no leaks)
A commit (and every rollback, manual or automatic) must never take down links or traffic that the change does not
concern, and must never leak frames between interfaces or VLANs while it is being applied.

* **Diff-driven apply.** The reconciler compares old and new desired state per object (port, bond, VLAN membership,
  VXLAN device, tc/nft rule set, mirror session). Unchanged objects get **no netlink/tc/nft write at all**. Nothing is
  ever rebuilt from scratch: `br0`, bonds and VXLAN devices are modified in place, never deleted and recreated,
  unless a kernel attribute is immutable (then only that one device is recreated, and `commit check` says so).
* **No link down for reconfiguration.** VLAN membership, PVID, descriptions, storm control, MAC limits, mirroring and
  RSTP parameters are all changed on the live link. Attributes that do reset a link on some drivers (e.g. MTU on
  certain NICs, bond hash policy) are detected per driver and listed by `commit check` as
  `warning: <if>: this change resets the link (driver <x>)` before you commit. Only the changed interface is affected.
* **Tighten before loosen (no leaks).** Operations are ordered so that each intermediate state of a port is a subset of
  either its old or its new permissions, never a union of them:
  1. restrictions first: install new filters/police/BPDU-block rules, remove VLANs, stop mirror sessions,
     remove ports from bonds/bridge;
  2. then permissions: add VLANs, set the PVID (a PVID change is remove-old, then add-new-with-pvid; never both at once),
     add ports, start mirror sessions (only after their output port has left switching);
  3. only then remove the old, now unused filters.
  A port that joins the bridge carries no VLAN at all until its configured VLANs are added
  (`default_pvid 0` on `br0`), so it never lands in VLAN 1 by accident. A port leaving a bond is removed from
  forwarding before it becomes a standalone port.
  The price is a sub-millisecond gap on the **changed** port only, which is preferable to a leak.
* **Unit-tested planner.** The op planner (old state, new state → ordered op list) is pure code. Property tests check,
  over random old/new configurations, that (a) no op touches an object whose desired state did not change, and (b)
  every intermediate state satisfies the subset rule above.
* **Lab tests** (Phase 3 onward): continuous traffic on unchanged ports during a stream of commits/rollbacks must show
  zero loss and zero carrier changes; sniffers on every port must never see a frame from a VLAN that the port has in
  neither its old nor its new configuration.

### 4.15 Owning the operating system
switchd must be the **only** thing that configures switch ports, bridges, bonds, addresses, routes, DNS, NTP, the
serial console and the firewall. Everything else that touches those is a conflict. Target OS: **minimal Debian 13**
(also Debian-based ARM images such as Armbian), no desktop, no NetworkManager.

* **Takeover** (`switchd takeover`, run by the .deb postinst, reversible with `switchd release`):
  1. **Preflight report**: lists every conflict found, without changing anything:
     network managers (NetworkManager, systemd-networkd, ifupdown/`networking.service`, netplan, connman, dhcpcd,
     dhclient, wpa_supplicant, ModemManager, cloud-init network), firewalls (firewalld, ufw, iptables/nftables services,
     docker), avahi/mDNS, systemd-resolved, time daemons, getty on the console ports.
  2. **Import**: the current management address, gateway, DNS and NTP are turned into the initial configuration
     (`virtual-chassis member 1 management …`), so SSH keeps working after the takeover.
  3. **Disable**: conflicting services are stopped and **masked** (not uninstalled), and the original state is recorded
     so `release` can restore it. Services switchd drives itself (chrony/timesyncd, the console getty, sshd) keep
     running, but their config files are generated by switchd from the configuration.
  4. **Kernel settings** (sysctl + modprobe, persistent): `br_netfilter` blacklisted (otherwise bridged frames go
     through iptables); `addr_gen_mode=1`, `accept_ra=0`, `disable_ipv6` on non-management interfaces, so switch and
     stacking ports never get an IPv6 link-local address or SLAAC; `rp_filter`/`arp_ignore` for the mgmt VRF;
     bridge `default_pvid 0`.
  5. The management change is applied like a **commit that needs confirmation**: if the imported management
     address does not work and nobody confirms, switchd reverts to the original networking.
* **At boot** nothing but switchd brings a port up. Ports stay down until switchd has applied the committed
  configuration (no loops or leaks during boot). switchd starts before `network-online.target`.
* **At runtime** the reconciler watches netlink for foreign changes to objects switchd owns (an address added,
  a port enslaved, a link set up/down, a VLAN added, a qdisc changed). It reverts them and logs
  `warning: foreign change on <if> reverted`. `show system alarms` lists active conflicts. A service that is unmasked
  again by the admin or a package upgrade raises an alarm.
* **Opt-out** for NICs switchd must not touch (e.g. a Wi-Fi card or a storage NIC): a planned statement
  `system ports unmanaged [ <linux-name> … ]` (to be specified in the reference before implementing it).
* **Appliance image (decided 2026-10-01, replaces the .deb path)**: cerOS ships only as a firmware image: Debian 13
  built with mmdebstrap, read-only squashfs with dm-verity, two system slots (A/B) with GRUB boot counting, a
  configuration and a data partition, signed update bundles (Ed25519) and automatic rollback. x86_64 UEFI first,
  arm64 later. Design: docs/os-image.md. SWUpdate and RAUC were evaluated; their concepts (signed manifest,
  streaming hashed install to the inactive slot, ORDER/TRY boot state) are implemented in switchd-update instead,
  because GRUB has no boot counter of its own, the health check and stack rollout are switchd's, and the stack
  already uses Ed25519 keys. Alpine was considered and rejected: switchd depends on systemd.

## 5. Configuration

The configuration language is specified in **docs/config-reference.md**. That covers syntax, formats, directives,
the commit model, the semantics and interactions of every statement, and the validation rules. Its examples are
parsed and validated by the test suite.

## 6. Security vs. performance (important)

| Channel | Encryption | Cost |
|---|---|---|
| Stacking plane (Raft, MC-LAG sync, VXLAN control) and mgmt heartbeat | TLS 1.3 over the L2 stream; BFD authenticated | Negligible (tiny traffic). **Always on.** |
| Peer link data (MC-LAG) | Optional **MACsec** (GCM-AES-128/256) | 32 B/frame. With NIC offload (e.g. mlx5, some Intel) it runs at line rate. In software, AES-NI x86 gets roughly 5–20 Gbit/s per core; small ARM boards get much less. |
| VXLAN underlay data | Optional **WireGuard** (or MACsec per hop, or IPsec) | Software only: roughly 5–10 Gbit/s per core on modern x86, far less on ARM. Adds 60 B on top of VXLAN's 50 B, so the underlay needs MTU ≥ overlay + 110. |

**Recommendation:** control plane always encrypted, which costs nothing. Data-plane
encryption is a per-link toggle. Leave it off on physically trusted direct cables
(the peer link) and turn it on for VXLAN over untrusted networks. Encrypting *everything*
by default would cap throughput at software-crypto speed on most hardware.

## 7. Code layout

Target (requested 2026-10-03, STATUS.md): libraries under `lib/` as modules of their own (netdev, nft, hw,
svc for process/service supervision, and the protocol cores lacp, rstp, lldp, bfd, ospf, rib, policy); programs
`cmd/switchd`, `cmd/swcli`, `cmd/switchd-update`, `cmd/rtest` as separate modules; a go.work joins them. Each
program builds alone. The layout below is the original one.

```
cmd/switchd/        daemon
cmd/swcli/          CLI client / login shell
internal/schema/    config schema (types, validation, completion, help)
internal/config/    tree, candidate, diff, commit/rollback, set-format parser/printer
internal/cluster/   raft store, PKI/CA, join, member RPC (gRPC over mTLS)
internal/dataplane/ netlink reconciler: bridge, vlan, bond, vxlan, macsec, mtu, nft, vrf
internal/lacp/      LACP state machine (AF_PACKET)
internal/mclag/     peer protocol, FDB sync, split-horizon, failover
internal/vxlanctl/  VTEP/MAC distribution
internal/cli/       parser, modes, completion, pipes (used by swcli & tests)
internal/web/       REST, UI (embed), metrics, health
internal/syslog/    remote syslog
internal/access/    user sync, getty/serial management
test/netns/         integration tests using network namespaces + veth
packaging/          systemd units, arch PKGBUILD, deb
```

## 8. Implementation order

Guiding rules:
* Build bottom-up. Every phase leaves a working, testable system and ends with commits and tests.
* Everything that does not need a real network (config engine, CLI, protocol state machines)
  is built and unit-tested **locally first**. Anything that touches the kernel runs on the **Proxmox VMs**.
* Stacking comes **before** MC-LAG and VXLAN, because both rely on the member-to-member mTLS channel
  and on config that spans members.
* Protocol state machines (LACP, RSTP, MC-LAG sync) are pure Go logic behind small I/O
  interfaces, so they can be unit-tested with simulated links before they hit the VMs.

### Phase 0: Foundations (local)
1. Go module, directory layout (§7), Makefile, cross-compiling for amd64/arm64/armv7, linting, `make test`.
2. Daemon skeleton: structured logging, signal handling, systemd notify, `--dry-run` mode (no kernel changes).
3. RPC between swcli and switchd: gRPC over a unix socket. The same API is later reused over mTLS between members.

### Phase 1: Config engine (local, pure unit tests)
1. **Schema DSL** in Go: containers, keyed lists, leaves with types (int range, enum, string pattern,
   IP/CIDR, MAC, VLAN list/ranges, interface reference), defaults, help text. It is the single source for
   validation, CLI completion, `?` help, web forms and generated docs.
2. **Config tree**: `set` / `delete` / `edit` path handling, `[ a b ]` lists, `inactive:` / annotations later.
3. **Formats**: parse and print `set` format, curly-brace format and JSON. Round-trip tests.
4. **Commit engine**:
   * Candidate per session. `configure` (shared), `configure private`, `configure exclusive`, with locks.
   * `commit check`, with cross-reference validation: VLAN exists, interface exists on its member, MTU consistency,
     one interface in one bond only, etc.
   * `commit`, `commit and-quit`, `commit comment`, `commit confirmed N` (auto-rollback timer).
   * `rollback N` (50 revisions with user, time and comment), `show | compare [rollback N]`.
5. **Store interface**: a local file store (bbolt) first. The Raft implementation replaces it in Phase 5.
6. **Apply pipeline**: committed config → per-member desired state → subsystems with *validate → plan → apply*.
   The plan step produces the ordered, minimal op list of §4.14; `commit check` shows its impact summary.
   If apply fails on a member, it falls back to the previous config and reports the error per member.

### Phase 2: CLI (local)
1. Line editing and history, prompts `user@host>` (operational) and `user@host#` (configuration), `[edit vlans]` banner.
2. Schema-driven tab completion, `?` help, unique-prefix abbreviations, `edit` / `up` / `top` / `exit`.
3. Pipes: `| match`, `| except`, `| count`, `| no-more`, `| display set`, `| display json`, and a pager.
4. Operational command framework (a registry of `show` / `request` / `clear` / `monitor` commands).
5. Permission classes: `super-user`, `operator`, `read-only`.

**Checkpoint A:** switchd runs locally in dry-run mode. You can SSH into this box (or run swcli), configure the
full schema, commit, roll back and compare. Good moment for you to review the CLI feel.

### Phase 3: Local data plane (VM: sw1 [+ srv1 for traffic])
1. Netlink interface discovery and live link events. Capability detection (driver, speed, offload flags via ethtool).
   `show interfaces [terse|extensive]` with stats64 counters.
2. **Reconciler**: desired vs actual, idempotent, and it touches only objects it owns (tagged via ifalias/altname).
   Diff-driven and hitless as in §4.14: only changed objects are touched, no link down, tighten-before-loosen ordering.
   Kernel state survives a switchd restart, so there is no traffic loss when the daemon restarts.
3. Bridge `br0` with `vlan_filtering`, access/trunk/native VLAN, admin up/down, descriptions.
4. **Jumbo frames**: MTU per port (native), per VLAN (nftables bridge rules with counters), and commit-time MTU checks.
5. Static LAGs (bond without LACP, which comes in Phase 6), hash policy, min-links.
6. FDB: `show ethernet-switching table`, `clear …`, ageing, static MACs, per-port MAC limits, storm control (tc police), BPDU guard.
7. **Management**: mgmt VRF, static IP or DHCP, default route, DNS, NTP (via systemd-timesyncd/chrony config).
8. **Syslog** to remote hosts over UDP/TCP/TLS, a local ring buffer, `show log`.
9. Netns integration tests (run on the VM) plus real traffic tests with srv1.

### Phase 4: Access (VM: sw1)
1. `system login user … class … authentication (encrypted-password|ssh-ed25519 …)`, synced to local accounts
   with swcli as the shell. Stale managed users are removed.
2. OpenSSH: a managed drop-in config, running inside the mgmt VRF. Root/Linux shell only for an explicit `start shell`
   privilege.
3. **Serial**: auto-detect `ttyS*` / `ttyUSB*` / `ttyACM*`, configurable ports and baud rate, managed `serial-getty@` units.
   Tested via the Proxmox serial socket (`qm terminal`).
4. REST API: moved to Phase 18 (no web UI, decided 2026-10-04).

### Phase 4b: Junos parity basics (VMs: sw1; hardware: physw4) — requested 2026-09-29
In this order (the user's priorities; each step is spec first, then implementation and lab tests):
1. **Junos interface names `x/y/z`**: x = stack member, y = NIC card (PCI device, numbered in PCI address order),
   z = port on the card (PCI function / `dev_port`). Example physw4: 01:00.0–.3 → `1/0/0`–`1/0/3`,
   04:00.0–.1 → `1/1/0`–`1/1/1`, 07:00.0 → `1/2/0`. No `ge-`/`xe-` prefixes. Numbers are pinned in the state
   on first sight, so adding a card in a lower slot does not renumber existing ports (new cards get the next free y).
   Non-PCI NICs (USB) get cards after the PCI ones. Logical units are `1/0/0.5`. `show chassis hardware` shows
   name ↔ Linux name ↔ PCI address ↔ driver ↔ MAC.
2. **Units and L3 interfaces**: `interfaces irb unit <n> family inet|inet6 address …` with `vlans <v> l3-interface
   irb.<n>` (IP on a VLAN, routing between irbs in the default instance), routed ports (`unit 0 family inet` on a
   port that is not a switch port) and routed subinterfaces (`vlan-tagging; unit <n> { vlan-id <v>; family inet … }`).
   Junos rule kept: `family ethernet-switching` only on unit 0. `routing-options static route …`.
   irb addresses are stack-wide (anycast gateway on every member). The per-member management IP stays in
   `virtual-chassis member <id> management` (VRF mgmt), because each member needs its own address there.
3. **Operational quick wins**: `show system rollback <n>` (the complete configuration of revision n)
   and `show system rollback <n> compare <m>` (`show | compare` already works in
   configuration mode, and `show configuration | compare rollback <n>` in operational mode), `show arp` /
   `show ipv6 neighbors` (all instances, including addresses the OS manages), `show system uptime` (clock, boot
   time, switchd uptime, last commit), `system host-name` also written to /etc/hostname and /etc/hosts (no reboot),
   `system name-server` / `domain-name` → resolver configuration, `request system reboot|halt|power-off [at|in]`
   (single member now; with stacking and MC-LAG, a reboot first drains: LACP out-of-sync, peer takes over).
   **Card number lifecycle (follow-up, requested 2026-09-29)**: numbers are bound to the PCI slot address and never
   shift when a card is removed (implemented). Still to do:
   * `show chassis hardware` also lists known cards that are absent (their reserved numbers and last seen model).
   * A card in a known slot that changed model (other driver or port count) raises an alarm; its configuration stays
     on the same names, but ports that no longer exist are reported, and commit check names the change.
   * A card that moved to another slot (recognised by its MAC addresses) is reported with a hint instead of silently
     becoming a new card: `request chassis card <new> renumber <old>` moves it back to its old number.
   * `request chassis card <n> forget` releases the number of a card that was removed for good.
   * These commands change names, so they show which configured interfaces are affected and need confirmation.
4. **NIC capability checks at commit**: per port capabilities from ethtool (link modes/speeds, pause, VLAN and tc
   offload, max MTU) in the inventory; `commit check` reports settings a NIC cannot do. `show system offload`
   (hardware acceleration per port, §4.11).

### Phase 5: Stacking (Junos Virtual Chassis syntax: `virtual-chassis member …`, `request virtual-chassis vc-port …`, `show virtual-chassis`) (VMs: sw1, sw2, sw3 with stacking NICs in a ring)
1. **Stack keys (minimal PKI, no expiry)**: the first member creates the stack key pair (Ed25519). Each member has
   its own key pair; joining means the stack key signs "member <id> has public key K". Certificates exist only
   because TLS needs them: validity 2000-01-01 to 9999-12-31 (RFC 5280's "no expiry" value), so nothing ever
   expires, there is no renewal, and a wrong clock (ARM boards without a battery-backed clock) never breaks the
   stack. No CRLs: a removed member's key is simply no longer in the replicated member list, which every
   member checks after the TLS handshake. Join tokens are one-time and expire (they are not certificates).
2. **Stacking transport**: stacking-port designation (local state), AF_PACKET sockets bound *only* to stacking ports,
   a reliable L2 stream as `net.Conn` (fuzzed and loss-tested in-process with simulated lossy links), TLS 1.3 mTLS on
   top, and IP-less BFD per link.
3. **Stack topology**: adjacency discovery, link-state flooding, hop-by-hop relay with shortest live paths. Tests for
   chain/ring/mesh and link loss, all in-process with simulated links.
4. **Plane separation test**: stacking-EtherType frames injected on data ports are forwarded untouched and never
   reach the stack code.
5. **Raft store** (over the stacking transport) replaces the local store. Config locks work across the whole stack. Voter management is automatic
   (up to 7 voters, the rest are non-voters). Witness mode (a member without a data plane).
5b. **Mastership migration and decommissioning** (requested 2026-09-29): Raft leadership transfer via
   `request chassis routing-engine master switch [member <id>]`; `request virtual-chassis member remove <id>`
   (move mastership if needed, drain, leave quorum, promote a replacement voter). No member is special (stack key on
   all members). Lab test: remove the master of a 3-member ring under traffic; no config loss, forwarding continues.
6. Per-member apply with results reported back: `commit` prints the result per member (like Junos VC).
7. Stack-wide operational commands: `show interfaces` / `show virtual-chassis` for all members, `request … member N`.
   CLI and web work from any member.
8. Failure tests: leader killed, stacking link cut (ring re-route), partition, member rejoin, and a check that the
   data plane keeps forwarding without quorum.

Status 2026-09-30: steps 1–8 done and lab-tested (test/lab: TestVirtualChassis, …RingCut, …MasterKilled,
…Partition, TestStackingFramesOnDataPorts), witness role (data plane off, never master; no lab member for it yet).
Open: the "drain" part of member removal (LACP partners) comes with Phase 6; a configuration session relayed to the
master ends when the stacking path to the master changes (end-to-end retransmission in the mesh would keep it).

### Phase 6: LACP (VMs: sw1, srv1)
1. 802.1AX LACP state machines (receive, periodic, selection, mux) in pure Go, unit-tested with simulated partners.
2. I/O: AF_PACKET + BPF per member port. The bond runs in non-LACP mode, and switchd adds/removes members
   according to the LACP state. Options: active/passive, fast/slow rate, system priority, port priority, min-links.
3. `show lacp interfaces`, `show lacp statistics`.
4. Interop test against a normal Linux 802.3ad bond on srv1.

### Phase 7: MC-LAG (VMs: sw1, sw2, srv1)
1. Peer session over the **stacking plane** (from Phase 5). **Micro-BFD** on the peer-link ports (IP-less, consumed
   only on peer-link ports). **BFD heartbeat** over mgmt (UDP, RFC 5881/5883, authenticated). Primary/secondary role
   election (priority, then member ID).
2. Shared LACP system ID and disjoint port-number ranges, so srv1 sees one partner.
3. Consistency checks (VLANs, MTU, LACP parameters, **port speed class**: both legs of an MC-LAG bundle must
   be able to run at the same speed, e.g. not 1G copper on one chassis and 10G SFP+ on the other). Checked at
   commit where possible (both members' inventories are known through the stack) and at runtime. On a mismatch
   the bond is set to proto-down with a reason. Config syntax follows Junos `multi-chassis` / `mc-ae` where it fits.
4. **MAC sync**: learned MACs, moves, coordinated ageing and flush on link down.
5. **Split horizon** via nftables, updated dynamically with each leg's state.
6. Failure handling (matrix over stacking path, peer-link and heartbeat): leg down, peer-link down with the peer alive (the secondary shuts its ports), peer dead,
   switchd crash, and reboot/rejoin (delay-restore timer).
7. `show mclag`, `show mclag consistency`, alarms.
8. Failure-matrix tests with measured convergence (high-rate ping and iperf3 from srv1).

### Phase 7b: Rolling upgrades without impact (requested 2026-09-30)
Members of a stack and of an MC-LAG domain must keep working together while they run different software versions,
so a pair can be updated one switch at a time: maintenance mode (drain: LACP out of sync, mastership moved,
MAC sync complete), update, rejoin (delay-restore), then the other switch.
* **Compatibility window**: a version works with its predecessor and successor release line, including across a major
  step: 1.9 (the last 1.x) ↔ 2.0 must work; larger jumps (e.g. 1.4 ↔ 2.5) are not supported and are refused with a
  clear message ("update to 1.9 first") instead of misbehaving.
* **Protocol versions**: every stack/MC-LAG message family (stacking link, mesh, Raft entries, control RPC, leg state,
  MAC sync, micro-BFD, LACP state files) carries a version; each side announces the range it speaks in the session
  hello, and both use the highest common one. Unknown fields are ignored, removed fields keep being sent for one
  release line. Replicated configuration: the config schema is versioned; the master commits only what every member
  can apply (new statements are refused while an older member is present), and upgrades of stored revisions
  (internal/daemon/upgrade.go) stay reversible within the window.
* `request system maintenance-mode enter|exit` (drain before an update; exit rejoins), `show version all-members`
  shows mixed versions, and a commit check warns while versions differ.
* Tests: a compatibility matrix in CI (N-1 ↔ N for every message family, recorded fixtures of older versions), and a
  lab rolling-upgrade test under traffic (0 loss) from the previous release to the current one.

### Phase 7c: Stack tunnels (requested 2026-09-30; config reference 5.2, 5.6, stack-protocol "Stack tunnels")
The stacking ring carries client traffic between members and replaces the MC-LAG peer-link.
1. Underlay: VRF `swstack`, member addresses, permanent neighbours from the stacking links, ECMP routes from the
   mesh topology; stacking ports at their NIC maximum MTU.
2. Tunnels `swvc<m>` per other switch member in `swbr0` (isolated, tagged VLANs both ends have, DF set).
3. MC-LAG on tunnels: peer-link, peer-link-bfd and heartbeat removed (E); split horizon on the peer's tunnel; DF
   rule for third members; flush of third members' addresses on leg failure; two-member split forwards at all
   costs, minority rule for 3+.
4. Stack MTU check (E) and `show virtual-chassis mtu`; VLAN 4094 reserved.
5. Lab: jumbo (host MTU 9000, DF) between hosts on different members, access VLAN, QinQ (customer tag), host VXLAN;
   cut one ring cable under traffic; MC-LAG tests on the ring (sw1/sw2 pair, sw3 single-homed host).
6. Later: the internal management VLAN 4094 (members without their own management cable).

### Phase 8: RSTP (VMs: sw1, sw2, sw3 in a loop, + srv2)
1. 802.1w state machines in pure Go (port roles, proposal/agreement, edge/p2p, TC → FDB flush), unit-tested.
2. I/O: bridge in user-mode STP, BPDUs via AF_PACKET, port states via netlink.
3. Edge ports, BPDU guard, root guard, cost and priority, and 802.1D compatibility.
4. **MC-LAG integration**: shared bridge ID. The primary computes MC-LAG port states, and BPDUs arriving on the secondary's
   leg are relayed over the peer link. The peer link is never blocked. When the peer fails, the survivor keeps the bridge ID.
5. Interop tests against mstpd on a srv VM, plus loop tests (sw1–sw3–sw2 triangle).

### Phase 8b: Multicast (IGMP/MLD snooping)
1. `protocols igmp-snooping vlan <v|all> { querier; immediate-leave; version 2|3; }`, MLD likewise. Default:
   snooping **on** for all VLANs (unknown multicast then only goes to router ports and interested receivers, the
   usual switch default), querier off. Kernel bridge snooping per VLAN, querier where no router is present.
2. `show igmp snooping membership`, MC-LAG sync of group state.

### Phase 9: VXLAN (VMs: sw1, sw2, sw3, srv2)
1. One vxlan device per member in vnifilter mode, VLAN↔VNI mapping, underlay source interface, MTU checks.
2. Control plane over the stack channel: VTEP and MAC advertisements per VNI, head-end replication (flood) lists,
   remote MAC installation, MAC moves.
3. Anycast VTEP for MC-LAG pairs.
4. Static external VTEPs and a flood-and-learn mode for non-stack peers.
5. Loop safety: VTEP-to-VTEP forwarding is never allowed (split horizon), so the mesh is loop-free by construction,
   and VXLAN ports are excluded from RSTP.
6. `show vxlan`, `show vxlan remote-vteps`, `show ethernet-switching table vni …`.

### Phase 9a: One program per protocol (requested 2026-10-03; config reference 1.9)

Decisions (user, 2026-10-03): every protocol and service in its own `cer-` daemon, switchd starts and watches them
and tells every CLI session when one fails; stacking stays in switchd; mclagd does MAC synchronisation, leg states
and failover only; no mirrord/igmpd (nothing runs at run time); the journal stays in memory on the image.

Design:
* **Supervision through systemd.** switchd writes `cer-<name>.service` units (program, arguments, Nice, real-time
  scheduling, OOMScoreAdjust, capabilities, watchdog, Restart=always) and starts/stops them; systemd executes them.
  Reason: the daemons must outlive a restart or crash of switchd (hitless), which direct child processes of switchd
  cannot (they die with switchd's cgroup or have to be adopted). switchd polls their state every second
  (`systemctl show`), notifies the CLI sessions of the whole stack (local `rpc.Server.Notify`, other members over
  the stacking protocol to the master) and restarts what systemd gave up. Backend behind an interface (a fake in
  tests).
* **IPC (`pkg/ipc`).** Unix stream sockets in `/run/ceros/<program>.sock` (root only, peer credentials checked),
  length-prefixed JSON frames, a versioned hello (protocol version, program, version). Every endpoint can serve
  calls and **state topics**: a subscriber gets the full state, a sync mark, then changes; after a reconnect it gets
  the full state again and drops keys it did not see. Clients reconnect by themselves. Calls in both directions on
  one connection (switchd asks a daemon for its status; the daemon asks switchd to relay to another member).
* **switchd's service socket** (`/run/ceros/switchd.sock`): topics `config` (key: daemon; the daemon's own config,
  computed by switchd as today, e.g. lldp ports, LACP bundle specs), `role` (member id, master, reachable members,
  stack id); calls `stack.call` (relay a call to the same daemon on another member over the stacking protocol),
  `notify` (to the CLI sessions), `lease` and other inputs to the data plane. A daemon lists the stacking-protocol
  methods it serves in its hello; switchd registers them with the stack node and forwards (old message names such as
  `mclag-legs` keep working across a rolling update).
* **Direct sockets between daemons** for local paths: cer-lacpd -> cer-mclagd (before join/leave), cer-lacpd topics
  (legs, enabled ports) for cer-mclagd, cer-rstpd, cer-lldpd; routing protocols -> cer-ribd; cer-bfdd -> protocols.
* **Logging (`pkg/journal`):** slog handler writing the journal's native protocol (MESSAGE, PRIORITY,
  SYSLOG_IDENTIFIER, SYSLOG_FACILITY, CEROS_FACILITY, CEROS_MEMBER); stderr when there is no journal.
  cer-syslogd follows the journal (`journalctl -f -o json`, cursor in /run), keeps the `show log` buffer, relays to
  the master's cer-syslogd on members, forwards on the master.
* **Daemon kit (`internal/daemonkit`):** flags, journal logging, signals, sd_notify ready/watchdog, connection to
  switchd, the config and role subscriptions, panic -> exit (systemd restarts).
* **Layout:** module path `github.com/thxrben/cerium-switchd`; reusable libraries under `pkg/` (no imports from
  `internal/`, checked by a test); programs under `cmd/` (switchd, swcli, switchd-update, cer-*, rtest), each
  built on its own (`make switchd`, `make cer-lldpd`, ...). Separate Go modules/repositories later are a mechanical
  step once the layering holds.

Stages (each one commit, all tests green, the system works after every stage; all done 2026-10-03, see STATUS.md):
1. Spec (1.9, os-image), plan. 2. Module path, `pkg/`, separate swcli and switchd-update binaries, Makefile, image
and lab install. 3. `pkg/ipc`, `pkg/journal`, `pkg/sdnotify`, daemonkit. 4. Supervisor, service socket,
`show system processes`, `restart`. 5. cer-lldpd. 6. cer-syslogd (+ journal on tmpfs). 7. cer-ntpd. 8. cer-dhcpcd
(leases -> switchd, which adds the addresses). 9. cer-lacpd. 10. cer-mclagd. 11. cer-rstpd. 12. cer-ribd (routes
leave the data plane). 13. cer-bfdd. Then OSPF continues as cer-ospfd, BGP as cer-bgpd.

### Phase 9c: OSPF and OSPFv3 (requested 2026-10-03; config reference 5.13, 5.8, 5.12)

One protocol core for both versions (`pkg/ospf`), pure and driven by packets, a clock and timers:
* **Types shared by both versions**: router ids and LS ids as 32-bit values; LS types as 16-bit codes (v2 types 1–5
  as they are, v3 function codes 0x2001… with their flooding scope: link, area, AS); one LSA header layout (the
  v2 options+type bytes are the v3 type field). The v2 DR/BDR are interface addresses, the v3 ones router ids:
  both are 32-bit values, so DR election is one function.
* **Codecs**: OSPFv2 packets (24-byte header, authentication null/simple/MD5) and LSAs 1–5; OSPFv3 packets (16-byte
  header, instance id, the checksum computed by the kernel through IPV6_CHECKSUM) and LSAs Router, Network,
  Inter-Area-Prefix, Inter-Area-Router, AS-External, Link, Intra-Area-Prefix; unknown LSAs flooded by their scope.
* **State machines**: interface (Down, Waiting, PtToPt, DROther, BDR, DR, Passive), neighbour (Down … Full),
  DR election, database exchange, requests, flooding with retransmission and delayed acknowledgments, aging,
  refresh, MaxAge removal, MinLSInterval/MinLSArrival.
* **Origination**: v2 router (p2p, transit, stub links), network, summary, ASBR summary, external; v3 router (no
  prefixes), network, link (link-local address and prefixes per interface), intra-area prefix (for the router and
  for each transit network), inter-area prefix and router, AS-external. Overload (max metric, RFC 6987).
* **SPF**: Dijkstra over router and network vertices with **all equal-cost paths** (up to 16 next hops); next hops
  from the neighbour's address (v2) or its link-local address from its Link LSA (v3); then stub/intra-area
  prefixes, inter-area routes (backbone summaries on an ABR), externals (type 1 before type 2, forwarding address).
* **Graceful restart** (RFC 3623 / 5187): helper mode, and restarting after a mastership change or switchd/cer-ospfd
  restart (grace LSAs, kept routes in cer-ribd).

Program **cer-ospfd** (one process, OSPF and OSPFv3 of every routing instance; runs the protocol on the master
only, idle on the other members): switchd computes its configuration (kernel devices, addresses, link-local
addresses, interface ids, costs from the reference bandwidth, router id); Linux I/O with raw IP sockets (protocol 89,
IPv4 224.0.0.5/6 with TTL 1; IPv6 ff02::5/6 with hop limit 1 and IPV6_CHECKSUM 12), bound to each interface and its
VRF; routes go to cer-ribd (`routes.set`, ECMP); BFD sessions through cer-bfdd; `show ospf|ospf3 …` and
`clear ospf|ospf3 neighbor` through calls to it. Unicast protocol packets that arrive on other members reach the
master unchanged (MC-LAG for protocols, below).

Steps: (1) shared types and both codecs with tests (encode/decode round trips, checksums, captured packets);
(2) LSDB with scopes; (3) interface/neighbour state machines, exchange and flooding, tested with simulated
broadcast and p2p networks of several routers; (4) origination; (5) SPF with ECMP, areas, externals (topology
tests); (6) cer-ospfd with Linux I/O, configuration from switchd, routes to cer-ribd, show/clear commands, smoke
test; (7) graceful restart, overload, BFD; (8) lab: interop with FRR on srv1 (v2 and v3, broadcast and p2p).
**Graceful restart, plan 2026-10-05** (RFC 3623, OSPFv3 RFC 5187; reference 5.13 `graceful-restart`):
1. **Helper** (pkg/ospf): a grace LSA (v2: opaque link-local type 9, opaque type 3; v3: type 0x000b) from a Full
   neighbour on that link (broadcast: matched by its IP-interface-address TLV, else the advertising router) with a
   grace period not yet over, and no topology change pending for it (no changed LSA of types 1-5/7 on its
   retransmission list) starts helping: the neighbour stays Full and advertised although its hellos stop, until the
   period ends (then it goes down as dead), the grace LSA is flushed (the restart succeeded), or the topology changes
   (strict LSA checking as RFC 3623 §3.2: a changed LSA of types 1-5/7 would be flooded to it: helping ends, its
   inactivity timer runs normally). `show ospf neighbor` shows `helper`; the log names start and end with the reason.
2. **Restarting** after a restart of cer-ospfd on the master (crash, `request daemon restart`, switchd's planned
   stops): cer-ospfd keeps its neighbours in `/run/ceros/ospf-restart.json` (instance, interfaces, neighbour router
   ids; written on changes; tmpfs: gone after a reboot). A planned stop first floods grace LSAs (reason software
   restart, period = restart-duration) and waits up to 1 s for acknowledgements; it flushes nothing and sends no
   1-way hellos. At start, with a recent file: grace LSAs before the first hello (unplanned: reason unknown), then
   restarting mode: no own router/network/summary/external LSAs originated or changed (received self-originated
   ones are kept as they are), no routes reported to cer-ribd (which keeps the old ones: no drop); it ends when every
   neighbour of the file is Full again, when the period is over, or when a received router LSA of a neighbour no
   longer lists this router (inconsistent): then normal origination, SPF, routes reported, grace LSAs flushed.
   cer-ribd's grace for OSPF becomes the configured restart-duration (not the fixed 180 s) when it is longer.
3. **Mastership change** (later step): the master replicates the restart file's content to the members (small, on
   changes); a new master's cer-ospfd starts in restarting mode with it.
4. Tests: the simulated network (sim_test.go) with a restarting router for v2 and v3: no route lost at the helper,
   helper exit on period, flush, topology change; restarting mode exit conditions; the file across a daemon restart.
Status 2026-10-05: 1, 2 and 4 done (pkg/ospf grace.go/restart.go, internal/ospfd/restart.go; not on a device; 3 is
open). Found on the way: OSPFv2 dropped opaque LSAs and ended an exchange whose DD listed one (now type 9 is kept as
Raw, the O bit is set); a helper keeps the restarting neighbour's DR election values until helping ends (then
re-elects at once), and a restarting router takes the DR/BDR roles its neighbours' hellos give it and leaves Waiting
at once (in the simulation the restart takes ~5 s instead of the 40 s wait timer). A planned restart is only a
restart the supervisor makes (`request daemon restart`, a new unit): /run/ceros/planned-restart/<program>; a reboot,
halt or `request system reload` (ports go down) never announces a grace period.

### Phase 9b: BGP (EVPN): own core, GoBGP's packet codec (decided 2026-10-04)
BGP is our own core (`pkg/bgp`, like `pkg/ospf`) in the program **cer-bgpd**; only GoBGP's message codec
(`github.com/osrg/gobgp/v3/pkg/packet/bgp`: standard library only, no server, no gRPC) is used. The GoBGP server
was considered and rejected (2026-10-04): its own RIB and policy engine cannot express Junos policy semantics
(`next policy`, advertising only active routes, `from protocol` export, per-group multipath, hidden routes),
policy changes are global and many peer changes reset sessions (against hitless reconfiguration), and it brings
gRPC/protobuf and a large memory footprint. FRR was rejected earlier as less predictable to drive.
* Core: session FSM and timers (RFC 4271), capabilities (4-byte AS, multiprotocol IPv4/IPv6 unicast, route refresh,
  graceful restart), Adj-RIB-In/Out, best path (RFC 4271 plus the Junos tie-breakers), multipath per group, route
  reflection (RFC 4456), graceful restart (RFC 4724, helper and restarting), remove-private, local-as.
* Policy through our engine (internal/policy) on import and export, exactly as Junos; routes to cer-ribd (replicated
  to the members like OSPF's), exports from the RIB; TCP MD5 and TTL on the sockets.
* On the master like OSPF: irb/MC-LAG sessions reach it as frames; routed ports of other members are relayed as a TCP
  stream; BFD through cer-bfdd (placement as for OSPF).
* Steps: (1) core with in-process tests (two and more speakers over pipes); (2) cer-bgpd: Linux sockets, VRFs,
  config from switchd, routes to cer-ribd, exports; (3) show/clear commands, receive/advertising-protocol, hidden;
  (4) hitless changes and commit warnings; (5) BFD client; (6) relay for routed ports of other members;
  (7) tests in network namespaces, interop with GoBGP/FRR (lab: FRR on srv1).
Use: simple BGP routing for irbs and routed ports, later the EVPN control plane for VXLAN towards non-stack VTEPs.

**`show route` in full (requested 2026-09-30)**, Junos layout, built on the basic `show route` of Phase 4b:
* IPv4 and IPv6 (`inet.0` / `inet6.0`, per routing instance `<name>.inet.0`), every source with its protocol and
  preference: Direct, Local, Static, BGP (and later others), active route marked, next hops and interfaces, age.
* Filters: `show route <prefix>` (longest match / `exact`), `protocol <p>`, `table <t>`, `instance <name>`,
  `terse`, `detail`/`extensive` (BGP attributes: AS path, local preference, MED, communities, originator),
  `summary` (counts per table and protocol), `advertising-protocol bgp <peer>` / `receive-protocol bgp <peer>`.
* BGP routes come from cer-bgpd's Adj-RIB-In (also those not installed, e.g. inactive or rejected by policy), installed ones are
  cross-checked with the kernel. Member targets (`member <id>` / `all-members`) as for the other show commands.

### Phase 10: MACsec (decided 2026-10-04; no WireGuard)
WireGuard is dropped: it would only serve remote L3 sites and road warriors, and costs too much per packet.
1. **Every port** can run MACsec (switch ports, `ae` legs, stacking ports). Linux `macsec` devices on top of the
   port; hardware offload (`offload mac|phy`) where the NIC has it, else software (AES-NI).
2. **Stacking links: on by default** (`virtual-chassis macsec disable` turns it off). The master generates the keys
   (SAK, GCM-AES-XPN-256: 64-bit packet numbers, so no exhaustion between rotations) and hands them to a member in
   the join reply over the mTLS control channel; renewed every hour (and on a member leaving) over the same channel.
   Static SAs, no MKA between members (the mTLS channel is the key agreement). A link switches over to MACsec right
   after the join: the ring carries the traffic around the one link that changes. Both ends install the new receive SA
   before either sends with it (rotation without loss).
3. **Client ports: pre-shared keys in Junos syntax** (`security macsec connectivity-association <ca> { security-mode
   static-cak; pre-shared-key { ckn …; cak …; } cipher-suite …; }`, `security macsec interfaces <if>
   connectivity-association <ca>`), negotiated with the standard MKA protocol (802.1X-2010) through wpa_supplicant
   (`macsec_linux` driver), one instance per port, supervised by switchd.
4. **MTU**: the stack MTU budget grows by 32 bytes (SecTAG with SCI 16 + ICV 16) on MACsec stacking links; client
   ports' MACsec device carries the configured mtu, the port gets 32 more (commit checks the hardware maximum).
5. `show security macsec connections|statistics`, alarms when a secured link stops passing traffic.
   Status 2026-10-04: stacking links done (unit-tested); client ports done 2026-10-04 (unit-tested): cer-mka@<port>
   units, the data plane moves a secured port's role to the MACsec device wpa_supplicant creates (dataplane.SecPort,
   DataNames). Open: NIC offload for client ports (wpa_supplicant macsec_offload), bundle members, lab test.
   wpa_supplicant's MKA offers GCM-AES-128/256 only (no XPN) and a fixed 2 s hello.
6. Benchmarks (software vs offload) documented.
7. **Plan 2026-10-05: offload on client ports.** wpa_supplicant creates the MACsec device, so the offload must be in
   its configuration (`macsec_offload=2`: MAC, the `macsec-hw-offload` NIC feature; 1 = PHY has no feature flag and is
   not used). The kernel cannot change the offload of a device with SAs, so switchd cannot set it afterwards.
   * Used when: the NIC reports `macsec-hw-offload` (ethtool features through the existing hwio/ethtool reader), the
     port has no `offload disable` (5.3.2), and the installed wpa_supplicant knows the option. 2.10 (Debian 13) may
     not: an unknown option makes it refuse the whole configuration (as `macsec_csindex` did in the lab), which on a
     must-secure port means no traffic. Known = the option's name is among the program's configuration keywords
     (the binary's strings; checked once per start of switchd, the program is read-only on the image).
   * Fallback: when the offloaded MKA does not secure the link within 30 s, or its unit fails twice, the port runs
     MKA again without offload (software) and a Minor alarm says so; retried with offload after 10 min (as the
     stacking links, 10b).
   * `show security macsec connections`: `Encryption: hardware (mac)` from the kernel (netlink, done 2026-10-04).
   * Tests: configuration text per case, the fallback timing with a fake supervisor; the lab has no offloading NIC
     (virtio): only the software path and the "not offered" decision are lab-testable.
8. **Plan 2026-10-05: bundle members.** A member port of an `ae` may be secured; its MACsec device takes the port's
   place in the team (LACP bundles) or the static bundle. cer-lacpd keeps sending LACPDUs on the physical port
   (unprotected, as on Junos), so its partner relation does not depend on MKA; but a member is only *enabled* in the
   team (collecting/distributing) once its MACsec device exists. Until MKA has secured it, LACP reports the port as
   not in sync (Actor sync bit cleared), so the partner does not send traffic over it either. MC-LAG legs work the
   same (each leg's ports are local to its member). E: a bundle whose ports mix secured and unsecured ports
   (traffic would leave both protected and unprotected). Steps: model check (all or none of a bundle's ports),
   dataplane (team port = the device, mtu + 32 on the physical port), cer-lacpd (the "secured" state per port from
   switchd, held out of sync before), show lacp (a `MACsec: negotiating` note), tests (dataplane plan, LACP machine
   held out of sync, a two-member MC-LAG with secured legs in test/daemons).
   Status 2026-10-05: 7 and 8 done, unit-tested (not on a device; the test/daemons MC-LAG case with secured legs is
   not written: it needs MKA, which test/daemons cannot run).

### Phase 11: Polish and packaging
1. ~~Full web UI~~: dropped 2026-10-04; the REST API (Phase 18) serves an external orchestrator instead.
2. `show system alarms`, config archival.
3. **Software update**: `request system software add <usb:|http(s):|ftp:|file>` with signed image bundles
   (docs/os-image.md); a signature is always required. Rolling upgrade across the stack (one member at a time,
   drained first), each member reboots into its backup slot and returns by itself when the new version fails.
4. **USB storage** (Phase 17): `save usb:<file>` / `load … usb:<file>`, `file list usb:`,
   `request system storage usb eject`; the stick is mounted only while in use, synced and unmounted right after
   (less wear, safe to pull).
5. **chassisd** (environment): temperatures, fans (speed control with a curve), PSUs from hwmon/IPMI/PMBus;
   `show chassis environment`, alarms and syslog on thresholds.
6. **SFP diagnostics**: `show interfaces diagnostics optics <if>` via the ethtool module EEPROM (SFF-8472
   DOM: temperature, voltage, bias, TX/RX power with thresholds). Works on most 10G SFP+ NICs.
7. Docs: user guide and a CLI reference generated from the schema. (The `.deb` packages planned here are replaced by
   the firmware image, docs/os-image.md.)

### Phase 12: Port authentication (802.1X)
Authenticator on switch ports (hostapd wired driver, per port, EAP → RADIUS or local users; MAB fallback;
dynamic VLAN) and supplicant (wpa_supplicant, for uplinks into a secured network). Off by default: ports need
no authentication unless configured.

**Plan 2026-10-05 (for review; nothing implemented yet)**:
1. **Syntax (Junos)**: `protocols dot1x authenticator { authentication-profile-name <p>; interface <if> {
   supplicant single|single-secure|multiple; mac-radius [restrict]; reauthentication <s>; guest-vlan <vlan>;
   server-fail deny|permit|use-cache|vlan-name <vlan>; } }`, `access radius-server <ip> { secret <s>; port <n>;
   source-address <ip>; }`, `access profile <p> { authentication-order [radius]; radius { authentication-server
   [<ip>…]; } }`. RADIUS leaves through the management instance (1.8) of the master or the member that has the port.
2. **Data plane**: the Linux bridge's port flags `locked` and `mab` (kernel 6.2+): a locked port forwards only from
   source MACs with an FDB entry; switchd adds the entry (VLAN, port) when the client authenticated and removes it
   on logoff, reauthentication failure or timeout. `single`: the first MAC; `multiple`: each MAC on its own (the MAB
   flag reports unknown MACs). Dynamic VLAN from RADIUS (Tunnel-Private-Group-ID): `single` changes the port's
   PVID for the session; `multiple` with different VLANs per MAC is not possible in the Linux bridge (E for the
   combination, or the first MAC's VLAN for all: decision for you).
3. **EAP**: hostapd per port (wired driver, `ieee8021x=1`, RADIUS client), supervised like cer-mka (unit
   `cer-dot1x@<port>`); its control socket tells switchd about authorized/deauthorized stations. MAB: switchd sends
   the RADIUS request itself (pkg/radius: Access-Request with the MAC as user name and password, Message-
   Authenticator), since hostapd's MAB support is limited.
4. **Stack/MC-LAG**: the state is per member port; an MC-LAG bundle authenticates each leg's ports on their member
   (both legs see the same client MAC: the second leg's authentication is the first one's, shared over the stack).
5. **Supplicant** (an uplink into a secured network): `protocols dot1x supplicant interface <if> { … }` with
   wpa_supplicant wired (cerOS extension).
6. **Show**: `show dot1x interface [detail]`, `clear dot1x interface <if>`, `show dot1x authentication-failed-users`.
7. **Tests**: hostapd and wpa_supplicant 2.12 are on the dev box: EAP-MD5/PEAP between them in network namespaces
   with hostapd's integrated RADIUS server; the bridge `locked`/`mab` flags in a user namespace (the kernel here
   has them); pkg/radius against RFC 2865 test vectors.
Questions: RADIUS only, or local users too? Dynamic VLAN in `multiple` mode (E, or the first VLAN)?

**Decisions of the user (2026-10-05)**: RADIUS and local users (`system login user`, never root) can authenticate;
on any port only **one** client authenticates (`supplicant single`), every other frame on the port then belongs to
the same VLAN(s) as configured (also a trunk); `multiple` is not offered. Dynamic VLAN therefore changes the port's
untagged VLAN (PVID) for the session only.

### Decisions of 2026-10-05 for item 13 and after
* Order: the **modular code base first**, before anything else: every program its own application, shared libraries
  wherever code is duplicated.
* Secure Boot: **own keys** enrolled in the firmware's db (plan 3 (a) below). arm64: later.
* cer-ribd (PLAN 15b.4): diff the kernel against cer-ribd and push only changes; a full reinsertion when something
  is wrong (unknown or stale routes in the kernel, a diff larger than a threshold, a failed incremental push).
* Tool output: every remaining place that parses non-JSON tool output moves to the kernel API.
* Routing protocols drain in maintenance mode (and so before a software update or reboot of a member that runs or
  relays them): OSPF max-metric (RFC 6987) and wait until the neighbours' paths moved, BGP graceful shutdown (RFC
  8326 community, then withdraw), before mastership moves; OSPF/BGP graceful restart across the mastership change.

### Modular code base: plan of 2026-10-05 (started the same day)
Facts (go list): every program already is its own binary (cmd/*), but all share one module; switchd imports the
daemons' implementation packages only for their API (configuration, requests, statuses, method names); every
daemon imports pkg/lacp through internal/svc; cer-bgpd/cer-ospfd import the configuration model through policy.
1. **API packages**: `internal/api/{bgpd,ospfd,ribd,bfdd,mclag,stp}` hold what switchd and other daemons use of a
   daemon (types and names); the daemon packages refer to them (aliases where the daemon uses the names a lot).
2. **Library modules** `lib/<name>` (each its own go.mod): `sys` (hwio, sysexec, nlx, netdev, sdnotify, journal),
   the protocol cores one module each (lacp, rstp, lldp, bfd, ospf, bgp, rib, dhcp, ntp, syslog, macsec), `conf`
   (schema, config, model, policy, memslots: the configuration model of cerOS), `platform` (ipc, svc, daemonkit,
   alarms, version, names, api/*), `software` (software, usbstore: switchd and the update daemon).
3. **Program modules** `apps/<program>` (switchd, swcli, switchd-update, cer-*, rtest), each with go.mod, main and
   its own internal/ (switchd: access, cli, commit, daemon, dataplane, diag, inventory, osconf, rpcserver, stack,
   supervise, webapi, …; cer-bgpd: bgpd; …). `go.work` at the root; every go.mod has relative `replace` lines, so a
   module also builds alone (GOWORK=off) and `go mod tidy` works.
4. **Duplicates** removed on the way: the cer-ribd client in cer-bgpd and cer-ospfd (-> platform/api/ribd), the
   sorted-keys helpers (slices.Sorted(maps.Keys)), and what a scan finds.
5. Makefile targets per program, CI per module, image/ and lab/ paths, README module map. No behaviour change: the
   whole test suite green after every step.

### Item 13 of 2026-10-05: further plans
1. **Modular code base** (requested 2026-10-03; steps there): one mechanical change with no behaviour change. Layout:
   `lib/{netdev,nlx,hwio,ipc,lacp,rstp,lldp,ospf,bgp,rib,bfd,macsec,dhcp,ntp,syslog,journal,sdnotify,sysexec}`
   (each its own go.mod), `cmd/<program>` each its own module, `internal/` stays switchd's (go.work ties them).
   swcli becomes its own program without netlink (it needs internal/rpc and internal/swcli only). Risk: import
   paths change everywhere (sed + gofmt), CI builds each module alone. Best done between two features, with the
   whole test suite green before and after. Question: now, or after the lab tests of what is pending?
2. **arm64**: the image for arm64 UEFI (Debian's arm64 kernel, grub-efi-arm64 or systemd-boot, the same partition
   layout), bundles per platform (the manifest has the platform already; an update of a mixed stack carries one bundle
   per platform: `request system software add` takes several sources, the master gives each member its own),
   QEMU tests with qemu-system-aarch64 and AAVMF (as test/image does for amd64). Question: which arm64 hardware
   (it decides drivers and the console)?
3. **Secure Boot**: a unified kernel image (UKI: kernel, initramfs and the command line with the slot's dm-verity
   root hash) per slot, signed; the firmware verifies it, it verifies the root file system (verity), so nothing
   unsigned runs. Two ways, a decision for you: (a) cerOS's own keys enrolled in the firmware's db by the operator
   (simple, most secure, a step per switch), or (b) the Microsoft-signed shim with cerOS's key as MOK (works with
   default firmware settings, one confirmation at the console per switch). With UKIs, systemd-boot's boot counting
   (`+3-0` names) can replace GRUB's grubenv counting; the A/B logic stays as in docs/os-image.md. The signing key
   is the release key (CEROS_SIGNING_KEY), kept off the build machine (sign step separate).
4. **Delta updates**: a delta bundle carries only the 128 KiB blocks of the new image that differ from a named base
   version (the base's image SHA-256 in the manifest), signed like a full bundle. The member reads the base blocks
   from its active slot (verified by dm-verity on the way), writes the new image into the backup slot block by block
   and checks the result's root hash against the manifest before it switches; a member whose active slot is not
   the base gets the full bundle. Needs reproducible images (mksquashfs `-reproducible`, sorted files, fixed times),
   so unchanged files stay in unchanged blocks; measured on two consecutive builds before deciding block size. RAM:
   one block plus the delta (in the update slot, 17.1).

### Phase 13: System diagnostics (`request system diagnose` / `show system bottlenecks`)
An overall check that lists what limits the switch, with a recommendation per finding:
* **PCIe**: per NIC the negotiated link speed/width vs. the card's maximum and vs. what its ports need at line rate
  (`current_link_speed/width` vs. `max_link_*` in sysfs; e.g. a 4×10G card in a PCIe 2.0 x4 slot).
* CPU: cores vs. NIC queues, IRQ affinity and RPS/XPS spread, frequency scaling governor, NUMA locality of NICs.
* NIC: offloads not active that the NIC supports, ring sizes, drops/overruns from the counters, flow control.
* Memory and softirq load under traffic, and the throughput the forwarding path reached (from the counters).

### Phase 10b: MACsec changes (decided 2026-10-04, later the same day; supersedes "on by default" above)
1. **Off by default.** On by default only on a stacking link whose two ends both have MACsec offload in the NIC
   (each end reports its port's `macsec-hw-offload` in the link handshake). Software MACsec on a stacking link is
   optional (configured), because of its CPU cost.
2. **Per link, not per member**: the setting is per stacking port (e.g. `virtual-chassis vc-port <port> macsec
   on|off|auto`, auto = on when both ends offload; plus a stack-wide default). Several VC ports between the same
   members may be in **mixed** operation: some encrypted, some plain (migration, or an encrypted link over an
   untrusted path to a remote site next to a plain local one).
3. **Migration**: adding a second VC port that offloads MACsec, moving traffic over, then removing the old one must
   work without losing the stack; the stack MTU overhead is then per link (58 plain, 90 encrypted) and the MTU
   check uses each link's own value.
4. The spec (5.2 `virtual-chassis macsec`) and the code (internal/daemon/stackmacsec.go: `enabled` is stack-wide
   today; model.StackOverheadOf is stack-wide) must be changed accordingly.
Status 2026-10-04: done, unit-tested (`virtual-chassis macsec { mode auto|on|off; interface <if> mode …; }`, offload
in the stacking hello, decision per link in model.StackLinkMACsec, devices offloaded mac/phy with software fallback
only for `on`, per-port overhead in the MTU check and `show virtual-chassis mtu`; `disable` converted to `mode off`).
Lab: the VMs' virtio NICs cannot offload, so `auto` keeps them plain; `mode on` tests software MACsec there.

### Phase 14: No swap (decided 2026-10-04)
A switch never swaps: a swapped-out daemon misses its protocol timers. Without swap the kernel's OOM killer acts
when memory runs out (accepted; the memory slots, Phase 15, prevent it).
1. switchd turns every active swap off at start (`swapoff` of each `/proc/swaps` entry) and masks systemd swap
   units; it checks every 30 s and turns new swap off again.
2. Major alarm `switchd/swap` while swap cannot be turned off.
3. The image build fails when the image has a swap entry (fstab, swap units, a swap file).
4. Core dumps limited (`systemd-coredump` off, `ulimit -c` small) so a crashing daemon cannot take the memory of a
   full dump at the moment memory is short.

### Phase 15: Memory slots (decided 2026-10-04; reference "system memory")
Goal: `show system limits` shows what this hardware holds when every table is full **at the same time**; capacity is
guaranteed per table and static while the switch runs; it changes only at start or `request system reload`.
**Without `system memory allocation` nothing changes**: every table grows dynamically as today (only the kernel's
neighbour thresholds are raised to a sane value, see 7).

1. **Fixed part** (not in slots), computed at start from hardware and configuration (deterministic, not from the
   current use): kernel (RAM not in MemTotal is already gone; 1.6 % of RAM for page bookkeeping + 128 MiB), NIC rings
   (ring size × queues × buffer size per port, from ethtool), the daemons' base memory (per-daemon constant measured
   per release), system services and management (sshd, journald, udev, NTP, syslog; CLI/SSH sessions × session
   limit; `system memory management-reserve`, default 256 MiB + 24 MiB per allowed session), and a margin (5 % of
   RAM, at least 3 × `min_free_kbytes`) for packet bursts and the kernel's free-memory minimum.
2. **Slots**: the rest is split into slots of **4 MiB** (1024 pages). A slot belongs to exactly one purpose and holds
   a whole number of entries. Purposes (bytes per entry measured per release, today's values):
   `bgp-ipv4` 2,100 · `bgp-ipv6` 2,340 · `bgp-paths` 1,500 · `ospf` 1,150 · `arp` 512 · `ndp` 512 · `mac` 250 ·
   `multicast` 300 · `update` (the bundle in RAM: max bundle size + 1 slot write buffer; default 512 MiB + 4 MiB =
   129 slots; fixed size, never a percentage; the bundle needs no unpacking: the image is the squashfs itself).
3. **System area** (automatic, whole slots, from what the configuration allows): interfaces and VLAN/VXLAN
   declarations, virtual chassis (members, credentials, stacking ports, tunnel neighbours), Raft log/snapshots,
   configuration (candidate, active, rollbacks), static routes, DHCP leases (pool sizes), MACsec, RSTP/LACP/MC-LAG,
   BFD sessions, LLDP neighbours (new cap: 8 per port), alarms.
4. **Configuration**: `system memory { allocation { <purpose> (percent <1..100> | slots <n>); } update-size <size>;
   management-reserve <size>; }`. Percentages are of the slots after the system area and the update slot, rounded
   down. Commit refuses an allocation larger than the slots of the smallest member (any member can become master, so
   counts must fit every member); a change warns that it takes effect on `request system reload` (Phase 16) or a
   reboot. The slots not allocated are the **dynamic area** (archival, USB transfers, temporary data; no guarantee).
5. **Slot map**: switchd computes it at start from the active configuration and keeps it until the next start/reload
   (`/run/switchd/memory-slots.json`, handed to every daemon at registration). Capacities are enforced stack-wide as
   the minimum over the members (a failover never overflows).
6. **Enforcement** (only with an allocation):
   * cer-bgpd: a prefix beyond `bgp-ipv4`/`bgp-ipv6` is not stored (as if withdrawn), a path beyond `bgp-paths`
     likewise; major alarm; neighbours stay up. Existing routes are never evicted.
   * cer-ospfd: LSDB overflow as RFC 1765 (external LSAs beyond the cap are not originated/stored, the overflow
     state is left after the exit interval); alarm.
   * cer-ribd: refuses routes beyond the sum of the routing purposes (+ static); alarm.
   * Kernel: `gc_thresh3` (IPv4/IPv6) = `arp`/`ndp` capacity (thresh2 = 7/8, thresh1 = 1/2), bridge
     `fdb_max_learned` = `mac` capacity (cer-mclagd/VXLAN sync counts against it), `mcast_hash_max` = `multicast`.
   * Daemons: `GOMEMLIMIT` = base + capacity × bytes × 1.3 (GC headroom), cgroup `memory.max` = 1.5 × that (a runaway
     daemon is killed alone, not the switch); `memory.min` for switchd, cer-ribd, cer-lacpd, cer-bfdd, cer-mclagd,
     cer-rstpd so their code pages are never dropped under pressure (no swap: the only thing reclaim takes).
   * Update: a bundle larger than the update slot is refused before the transfer (size known) or the moment it
     exceeds it.
7. **Without an allocation**: unchanged dynamic behaviour, but the neighbour thresholds (default 1024, too small
   for a switch) are raised to a RAM-scaled value (1/64 of RAM ÷ 512 B), and an update is accepted only when free
   memory covers it.
8. **Commands**: `show system memory` (fixed part, system area, per purpose: slots, capacity, use, % full; applied vs
   configured when a reload is pending); `request system memory setup` (interactive: shows the slots of this
   hardware, asks per purpose for a percentage or count, shows the capacities live, writes `system memory` into the
   candidate, no commit); `show system limits` gets a column **Applied** left of the current column (now
   **Supported**): the capacity from the slots, or `-` without an allocation; new rows for the slot purposes (Supported
   = what all available slots would hold) and the update bundle size.
9. **Costs per entry**: `internal/memslots` holds the table; a measuring test (`-update`) rewrites it from the real
   structures (Go heap per entry) plus the kernel's object sizes; the test fails when the code drifts > 10 % from the
   table, so a release never ships stale costs. An update warns when a purpose's capacity in the new version falls
   below its current use.
10. **Structure optimization first**: interned BGP attributes (AS paths, communities, attribute sets via Go's
    `unique` package), RIB attributes shared between routes, compact prefixes (netip.Prefix, no strings); target ≈
    1/3 of today's bytes per route; costs table re-measured.

### Phase 15b: leftovers (planned 2026-10-05)
1. **BGP routes to cer-ribd as changes, not tables.** Today every change cycle (RoutesDelay) builds the whole table
   (pkg/bgp Speaker.table: a copy of every path), groups it per neighbour (rib.Route), sends it as JSON to cer-ribd and
   to every member, and keeps the last one (bgpd instance.last) for resends: with a full Internet table hundreds of
   MB of transient memory per cycle, on top of the slots. New:
   * **Speaker**: `OnChanged(prefixes []netip.Prefix, converged bool)` replaces OnRoutes: only *which* prefixes were
     decided since the last call (a prefix is the unit of change, so rank changes are included). The sender asks the
     event loop for their current ranked paths when it sends (`Paths(ctx, prefixes)`; empty = gone), so every message
     carries the newest state and a queued older one can never overwrite a newer one. `Prefixes(ctx)` lists the
     table's prefixes for a resync (the keys only, no paths).
   * **cer-ribd**: `routes.delta` {instance, protocol, seq, prefixes: [{prefix, routes (with source)}], sync:
     ""|"begin"|"end", full}. `rib.RIB.Replace(instance, proto, prefix, routes)` replaces every route of that
     protocol at the prefix (all sources; counts and limits as Set). Seq per (instance, protocol): a gap answers
     "resync" and cer-ribd keeps what it has. Sync: "begin" opens a generation, the pieces follow (4096 prefixes per
     message), "end" removes the protocol's routes of the instance not refreshed since "begin" (nothing is
     withdrawn before the new table is complete: no route flap, no drop).
   * **cer-bgpd**: one ordered worker sends the changes (split into pieces of 4096 prefixes); a failed call or a
     "resync" answer, cer-ribd reconnecting, or a new instance -> a full sync from the speaker's Snapshot. No
     instance.last any more.
   * **Members**: the same messages per member (stack op `bgp-routes-delta`, seq per member); a member that becomes
     reachable, a gap or an error -> full sync for that member alone. A member of an older release answers "unknown
     operation": it gets the old full SetRoutes (built from the snapshot pieces, only during a rolling update).
   * **Tests**: property test (random churn: deltas applied to an empty RIB equal RIB.Set of the full table, for
     every source), lost delta -> resync without a withdrawal of unchanged routes, member resync, mixed versions;
     memory test (bgp cost table: the transient per change cycle is bounded by the changes, not the table).
2. **Multicast count in `show system memory`**: the bridge's MDB entries (memberships) as the `multicast` purpose's
   use, read through netlink (RTM_GETMDB dump; not `bridge -j mdb`).
3. **`request system memory setup` across members**: the slots shown and the capacities computed are the stack's
   smallest member's (each member's plan through the ops `memory` call), as the reference says; today this member's.
4. **Found on the way (open)**: cer-ribd still revalidates every BGP route after a change of another protocol (a
   copy of references to all of them) and hands the kernel installer the whole active table on every change
   (netdev.SyncRoutes diffs it): with a full table a large transient per change cycle as well. Next: install from
   RIB.Changes() (only the changed prefixes; a full reconciliation at start and every few minutes), and revalidate
   only the BGP routes whose next hop lies under a changed non-BGP prefix.
Status 2026-10-05: 1-3 done, unit-tested (not on a device). 1: the sync's "end" sweeps only when the speaker is
converged (a restarted cer-bgpd starts with an empty table: sweeping then would remove the kernel's routes before BGP
has learned them again); a converged speaker syncs once more; an empty delta every 30 s lets a restarted cer-ribd
notice the gap; cer-ribd's reconnect forces a sync. Costs per entry: bgp-ipv4 1781, bgp-ipv6 2093, bgp-paths 1281
(the export copy of 440 B per route is gone; a piece of 4096 prefixes, 3.8 MiB measured, is in the daemon base).

### Phase 16: `request system reload [member <id> | all-members]` (decided 2026-10-04)
Restarts the whole switch software without rebooting the operating system (applies a new slot map, Phase 15).
1. Drain as for maintenance mode (5.2): mastership moves away, stacking paths route around, MC-LAG legs leave their
   bundles after the partners stopped sending, OSPF advertises max-metric (stub router) and waits for the neighbours
   to move away, BGP neighbours get a Cease (administrative shutdown) NOTIFICATION after their routes were withdrawn
   (graceful shutdown community where configured).
2. Forwarding stops: switch ports go down.
3. All cer-* daemons stop (reverse start order), switchd exits with the reload code; systemd starts it again; it
   computes the new slot map, applies the kernel limits, starts the daemons, applies the configuration, brings the
   ports up and leaves maintenance mode.
4. `all-members`: one member at a time, each back and in sync before the next (as the rolling update); this member
   last. A single switch asks first (it does not forward while it reloads).
5. Super-user, `[yes,no] (no)` question, every CLI session is notified; shown in `show system uptime`
   ("software started" vs "system booted").

### Phase 17: Software upload and RAM-only bundles; USB storage (decided 2026-10-04)
1. **Bundles never touch the disk**: every bundle (fetched by http/https/ftp/sftp, uploaded, or read from USB) is
   received into a sealed `memfd` (RAM, in the update slot when slots are allocated; else accepted only when free
   memory covers it); SHA-256 and signature are checked as now. Members receive it over the stack into their own
   memfd. The update daemon gets the memfd (fd passing) and writes the image into the backup slot (as now, 4 MiB
   chunks synced). The config check of the new version mounts the image from the memfd. A received bundle lives until
   it is installed, replaced, 1 h unused, or the member restarts (`/var/lib/ceros/software` is no longer used).
2. **Upload over HTTPS** (`system services web-management`: the first part of the REST API): `PUT
   /api/v1/software/upload` (Basic auth with a local user of class super-user; others 403), `POST
   /api/v1/software/install` (same options as the CLI), `GET /api/v1/software` (status). Listens only in the
   management instance. Without `certificate`/`key`, a temporary self-signed certificate (ECDSA P-256) is generated
   at start and held only in RAM; its SHA-256 fingerprint is shown by `show system services web-management` (and
   logged). Size limit `system services web-management upload-limit` (default 1 GiB, at most the update slot).
   CLI: `request system software add upload` installs the uploaded bundle.
3. No FTP/SFTP server on the switch; fetching from servers stays.
4. **USB storage**: `save usb:<file>`, `load override|merge|replace usb:<file>`, `file list usb:`, `request
   system storage usb eject`. Mounted (vfat/exfat/ext4) only for the operation at `/run/switchd/usb`, synced and
   unmounted right after; eject also powers the port's device off. Updates from USB read the bundle into RAM.
   **Plan (2026-10-05)**:
   * **Which stick**: `usb:` is the stick in the switch the CLI session runs on (where swcli runs: the member you
     logged in to, or whose console you use), as home-directory files already are. swcli cannot mount (it runs as
     the user), so it asks its local switchd (RPC `usb`), which checks the user's class (operator for read/list/
     save/eject) from SO_PEERCRED and does the work. `request system software add usb:<file>` keeps reading on the
     master for now (the bundle is fetched there); a stick on another member is a later step (that member reads
     the bundle into its RAM and sends it to the master over the existing bundle stream).
   * **Finding sticks** (`internal/usbstore`): block devices from sysfs (`/sys/block/sd*`) on a USB bus or marked
     removable, **never the system disk**: every disk holding a mounted file system of the running system
     (`/proc/self/mountinfo` major:minor → `/sys/dev/block/M:m` → its disk) or a cerOS partition (labels `ceros-*`,
     the A/B slots, config and data) is excluded. physw4 boots from a USB stick: without this, "the first USB disk"
     (today's software/fetch.go) can be the system itself. First stick by sysfs order; on it, the first partition (or
     the whole disk) whose file system mounts.
   * **Mounting through the kernel API** (unix.Mount/Unmount, no `mount` tool): types tried in order vfat, exfat,
     ext4 (others: "unsupported file system"); options `nosuid,nodev,noexec`, plus `ro` for reads; at
     `/run/switchd/usb` (0700). Every call under hwio deadlines (`system timeouts disk-operation`); a stick that
     hangs raises the hang alarm, as every device. One USB operation at a time per member (mutex, a second waits up
     to 5 s, then "the USB stick is busy"). After a write: fsync of the file and syncfs, then unmount.
   * **Paths**: relative to the stick's root; opened with openat2 RESOLVE_IN_ROOT|RESOLVE_NO_MAGICLINKS (no `..` or
     symlink escapes). Writes go to a temporary name and are renamed over the target (a pulled stick never leaves a
     half file under the real name). Size limit for configuration files: 16 MiB.
   * **Commands**: `save usb:<file>`, `load merge|replace|override|set usb:<file>` (config mode; same semantics as
     files); `file list usb:[<dir>]` (operational: the stick (vendor, model, size, file system, label), then name,
     size, time; directories with `/`); `request system storage usb eject` (sync, unmount, delete the SCSI device,
     power the USB port off through sysfs `remove`; then "the stick can be removed"). Completion for `usb:` paths
     lists the stick's files (one read-only mount).
   * **Software fetch** (software/fetch.go) uses the same finder and mounter (system disk excluded, read-only API
     mount).
   * **Tests**: finder against a fake sysfs/mountinfo (USB system disk excluded, partitions, removable flag); path
     resolution (escapes refused); a fake mounter for the operation order (mount ro/rw, write temp + rename, sync,
     unmount even on errors); CLI with a fake RPC. Real mounts need root: lab (sw2/sw3 with a virtual USB disk from
     Proxmox) when the lab is up.

### Phase 17b: One update at a time (requested 2026-10-04)
Today the master refuses a second update while its own runs, and a member's update daemon refuses a second install
while it writes. Missing:
1. **Stack-wide**: before an update or rollback starts, every member's update daemon is asked; any member with an
   update in progress (writing, rebooting, waiting for health, rolling back) refuses the new one, naming the member
   and its state. A new master (failover during an update) therefore cannot start a second one; it continues
   reporting the running one.
2. **The bundle in use is locked**: an upload or a download that would replace the bundle an update is using is
   refused (409 / CLI error) until the update is done.
3. `request system reload|reboot|halt`, `request system software rollback`, `request virtual-chassis member remove`
   and `request system zeroize` are refused while an update runs (they name it); a shell's `reboot` still works.
4. The update daemon persists "busy" over the reboot it causes (already in its state file) and answers it in status.

### show system limits: always "used of available" (requested 2026-10-04, later)
Every "n of m" line counts the same thing on both sides. E.g. `MACsec offload: 0 of 5 ports` is wrong when none of
the 5 ports can offload: it must be `0 of 0` (ports using offload of the ports able to). Go through every line of
the page (and `show system offload`) for the same mistake.

### Phase 18: REST API for orchestrators (decided 2026-10-04; replaces the web UI)
One API per virtual chassis (master, `cme` address) or single switch, extending `system services web-management`
(reference 5.1; spec first, endpoint by endpoint):
1. Configuration: read (active, candidate, revisions, `json`/`set`/curly), private candidate sessions (load
   merge/replace/override/set, delete), compare, commit check, commit (confirmed, comment), confirm, rollback;
   the same commit engine as the CLI (locks, confirmation, per-member results).
2. State: everything `show` gives, as JSON (interfaces, VC, LACP, MC-LAG, RSTP, routes, BGP/OSPF, alarms, processes,
   memory, limits), `member <id>`/all-members as a parameter.
3. Requests: the `request`/`clear` commands (reboot, reload, maintenance mode, software, daemons).
4. Events: a stream (server-sent events) of notices, alarms, commits and link changes, so an orchestrator need not
   poll. `/healthz`, `/readyz`, `/metrics`.
5. Authentication: Basic as today, plus API tokens per user (for orchestrators); an OpenAPI description.
**Plan 2026-10-05** (slices; each spec first in reference 5.1, then tests):
* 18.1 configuration and commands: the API runs the CLI itself (a cli.Shell per request, or per configuration
  session held on the server with `configure private`), with the user's class, so locks, confirmation, permissions
  and per-member results are the CLI's. `POST /api/v1/cli` (operational commands, questions answered from the
  request), `GET /api/v1/config[?format=json|set|text]`, `GET /api/v1/config/revisions`, sessions (open, load,
  commands, compare, check, commit, delete; idle 30 min, 8 per user), `POST /api/v1/config/confirm`.
  **Done 2026-10-05**, tested end to end against a real engine and CLI (internal/webapi/cli_test.go).
* 18.2 state and events: `GET /api/v1/state/<name>[?member=<id>]` = the Ops structures behind `show` as JSON
  (interfaces, virtual-chassis, lacp, mclag, spanning-tree, routes, bgp, ospf, alarms, processes, memory, limits;
  the cli types have JSON tags); `GET /api/v1/events` (server-sent events: the notices every CLI session gets,
  which include alarms and commits; link changes later); `/healthz` (unauthenticated, 200 while the API runs),
  `/readyz` (the configuration applied, a master known), `/metrics` (Prometheus text: interfaces' counters, alarms
  by class, daemons running, memory per purpose; authenticated).
* 18.3 API tokens: `system services web-management api-token <name> { user <u>; hash <sha256>; }` (in the
  configuration: they work on every master); `request system api-token create <name> user <u>` prints the token
  once (32 random bytes, base64url) and writes its hash into the candidate; `Authorization: Bearer <token>` has the
  user's class. `GET /api/v1/openapi.json` (written by hand, checked by a test against the routes).
  **18.2 and 18.3 done 2026-10-05** (unit-tested). Changed on the way: the OpenAPI description is generated from
  the route table that also registers the handlers (not written by hand). Open: link-change events, `member=`.

### Order (2026-10-04)
Lab deploy and tests of everything since 05240a8 → Phase 14 (swap) → Phase 17.1–3 (RAM bundles, upload) and 17b → Phase 15
(memory slots, with the structure optimization first) → Phase 16 (reload) → Phase 17.4 (USB) → Phase 10 (MACsec).
Each: reference first, tests, lab.

### VM needs by phase
| Phase | VMs needed |
|---|---|
| 0–2 | none (local) |
| 3–4 | sw1, srv1 |
| 5 | sw1, sw2, sw3 |
| 6–7 | sw1, sw2, srv1 |
| 8–9 | all five |

## 9. Known limits / non-goals
* Routing is basic: irb and routed ports, static routes, OSPF/OSPFv3 (Phase 9c) and BGP (Phase 9b, own core). No other routing
  protocols, no policy routing, no MPLS.
* Throughput is bounded by the host/NIC (kernel bridge). Expect roughly 10–40 Gbit/s on decent x86 with large frames, and lower with small packets. Hardware offload is only available where switchdev drivers exist.
* Target scale: 2–4 members typical, **up to 16** supported (Raft: up to 7 voters, the rest are non-voting
  replicas; see §3), ARM + x86 mixed, ≤10G today. 100G later → XDP/eBPF fast path and switchdev offload
  are kept as a future milestone. The dataplane package is an interface, so a fast path can be added.

## 10. Decisions taken (2026-09-29)
* SSH: system OpenSSH, with swcli as login shell for config-defined users. The serial console uses the same flow (getty → login → swcli).
* Data-plane encryption: MACsec on any port; on stacking links per link, by default only where both NICs offload (Phase 10b, 2026-10-04; WireGuard dropped). The control plane always uses mTLS.
* Three separate planes: data (switch ports), stacking (dedicated 1:1 stacking ports in a ring, TLS over an L2 stream,
  multi-hop relay, plus client traffic between members in stack tunnels, 2026-09-30), and mgmt (administration only).
* Stack control runs **only** over stacking ports. The mgmt network carries only administration.
* BFD everywhere liveness matters: stacking links (IP-less).
* MTU follows Junos convention (frame size incl. 14-byte header, default 1514).
* Bulk port config via `interface-range` (member-range and wildcards, also for hot-plugged NICs).
* RSTP is required in v1, with MC-LAG-aware integration.
* Dev machine = build and unit tests only. Integration tests run on Proxmox VMs running Debian 13 (see §11).

## 11. Test lab (Proxmox)
All VMs: **Debian 13 (trixie)**, 2 vCPU, 2 GB RAM, 16 GB disk, virtio NICs, plus a serial port
(`serial0: socket`) for console tests. Root SSH with the dev machine's key.

Installation (all VMs): Debian 13 **netinst**, and in tasksel select **only "SSH server"** (no desktop, no
"standard system utilities"). Configure the mgmt NIC in the installer as usual (ifupdown; DHCP or static). Leaving
that on the switch VMs is intended: it is the realistic starting point that `switchd takeover` must handle.
Then install: `qemu-guest-agent tcpdump ethtool iperf3 nftables` (and on srv1/srv2 nothing more; bonds are
created with iproute2). Do not install NetworkManager, docker or a firewall.
Take a Proxmox **snapshot "clean"** of every VM right after this, so tests can start from a known state.

| VM | Role | NICs |
|---|---|---|
| sw1 | MC-LAG peer A | mgmt, stk-12, stk-13, peer1, peer2, srv1-a, underlay, loop-13 |
| sw2 | MC-LAG peer B | mgmt, stk-12, stk-23, peer1, peer2, srv1-b, underlay, loop-23 |
| sw3 | 3rd stack member (Raft quorum, VXLAN remote, RSTP loop) | mgmt, stk-13, stk-23, underlay, srv2, loop-13, loop-23 |
| srv1 | dual-homed server (LACP bond to sw1+sw2) | mgmt, srv1-a, srv1-b |
| srv2 | single-homed server on sw3 | mgmt, srv2 |
| physw4 (10.5.20.76) | physical box: 4× BCM5719 (01:00), 2× Intel 82576 (04:00), onboard r8169 mgmt (07:00) | real NICs, offload/ethtool behaviour |

`stk-*` are the stacking links (a ring sw1–sw2–sw3). Each non-mgmt link is its own point-to-point bridge on Proxmox (the `underlay` bridge is shared by sw1/2/3), with **MTU 9000+**.
**Important:** Proxmox *Linux* bridges never forward LACP (01:80:C2:00:00:02) and drop
BPDUs when STP is on. The p2p link bridges must therefore be **OVS bridges with
`other-config:forward-bpdu=true`**, or directly connected via something equally transparent.
