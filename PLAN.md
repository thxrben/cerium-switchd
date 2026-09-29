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

* Every node is a **member** with an ID (`member 1`, `member 2`, …). Any number of members.
* Interfaces are named `<member>/<linux-ifname>` in config, e.g. `1/enp3s0f0`.
  An optional per-member alias map (`ge-1/0/0` → `enp3s0f0`) allows Junos-style names.
  Interfaces are **discovered dynamically**: hot-plugged NICs show up in
  `show interfaces` and can then be configured. There is no fixed port count.
* Config and web UI can run from any member. Writes go through the Raft leader,
  and each member applies only its own part of the tree.
* Joining: `request stack join <addr> token <t>` → CSR is signed by the stack CA → the node becomes a Raft voter.
* Raft needs a majority to commit. With 2 members, a third lightweight "witness"
  (switchd in witness mode, e.g. on a VM/RPi) is recommended. Without quorum, the
  data plane **keeps running** on the last committed config and only config changes are blocked.

## 4. Features and implementation

### 4.1 L2 switching
* One bridge `br0` per member, `vlan_filtering=1`, access/trunk/native VLAN per port.
* MAC ageing, static MACs, per-port MAC limits, storm control (tc police), BPDU guard.
* Loop prevention v1: BPDU guard + MC-LAG consistency checks (STP question below).

### 4.2 Jumbo frames
* **Per port**: native (`mtu 9216` → netdev MTU on port/bond/member ports).
* **Per VLAN**: the Linux bridge has no per-VLAN MTU. We enforce it with
  nftables `bridge` family rules (`vlan id X meta length > N drop`) plus a counter.
  The effective limit is min(port, VLAN).
* Commit-time validation catches mismatched MTUs (peer link < MC-LAG MTU,
  VXLAN underlay < overlay + 50, etc.).

### 4.3 MC-LAG
* The pair is 2 members that share a **peer link** (a LAG between them) and a
  **keepalive** path (mgmt network, UDP+HMAC or mTLS) for split-brain detection.
* Both peers announce the same LACP system ID/priority and disjoint port-number
  ranges, so the partner sees one LAG.
* **MAC sync**: switchd watches netlink FDB events. MACs learned on an MC-LAG
  bond are sent to the peer and installed as static FDB entries on the same
  MC-LAG bond there. Ageing is coordinated, so an entry is removed only when both sides aged it out.
* **Split horizon**: traffic arriving from the peer link must not leave on an
  MC-LAG bond that is up locally (nft bridge rule). The rule is removed
  when the local leg fails, so the peer can forward via the peer link.
* **Failure handling** (Cumulus-clag style):
  * Local MC-LAG leg down → peer link unblocked for that bond, MACs point to peer link.
  * Peer link down, keepalive up → the *secondary* shuts its MC-LAG ports (LACP out of sync) to avoid a split-brain.
  * Peer dead (both down) → the survivor carries everything.
* Consistency checks: VLANs, MTU and LACP params must match on both peers, otherwise
  the bond goes proto-down with a clear reason in `show mclag`.

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
* Dedicated mgmt port (or a VLAN interface) inside a **mgmt VRF** (`vrf mgmt`),
  so management traffic is separated from the switched data plane. This port has no routing between VLANs.
* DHCP or static, default route in the VRF, DNS, NTP.
* SSH, web and syslog bind into the mgmt VRF.

### 4.6 CLI (Junos-like)
* Operational mode: `show interfaces [terse|extensive]`, `show ethernet-switching table`,
  `show vlans`, `show lacp interfaces`, `show mclag`, `show vxlan`, `show stack`,
  `show system alarms`, `monitor interface`, `request system reboot member N`, …
* Configuration mode: `configure [private|exclusive]`, `set`, `delete`, `edit`, `up`, `top`,
  `show`, `show | compare`, `show | display set`, `commit`, `commit check`,
  `commit and-quit`, `commit confirmed N`, `commit comment "…"`, `rollback N`, `exit`.
* Tab completion and `?` help are driven by the same schema that validates config.
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

## 5. Config example
```
set system host-name core
set system syslog host 10.0.0.5 transport tls
set stack member 1 hostname sw-a
set stack member 2 hostname sw-b
set interfaces 1/enp1s0 mtu 9216
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 member 1/enp1s0
set interfaces ae1 member 2/enp1s0
set interfaces ae1 mclag id 1
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk vlan members [ 10 20 ]
set mclag peer-link ae0 keepalive via mgmt
set vlans users vlan-id 10 mtu 1500 vxlan vni 10010
set vlans storage vlan-id 20 mtu 9000
set vxlan source-interface lo0 vtep 10.255.0.1 mode control-plane
```

## 6. Security vs. performance (important)

| Channel | Encryption | Cost |
|---|---|---|
| Control plane (Raft, MC-LAG sync, VXLAN control, keepalive) | mTLS 1.3 | Negligible (tiny traffic). **Always on.** |
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

## 8. Milestones (each ends with a commit + tests)
1. **Config core**: schema, tree, set/delete, commit/rollback/compare, persistent store; CLI REPL over unix socket.
2. **Local data plane**: bridge, VLANs, access/trunk, MTU per port and VLAN, mgmt VRF, syslog. Netns tests.
3. **Access**: user management, SSH (login shell), serial getty, web API, health, metrics, minimal UI.
4. **Stacking**: CA/mTLS, join, Raft, per-member apply, `show stack`.
5. **LACP + MC-LAG**: userspace LACP, peer link, keepalive, FDB sync, split horizon, failover. Netns tests with a simulated partner switch.
6. **VXLAN**: vnifilter device, control-plane mesh, anycast VTEP for MC-LAG pairs.
7. **Security extras**: MACsec peer link, WireGuard underlay option.
8. **Polish**: full web UI, packaging, docs, `commit confirmed`, alarms.

## 9. Known limits / non-goals
* No inter-VLAN routing (only mgmt IP).
* Throughput is bounded by the host/NIC (kernel bridge). Expect roughly 10–40 Gbit/s on decent x86 with large frames, and lower with small packets. Hardware offload is only available where switchdev drivers exist.
* STP: see open question.
