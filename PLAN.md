# mclag — Linux HA switch (plan)

Goal: turn ordinary Linux boxes (any arch, any NIC) into a stack of HA-capable L2
switches with MC-LAG, VXLAN, a Junos-like CLI (SSH + serial), and a web UI/API.

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
| Data | switch ports (e.g. 10G), peer-links, VXLAN | client traffic only, no IP on any port |
| Stacking | dedicated stacking ports (e.g. 1G), direct 1:1 cables | Raft/config, state, MC-LAG sync, RSTP relay, BFD. IP-less (EtherType 0x88b5) |
| Management | IP on any VLAN (IRB-like) or a dedicated port, VRF `mgmt` | SSH, web, syslog, NTP, DNS, MC-LAG BFD heartbeat |

* Every switch is a **member** with an ID (1–16). Interfaces are named `<member>/<linux-ifname>`, plus
  stack-global `ae<N>`. `interface-range` (member-range, wildcards) handles large and hot-plugged port sets.
* **Stacking ports** are designated locally (`request stack port add <if>`), because a switch needs them before it
  has any configuration. Each stacking link runs a reliable L2 stream (seq/ack/retransmit/fragmentation, exposed as a
  `net.Conn`) with **TLS 1.3 mTLS** on top, plus IP-less **BFD** for fast failure detection.
* **Topology**: chain, ring or mesh. Members exchange link-state (adjacency) over the stacking plane, and messages to
  non-adjacent members are relayed hop by hop along the shortest live path. Each hop is TLS-protected, and all
  members are authenticated stack members.
* The stacking plane **never** listens on data ports. Stacking-EtherType frames on access, trunk or VXLAN ports are
  client traffic and are switched normally. Tested explicitly.
* Config and CLI/web work from any member. Writes go through the Raft leader over the stacking plane, and each
  member applies only its own part of the tree.
* Joining: a new switch with designated stacking ports announces itself on them. `request stack member add <id>
  token <t>` authorises it → it gets a certificate from the stack CA → it joins Raft (≤5 voters, the rest non-voting).
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
* Commit-time validation catches mismatched MTUs (peer link < MC-LAG MTU,
  VXLAN underlay < overlay + 50, etc.).

### 4.3 MC-LAG
* The pair is 2 members sharing:
  * a **peer-link**: a pure data bundle, all VLANs tagged, no IP, never blocked by RSTP, with IP-less **micro-BFD**
    per port;
  * the **stacking plane** for MAC sync, state and consistency;
  * a **BFD heartbeat** over mgmt (RFC 5881/5883), for split-brain decisions only.
* Both peers announce the same LACP system ID/priority and disjoint port-number ranges, so the partner sees one LAG.
* **MAC sync**: switchd watches netlink FDB events. MACs learned on an MC-LAG bond are sent to the peer and installed
  as static FDB entries on the same bond there. Ageing is coordinated.
* **Split horizon**: traffic arriving from the peer-link must not leave on an MC-LAG bond that is up locally (tc/nft
  rule). It is lifted per bond when the local leg fails.
* **Failure matrix**: over the stacking path, peer-link and heartbeat (config reference §5.6). The secondary disables
  its MC-LAG legs whenever the peer is alive but data can no longer flow via the peer-link.
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
* No routing between VLANs, and none between mgmt and data. SSH, web, syslog, NTP and DNS bind into VRF `mgmt`.
* Without a `management` block, the host's existing network config is left untouched (safe first install).

### 4.6 CLI (Junos-like)
* Operational mode: `show interfaces [terse|extensive]`, `show ethernet-switching table`,
  `show vlans`, `show lacp interfaces`, `show mclag`, `show vxlan`, `show stack`,
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

### 4.8 Web UI / API
* HTTPS (self-signed or configured cert), in the mgmt VRF.
* REST: `GET/POST /api/v1/config` (candidate, compare, commit with the same
  semantics as the CLI), `/api/v1/state/*` (interfaces, FDB, LACP, MC-LAG, VXLAN, stack).
* `/healthz` (liveness), `/readyz` (config applied, quorum, peer state).
* `/metrics` Prometheus (per-port counters, drops, FDB size, peer RTT…).
* UI: small embedded SPA (served from the binary). It shows stack overview, port
  grid per member, live counters, FDB search, config editor with diff and commit, and alarms.
* Auth: the same users/classes as the CLI.

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
4. **Web / API**: HTTPS server (self-signed or configured cert) in the mgmt VRF, login with the config users,
   REST for config (candidate / compare / commit / rollback, with the same semantics as the CLI) and state,
   `/healthz`, `/readyz`, `/metrics` (Prometheus).
5. Minimal web UI: login, dashboard, interfaces, FDB search, config editor (text + diff + commit).

### Phase 5: Stacking (VMs: sw1, sw2, sw3 with stacking NICs in a ring)
1. **PKI**: stack CA created on the first member, join tokens, CSR signing, automatic cert renewal.
2. **Stacking transport**: stacking-port designation (local state), AF_PACKET sockets bound *only* to stacking ports,
   a reliable L2 stream as `net.Conn` (fuzzed and loss-tested in-process with simulated lossy links), TLS 1.3 mTLS on
   top, and IP-less BFD per link.
3. **Stack topology**: adjacency discovery, link-state flooding, hop-by-hop relay with shortest live paths. Tests for
   chain/ring/mesh and link loss, all in-process with simulated links.
4. **Plane separation test**: stacking-EtherType frames injected on data ports are forwarded untouched and never
   reach the stack code.
5. **Raft store** (over the stacking transport) replaces the local store. Config locks work across the whole stack. Voter management is automatic
   (max 5 voters, the rest are non-voters). Witness mode (a member without a data plane).
6. Per-member apply with results reported back: `commit` prints the result per member (like Junos VC).
7. Stack-wide operational commands: `show interfaces` / `show stack` for all members, `request … member N`.
   CLI and web work from any member.
8. Failure tests: leader killed, stacking link cut (ring re-route), partition, member rejoin, and a check that the
   data plane keeps forwarding without quorum.

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
3. Consistency checks (VLANs, MTU, LACP parameters). On a mismatch the bond is set to proto-down with a reason.
4. **MAC sync**: learned MACs, moves, coordinated ageing and flush on link down.
5. **Split horizon** via nftables, updated dynamically with each leg's state.
6. Failure handling (matrix over stacking path, peer-link and heartbeat): leg down, peer-link down with the peer alive (the secondary shuts its ports), peer dead,
   switchd crash, and reboot/rejoin (delay-restore timer).
7. `show mclag`, `show mclag consistency`, alarms.
8. Failure-matrix tests with measured convergence (high-rate ping and iperf3 from srv1).

### Phase 8: RSTP (VMs: sw1, sw2, sw3 in a loop, + srv2)
1. 802.1w state machines in pure Go (port roles, proposal/agreement, edge/p2p, TC → FDB flush), unit-tested.
2. I/O: bridge in user-mode STP, BPDUs via AF_PACKET, port states via netlink.
3. Edge ports, BPDU guard, root guard, cost and priority, and 802.1D compatibility.
4. **MC-LAG integration**: shared bridge ID. The primary computes MC-LAG port states, and BPDUs arriving on the secondary's
   leg are relayed over the peer link. The peer link is never blocked. When the peer fails, the survivor keeps the bridge ID.
5. Interop tests against mstpd on a srv VM, plus loop tests (sw1–sw3–sw2 triangle).

### Phase 9: VXLAN (VMs: sw1, sw2, sw3, srv2)
1. One vxlan device per member in vnifilter mode, VLAN↔VNI mapping, underlay source interface, MTU checks.
2. Control plane over the stack channel: VTEP and MAC advertisements per VNI, head-end replication (flood) lists,
   remote MAC installation, MAC moves.
3. Anycast VTEP for MC-LAG pairs.
4. Static external VTEPs and a flood-and-learn mode for non-stack peers.
5. Loop safety: VTEP-to-VTEP forwarding is never allowed (split horizon), so the mesh is loop-free by construction,
   and VXLAN ports are excluded from RSTP.
6. `show vxlan`, `show vxlan remote-vteps`, `show ethernet-switching table vni …`.

### Phase 10: Data-plane encryption (opt-in per link)
1. **MACsec** on the peer link. Keys (SAKs) are generated and rotated by switchd and exchanged over the mTLS channel,
   so no wpa_supplicant/MKA is needed. Hardware offload is used where the NIC supports it.
2. **WireGuard** underlay for VXLAN: keys are generated per member and distributed via the stack, and VXLAN runs over WireGuard IPs.
   Commit checks cover the MTU budget (overlay + 50 + 60).
3. Benchmarks on x86 and one ARM board, with the numbers documented.

### Phase 11: Polish and packaging
1. Full web UI: stack view, port grid per member, live graphs, MC-LAG/RSTP/VXLAN status, alarms.
2. `show system alarms`, `request system software add` (rolling upgrade across the stack), config archival.
3. `.deb` packages (Debian first) and a bootstrap script. Docs: user guide and a CLI reference generated from the schema.

### VM needs by phase
| Phase | VMs needed |
|---|---|
| 0–2 | none (local) |
| 3–4 | sw1, srv1 |
| 5 | sw1, sw2, sw3 |
| 6–7 | sw1, sw2, srv1 |
| 8–9 | all five |

## 9. Known limits / non-goals
* No inter-VLAN routing (only mgmt IP).
* Throughput is bounded by the host/NIC (kernel bridge). Expect roughly 10–40 Gbit/s on decent x86 with large frames, and lower with small packets. Hardware offload is only available where switchdev drivers exist.
* Target scale: 2–4 members typical, **up to 16** supported (Raft: max 5 voters, the rest are non-voting
  replicas), ARM + x86 mixed, ≤10G today. 100G later → XDP/eBPF fast path and switchdev offload
  are kept as a future milestone. The dataplane package is an interface, so a fast path can be added.

## 10. Decisions taken (2026-09-29)
* SSH: system OpenSSH, with swcli as login shell for config-defined users. The serial console uses the same flow (getty → login → swcli).
* Data-plane encryption: opt-in per link (MACsec peer link, WireGuard underlay). The control plane always uses mTLS.
* Three separate planes: data (switch ports, peer-link), stacking (dedicated 1:1 stacking ports, IP-less, TLS over an
  L2 stream, multi-hop relay), and mgmt (IRB-like IP on any VLAN or a dedicated port, VRF `mgmt`).
* Stack control runs **only** over stacking ports. The mgmt network carries just the MC-LAG BFD split-brain heartbeat.
* BFD everywhere liveness matters: stacking links (IP-less), peer-link ports (IP-less micro-BFD), heartbeat (UDP over mgmt).
* MTU follows Junos convention (frame size incl. 14-byte header, default 1514).
* Bulk port config via `interface-range` (member-range and wildcards, also for hot-plugged NICs).
* RSTP is required in v1, with MC-LAG-aware integration.
* Dev machine = build and unit tests only. Integration tests run on Proxmox VMs running Debian 13 (see §11).

## 11. Test lab (Proxmox)
All VMs: **Debian 13 (trixie)**, 2 vCPU, 2 GB RAM, 16 GB disk, virtio NICs, plus a serial port
(`serial0: socket`) for console tests. Root SSH with the dev machine's key.

| VM | Role | NICs |
|---|---|---|
| sw1 | MC-LAG peer A | mgmt, stk-12, stk-13, peer1, peer2, srv1-a, underlay, loop-13 |
| sw2 | MC-LAG peer B | mgmt, stk-12, stk-23, peer1, peer2, srv1-b, underlay, loop-23 |
| sw3 | 3rd stack member (Raft quorum, VXLAN remote, RSTP loop) | mgmt, stk-13, stk-23, underlay, srv2, loop-13, loop-23 |
| srv1 | dual-homed server (LACP bond to sw1+sw2) | mgmt, srv1-a, srv1-b |
| srv2 | single-homed server on sw3 | mgmt, srv2 |

`stk-*` are the stacking links (a ring sw1–sw2–sw3). Each non-mgmt link is its own point-to-point bridge on Proxmox (the `underlay` bridge is shared by sw1/2/3), with **MTU 9000+**.
**Important:** Proxmox *Linux* bridges never forward LACP (01:80:C2:00:00:02) and drop
BPDUs when STP is on. The p2p link bridges must therefore be **OVS bridges with
`other-config:forward-bpdu=true`**, or directly connected via something equally transparent.
