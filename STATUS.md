# Status / where to continue

Last updated: 2026-10-03 (evening).

## Done
- Phase 1.1–1.3: schema, config tree, set/curly/JSON formats, diff (tested + fuzzed).
- Commit-check validation (`internal/model`), interface-range, Junos MTU, three-plane stacking design.
- Spec: `docs/config-reference.md` (tests keep index, prose coverage and examples valid).
- Directives: `inactive:` / activate / deactivate (all formats, diff `!` lines, `Tree.Active()` used by
  `model.Build`), `load merge|replace|override|set` (atomic, `replace:`/`delete:`), `copy`, `rename`.
- Commit engine (`internal/commit`): FileStore (atomic writes, 50 revisions, pending state written before the
  revision it covers), sessions shared/private/exclusive, commit check incl. operator permissions,
  apply with revert on failure, confirmation (required/optional, stricter policy, timer restart, rollback to
  last confirmed, restart-safe), `config.Patch` for private `update`. Tested with fake clock/applier + race detector.
- CLI engine (`internal/cli`): operational + configuration mode, all §3.2 commands, commit variants, pipes,
  schema/config-aware completion and `?` help, Junos-style caret errors, permissions, panic recovery;
  file I/O and prompts go through the client (Terminal). Fuzzed (FuzzShell).
- RPC (`internal/rpc`, JSON lines, SO_PEERCRED auth), swcli client (`internal/swcli`: line editor, Tab/?,
  history, bracketed paste, pager, Ctrl-C interrupt, degraded mode), switchd daemon (`internal/daemon`, dry-run
  applier only). One binary: `swcli` is a symlink to `switchd`. `packaging/switchd.service`, `lab/deploy.yml`.
  Checkpoint A reached: running on sw1 in dry-run; commit/confirm/automatic rollback verified across a restart.
- Lab: all five VMs bootstrapped via Ansible (lab/), root key login works.
- Plan §4.15: OS ownership (takeover/release, masking conflicts, sysctls, foreign-change revert); lab install steps in §11.
- Plan §4.14: hitless reconfiguration (diff-driven, no link down, tighten-before-loosen, planner property tests).

- Lab VMs have static management addresses (lab/static-ip.yml with revert guard); lab/README.md documents it.

## In progress: Phase 3 data plane (`internal/dataplane`)
- Done and tested: state.go, desired.go (Compute),
  plan.go (hitless Plan: tighten → structure → loosen → up → cleanup), kernel.go (Kernel interface,
  Execute, Fake kernel with Linux semantics), plan_test.go (property test: subset rule after every op,
  untouched links, idempotence, convergence; example plans; Compute notes).
- Reference 1.4 updated: released interfaces go down and leave bridge/bundle; switchd owns `swbr0` and ae bonds.
- Real apply on sw1 (switchd without --dry-run): `test/lab` (go test -tags lab ./test/lab) passes: access
  isolation, tagged frames dropped on access ports, trunk + native VLAN, 0 loss during 5 commits on another
  port, released port goes down, restart plans nothing.
- Static LAG + jumbo MTU lab tests; show interfaces/ethernet-switching table/vlans, clear table.
- Self-healing: link events + 30 s periodic reconcile against the last applied config (reverts foreign
  changes in ~1.2 s, configures appearing ports); inventory with IFLA_MAX_MTU and HasIP (wildcards never
  select ports with OS IP addresses, explicit use warns). 10 lab tests pass.
- mac-limit (userspace enforcement) and storm control (tc chains: link-local bypass, pps policers with
  goto chain; rate kept in flower classid; stale rules removed on read). 13 lab tests pass.
- Management plane: VRF mgmt (table 100), IRB mgmt0 on a bridge VLAN (bridge self VLAN = only that VLAN
  reaches the CPU) or dedicated port, static addresses + default routes, l3mdev_accept for sshd; torn down
  when unconfigured. 14 lab tests pass.
- Remote syslog (`internal/syslog`): hub with ring buffer (`show log`), RFC 5424 forwarders over UDP/TCP/TLS
  from VRF mgmt, 10k queue dropping oldest, facilities change-log/authorization/interactive-commands,
  `show system syslog`. 15 lab tests pass.
- Accounts (`internal/access`): configured users become OS accounts with swcli as shell (only accounts
  switchd created; conflicts are commit errors; root never managed), root-owned authorized_keys, SHA-512 crypt
  (matches spec vectors and openssl), `plain-text-password`, `start shell` (super-user). Lab: key and password
  SSH logins, classes enforced, removal ends sessions. 16 lab tests pass.
- CLI SSH server: own sshd instance (switchd-sshd, /etc/switchd/sshd_config, group switchd-cli, root via
  ForceCommand swcli); the OS sshd is never modified; port conflicts are commit errors. Lab uses port 2222
  (Proxmox firewall opened for 2222). Serial consoles: auto-detected UARTs/USB adapters get serial-getty.
- Property test verified by mutation (deleting VLANs after adding them is caught as a leak).

- Multi-user notices (commit/confirm/rollback), shared candidate persisted, swcli survives switchd
  restarts (offline prompt, reconnect) and its own crashes (supervisor, crash report, shell for super-users),
  local consoles (serial + display) autologin root into the CLI; `system ports login-required`. 17 lab tests pass.

- Phase 4b steps 1–3 done: x/y/z interface names (pinned card numbers, old configs converted hitlessly,
  verified on physw4), show chassis hardware, L3 (irb, routed ports/subinterfaces, static routes, IPv4+IPv6,
  forwarding only on switchd's L3 interfaces, accept_ra 2 for OS NICs), show system rollback, show arp /
  ipv6 neighbors, show system uptime, request system reboot/halt/power-off, host name + resolv.conf in the OS.
  18 lab tests pass.

- Phase 4b step 4 done: hardware capability checks (max speed, pause support, vlan-challenged, bundle speed
  mix) and show system offload.

- Junos restructuring (2026-09-30): stack -> virtual-chassis (mastership-priority), management moved to
  routing instances (system management-instance + routing-instances mgmt_ceros, per-member irb addresses),
  data routing instances (virtual-router VRFs), protection filter (nftables) for data L3 addresses, show route.
  Old configs converted on read (internal/daemon/upgrade.go). physw4 is switched off (user), deploy to sw1 only.

## In progress: Phase 5 stacking
- Done: link protocol (docs/stack-protocol.md, internal/stack/link) with BFD-style liveness; AF_PACKET I/O
  (verified sw1 ens20 <-> sw3 ens19, stk-13); stack keys (internal/stack/pki: Ed25519, no expiry, join tokens).
- Spec: Junos VC syntax (request virtual-chassis vc-port set pic-slot <card> port <port>, show virtual-chassis),
  mastership switch + member remove (decommissioning).

- Stack manager in switchd (internal/stack): VC ports, show virtual-chassis [vc-port], authenticated member
  sessions per stacking link, join with one-time tokens (member add / join token; config handed over, old one
  kept, switchd restarts with the new member id). Lab: sw1, sw2, sw3 form one virtual chassis
  (3d643177f94057f8) over the stk ring; test hosts moved off stacking links (hSw2 on underlay ens1, hotplug on
  ens21).

- Mesh (internal/stack/mesh): link-state topology, hop-by-hop relay, streams (net.Conn, credit flow control).
- Stack control (internal/stack/control): Raft (hashicorp/raft) over mesh streams; replicated revisions, pending
  confirmation, shared candidate, member list, join tokens; founder bootstrap carries the old history; 7 voters by
  priority; priority handoff after elections; leadership transfer; member removal.
- switchd: configuration mode relayed to the master (prompt/banner {master:N}), commits applied on all members with
  per-member results, stack-wide commit checks (each member's hardware and OS), catch-up of members that missed a
  commit, `request chassis routing-engine master switch`, `request virtual-chassis member remove` (the removed
  switch becomes a stack of its own with its config), member list enforced on sessions. Replicated state is only
  acted upon once current (a re-joining member replays the log; no old revisions reach the data plane).
- Lab: sw1-sw3 form one Raft stack (re-joined with tokens after the upgrade). 19 lab tests pass, incl.
  TestVirtualChassis (commit from sw2, removal and re-join of the master under 100 pps traffic: 0 loss).
- Fixed on the way: account creation recorded before useradd (sw3 had an orphaned "thorben" from a restart during
  creation; its state file was repaired by hand, the log proved switchd created it); swcli batch mode answers
  questions from the input.

- Operational commands on other members (`member <id>` | `all-members` | `local`), witness role, a member shows its
  own commit at once after a relayed session; lab tests for ring cut, master killed, minority partition (forwarding
  continues, configure refused), stacking frames on data ports. 23 lab tests pass.

- Phase 6 LACP done: internal/lacp (802.1AX machines, pure, tested; fuzzed PDU), LACP bundles are Linux team devices
  (per-port enable by switchd, link follows distributing ports, BPF transmit hash), state restored across switchd
  restarts (0 loss in the lab), show lacp interfaces/statistics. Lab interop with a Linux 802.3ad bond (the lab sets
  virtio speed/duplex: 802.3ad needs them).
- Phase 7 MC-LAG core done: shared LACP system, peer-link (all VLANs, no learning), split horizon, leg state exchange,
  holds (peer-link down on the secondary, delay-restore), MAC synchronisation, micro-BFD on peer-link ports (peer-link
  state only; single-port removal and authentication open), show mclag. Lab TestMCLAG: srv1 dual-homed.
- MC-LAG consistency checks (bundle facts with the leg states, secondary holds after 10 s; show mclag consistency).
- Phase 7c stack tunnels (decided 2026-09-30): client traffic between members in VXLAN over a hidden routed underlay
  on the stacking ring (VRF swstack), replacing the MC-LAG peer-link (peer-link, micro-BFD, heartbeat removed;
  stored configs converted); split horizon on the peer's tunnel, DF rule for third members, forget messages,
  two-member split forwards at all costs, minority rule for 3+; stack MTU check, show virtual-chassis mtu, VLAN 4094
  reserved. Lab: TestMCLAG passes on the ring (ring cable cut under traffic: 18 of 500 pings lost, ~180 ms);
  TestStackJumbo: plain and host-VXLAN jumbo frames pass across members.
  Fixed on the way: the stacking socket died on ENETDOWN (port restart when it joins the VRF); SyncL3 deleted the
  stack table's routes; without an unreachable default, tunnel lookups fell through to the main table (mgmt port).

## Done 2026-09-30 evening (unit-tested; NOT yet deployed/verified in the lab, deploy was not permitted)
- Lab test TestRoutedUnits (routed subinterfaces in several instances with the same subnet; passes on sw1).
- CLI SSH server runs inside VRF mgmt_ceros when `system management-instance` is set (`ip vrf exec`); spec 5.1;
  lab assertions added to TestManagementPlane (need the new build on sw1-sw3).
- `request virtual-chassis force-master` (two-member commit override): Raft RecoverCluster at next start, peers
  come back as non-voters until reachable; unit test TestForceMaster; spec 5.2. Needs a lab test.
- Member removal: the removed switch becomes member 1 (config rewritten by internal/daemon/leave.go, stacking and
  mclag statements and VC ports deleted, new keys). Needs a lab test (sw3 remove + re-join).
- Decisions by the user: irbs keep routing within an instance (as in Junos); overlapping subnets in one instance are
  an error; operational commands stay local by default, `member <id>` forwards (already implemented, check
  `request system halt member 2` in the lab); Proxmox stack NIC MTU is now 9100 (update stackJumbo, re-test).

- Branding (user, 2026-09-30): the product is **cerOS** (metal: Cerium). The management routing instance is
  `mgmt_ceros` (was `mgmt_junos`; stored configs are converted on read, internal/daemon/upgrade.go). Renamed:
  docs/PLAN titles, `show version`, systemd unit descriptions, new stack certificates ("ceros stack"). NOT renamed on
  purpose: Go module `mclag`, binaries `switchd`/`swcli`, /etc/switchd, unit names, the `mclag` feature statement
  (MC-LAG), the LACP system-id hash string (would change the bundles' system MAC).

- Origin rules (internal/dataplane/origin_linux.go): with a management instance, what the switch originates itself
  (unbound connects: DNS, NTP, apt, ssh clients) is routed by table of mgmt_ceros only, `unreachable` otherwise
  (rules 1100/1101 after the l3mdev rule; unspecified source + every management address, v4 and v6). Servers keep
  their reply paths. Lab TestOriginViaManagement passes. Found on the way: TCP re-routes each packet after the source
  is chosen, so matching only the unspecified source is not enough.
- NTP (internal/ntp): own SNTP client through the management VRF (step > 128 ms, slew below; prefer, lowest delay;
  replies validated), `show system ntp`, commit warning when another time service runs. Lab TestNTPViaManagement.
- CLI SSH unit change now needs `systemctl restart` (a reload kept the old process outside the VRF).
- Lab tests added: TestRoutedUnits, TestOriginViaManagement, TestNTPViaManagement, TestVirtualChassisRemoveRenumber.
  TestForceMaster (stops switchd on sw2/sw3) was written but NOT added: the user must allow taking sw2/sw3 down.
- physw4 (10.5.20.76) received the build with the first deploy of the day; the dev box lost its route to it (wg11)
  afterwards, the user's own session stayed up. Its config has no management instance, so the origin rules are inactive there.
- Open: stack NICs still report max MTU 9014 (Proxmox mtu 9100 needs the VMs restarted / NICs re-attached; user
  restarts them), then re-run TestStackJumbo.

- Anycast gateway fixed: bridge + irb MAC derived from the stack id (was the OS MAC; sw1/sw2 only matched as
  cloned VMs), DAD off on irb units (every member holds the same IPv6 addresses). Lab TestGatewayMAC.
- A removed member keeps its VC port designations (deleting them broke re-joining).
- Lab after the VM restart (stack NICs now 9114): TestStackJumbo passes (host MTU capped by srv1's NIC, still 9000:
  srv1 needs a full Proxmox stop/start). TestForceMaster added and passes.
- Proxmox bridges: forward-bpdu had not been active after the MTU change (ovs_options does not apply it; user set it
  live with ovs-vsctl). LACP, MC-LAG, storm control pass again.

## cerOS firmware image (2026-10-01, decided by the user)
- cerOS ships only as an image (the .deb / binary-swap path is gone): docs/os-image.md is the design (disk layout,
  mounts, boot, A/B slots, GRUB boot counting, config/data partitions, signed bundles, rollback, failure table).
  Reference 3.6 rewritten; `system root-authentication` added (5.1) because /etc is volatile.
- Code: internal/software (bundle.go: Ed25519-signed bundles, streaming verification; slots.go: grubenv, slot
  detection, slot write + read-back), internal/updated (install to the backup slot, reboot, health confirmation,
  rollback, config backup/restore, `check` for the master), daemon/software.go orchestrates bundles; old
  package/install code removed. `switchd bundle|keygen|verify-bundle`.
- Image: image/ (Dockerfile, build-rootfs.sh with mmdebstrap, dracut module 90ceros, grub.cfg, make-disk.sh,
  build.sh = `make image`). Development key in image/keys (dev.key is meant to be public; release builds use
  CEROS_SIGNING_KEY / CEROS_TRUSTED_KEYS).
- QEMU tests (test/image/test-update.sh, vm.py drives the serial console): boot + confirm, update, rollback command,
  unhealthy switchd -> daemon rollback, corrupted hash tree -> verity -> boot loader returns, unreadable slot ->
  GRUB starts the other slot, power loss during the slot write, changed/foreign bundles rejected.
  Found on the way: dm-verity only notices blocks that are read (a test must corrupt the hash tree); GRUB's
  `fallback` does not catch errors inside a menu entry (boot_slot checks the kernel itself).
- Open: lab switches still run Debian + binary (migrate with new VM disks, user's go); `request system zeroize` /
  `storage cleanup`; arm64; Secure Boot (UKI with the root hash); delta updates.
- VC guide review (docs/VC-Bestpractice-guide.pdf, read in full): docs/vc-guide-review.md lists the deviations and
  the questions for the user.

## Requested 2026-10-03 (added to the routing plan of 2026-10-01)
- **One program per protocol** (decided 2026-10-03, PLAN.md Phase 9a, reference 1.9): done 2026-10-03, unit-tested,
  NOT yet run on the lab switches. switchd writes and starts the units (cer-<name>.service: nice/real-time,
  OOM score, capability bounding set, watchdog 10 s, Restart=always 100-200 ms), checks them every second, reports
  failures/recoveries to every CLI session of the stack; `show system processes`, `restart <daemon>`.
  Daemons: cer-lacpd, cer-mclagd, cer-rstpd, cer-lldpd, cer-syslogd (journal -> syslog, kernel messages included),
  cer-ntpd, cer-dhcpcd (leases only; switchd adds addresses), cer-ribd (every route; the data plane installs none
  now), cer-bfdd (real-time, on demand). IPC: pkg/ipc (calls + state topics with resync), internal/svc (protocol),
  internal/daemonkit; stacking messages are relayed by switchd under their old names (mixed versions work).
  Hitless restarts by design: LACP (lacp.json, waits for MC-LAG holds), MC-LAG (no delay-restore when legs are up,
  maintenance as a topic), RSTP (rstp.json), DHCP (leases in /run, resumed), routes (nothing removed before a
  source reported), switchd (daemons keep running; it waits up to 5 s for the leases). test/daemons runs every
  program against a fake switchd. Image: all programs installed, journal volatile (tmpfs, 64 MB).
  Fixed on the way: LACP partner choice depended on PDU arrival order (TestWrongPartner flaked 29/200); the CLI
  session test's notice race (2/40); ipc listeners unlinked their successor's socket.
  Open (first lab run): check every unit's capability set and `show system processes` on all members (a too narrow
  bounding set shows as a failing daemon); /sbin/bridge-stp on the read-only image root (EnsureSTPHelper writes it);
  BFD interval commit checks (W below 100 ms, E below 50 ms) are not in the model yet; route replication from the
  master for the routing protocols comes with cer-ospfd/cer-bgpd.
- **Modular code base.** Reusable libraries, and separate modules for the programs, so each one builds on its own:
  - Libraries (pure, documented APIs, no switchd types): network devices through netlink (links, bonds/teams,
    bridge ports and VLANs, VRFs, routes with protocol ids, FDB, neighbours), nftables tables, hardware state
    (ethtool features, speeds, PCIe, sysfs inventory), process and service supervision (start, stop, watch,
    restart external daemons and systemd units: sshd instance, getty, an out-of-process routing daemon if one is
    ever needed), and the protocol cores (LACP, RSTP, LLDP, BFD, OSPF, RIB, policy, MKA later).
  - Programs as separate modules: switchd, swcli (a binary of its own instead of the symlink; it needs only the RPC
    client and the line editor, no netlink), the update daemon, rtest. A Go workspace (go.work) ties them together
    while they live in one repository; later they can move into repositories of their own without code changes.
  - Steps: (1) module path `mclag` -> the repository path; (2) move the libraries into `lib/...` with their own
    go.mod, keeping internal/ for switchd-only code; (3) cmd/swcli and cmd/switchd-update as programs; (4) Makefile
    targets per program (`make switchd`, `make swcli`, `make update`) and CI building each module alone;
    (5) docs (README: the module map). Done between protocol steps, as one mechanical change with no behaviour
    change (all tests green before and after).
- **OSPFv3 with full IPv6** is part of the first version (as agreed): the OSPF core is written for both versions
  from the start (address family per interface, link-local next hops, LSA types as 16-bit codes, instance ids),
  OSPFv2 packet codec first, the OSPFv3 codec and LSAs right after.
- **MC-LAG for every protocol** (reference 5.8 "Routing in a virtual chassis", corrected): OSPF/OSPFv3, BGP and BFD
  over irb interfaces whose VLANs ride MC-LAG bundles, and over routed MC-LAG bundles: unicast protocol frames that
  arrive on a member that is not the master are passed to the master through the stack tunnel (nft netdev
  ingress, VLAN kept), so sessions end on the master and a leg failure is no protocol event. Tests: in-process
  (two receive paths), docker interop with a partner bonded to two "members", lab with srv1 on an MC-LAG.
  LACP already handles MC-LAG (5.6).
- **ECMP** everywhere: the RIB keeps up to 16 equal-cost next hops (done), the kernel gets multipath routes (done),
  OSPF SPF computes all equal-cost paths, BGP `multipath`/`multiple-as`, and switchd sets
  `fib_multipath_hash_policy` = 1 (layer 3+4) for IPv4 and IPv6 (reference 5.8).
- **MC-LAG loop (user report: an upstream switch shut ports for STP reasons).** Found and fixed 2026-10-03: a
  joining leg forwarded before its peer filtered traffic from the stack towards its own leg (up to one 50 ms step
  plus message time), and a draining leg (maintenance mode) kept receiving while the peer had lifted the filter. In
  both windows the partner's flooded frames came back to it on the same bundle; with RSTP off the stack floods BPDUs,
  so the partner saw its own BPDU and blocked the port. Now a joining leg is announced to the peer first
  (lacp.Runtime.BeforeJoin -> RPC mclag-leg-joining, the peer installs the filter before answering, 300 ms
  timeout), and a draining leg keeps broadcast/multicast filtered on the peer (legsMsg.Draining). Unit-tested;
  to be checked in the lab with an external RSTP/STP switch on an MC-LAG (mstpd on srv1) once the harness works.

## Done 2026-10-03 (later; unit-tested, NOT yet on the lab switches)
- Graceful shutdown of the cer- daemons (stop stages with budgets and kills).
- Deadlines on every kernel, disk and tool call (pkg/hwio, pkg/sysexec, pkg/nlx), alarms for hanging devices,
  switchd watchdog fed by its loops; slots read from the disks with errors (show system software/version); /var
  noexec; docs/os-image.md: what is signed and checked (§4.1) and hanging devices (§7).
- sshd's /run/sshd on the image (switchd creates it before `sshd -t`; tmpfiles entry).
- **OSPF and OSPFv3** (PLAN Phase 9c): one core for both versions (pkg/ospf: codecs, state machines, flooding,
  origination with throttling, SPF with ECMP, areas, externals, overload), cer-ospfd (Linux raw sockets, routes to
  cer-ribd, replicated to the members, export policies, show/clear commands), the protection filter lets OSPF in on
  OSPF interfaces. Tests: simulated networks for both versions; real sockets in network namespaces (OSPFv2 here,
  OSPFv3 needs IPv6, which this sandbox lacks).
- **MC-LAG for protocols**: protocol frames for irbs (OSPF, BFD, BGP) go from every non-master member to the master
  unchanged (tc redirect into the stack tunnel; MACs, VLAN and packet kept; an untagged frame gets its port VLAN's
  tag for the tunnel); routed interfaces of other members and routed MC-LAG bundles relay OSPF over the stacking
  protocol (ospf-rx/tx/link). The tc frame test skips here (no tc classifiers in this kernel): run it in the lab.
- **RSTP**: the image ships /sbin/bridge-stp (RSTP did not start on physw4: read-only root), an alarm and the reason
  in `show spanning-tree` when RSTP cannot run; bpdu-block (cer-rstpd watches, switchd keeps the port down,
  `clear error bpdu interface`, disable-timeout, persistent); `clear spanning-tree protocol-migration|statistics`.
- **Phase 5**: reliable mesh streams (retransmission, reordering, window probes, 30 s give-up; fallback to the old
  streams for members of an older release): a CLI session relayed to the master, Raft and RPC survive a stacking
  path change (docs/stack-protocol.md "Mesh").

## Requested 2026-10-03 (still to do, in this order)
1. **Other**: LACP port numbers: done 2026-10-04. The formula stays (no number changes, no flap on update); a commit
   with a port of card >= 16 or port >= 64 in an LACP bundle fails (16 bits cannot number every valid port; static
   bundles take any port), and cer-lacpd never gets an unnumbered port. Wireshark (decode Raft msgpack, and the
   reliable-stream messages OPEN2..PROBE2 the dissector does not know yet): postponed by the user 2026-10-04.
2. **BFD**: interval check at commit: done 2026-10-04 (W below 100 ms on a member with fewer than 4 CPUs; each
   member checks the sessions it runs or, as a possible master, may run: routed ports by owner, irb/MC-LAG/BGP on
   every non-witness member; model.CPUCounter, kernelInventory.CPUs, checkBFD; E below 50 ms is the schema's range).
   BFD for OSPF/OSPFv3 and the relay for routed interfaces of other members: done 2026-10-04, unit-tested. The master's
   cer-ospfd asks for a session per 2-Way neighbour on interfaces with bfd-liveness-detection (OSPFv3: link-local
   peer with the device as zone), in its own cer-bfdd when it has the device (irb, own port, MC-LAG with a master
   leg), else in the relaying member's (ospf-bfd-set, whole list, every 10 s; answered with the states). Up -> down
   takes the neighbour down at once (pkg/ospf NeighborFailed); a session never up changes nothing. Relayed states go
   to the master in order with a count of failures (a lost report is repaired). Open: lab run, interop with FRR's
   bfdd; BGP's BFD client comes with cer-bgpd.
3. **ECMP**: done 2026-10-04: switchd sets net.ipv4/ipv6 fib_multipath_hash_policy = 1 (layer 3+4, reference 5.8)
   on every L3 sync (only written when different; skipped without IPv6); checked by the lab's TestRouting (not run
   yet). The first sync after the update re-hashes existing multipath flows once (no drops; a flow may move).
4. **LACP with the UniFi (physw4)**: investigated 2026-10-03. cerOS behaves correctly: tcpdump shows well-formed
   LACPDUs leaving 1/0/2 every second (actor a6:2c:0c:23:c4:1f key 1 port 1026, partner = UniFi port 23 key 66), and
   the UniFi's 0/23 counters show them arriving (CPU-trapped, as on the working 0/24), yet its LACP keeps an all-zero
   partner on 0/23. Port settings of 0/23 and 0/24 are the same; STP is fine (3/1 forwarding, no BPDUs returned, no
   loop). Periodic timers are per port, as in 802.1AX (not a cause). Remaining: user tests on the UniFi (bounce 0/23
   or no addport/addport; swap the cables at physw4 to see whether the fault follows the UniFi port or 1/0/2).
   Done on the way: cer-lacpd warns when a send fails (d12f6bd) and reports a partner that does not receive our
   LACPDUs (its PDUs never name our port) instead of "configure periodic slow" (02a7717).
5. **`request daemon restart|stop|start <daemon> [member <id>|all-members]`**: done 2026-10-04 (decided: a stop lasts
   until the reboot). The supervisor never starts a stopped daemon (needed or "always"); the set is in
   /run/switchd/stopped-daemons (tmpfs: survives a switchd restart, not a reboot); `show system processes` shows
   "stopped (request daemon stop)"; the update daemon can be restarted, not stopped; `restart <daemon>` stays as a
   hidden short form. Unit-tested (supervisor, CLI).
6. ~~Applying `request system diagnose` hints~~: declined by the user 2026-10-04.
7. OSPF follow-ups: graceful restart (helper and restarting, grace LSAs), lab interop with FRR (v2 and
   v3, broadcast and p2p), the punt frame test and OSPFv3 sockets in the lab.

8. **physw4 field issues (2026-10-03)**: RSTP did not run because the image cc68085 lacks /sbin/bridge-stp (fixed in
   72ce500, build check in cadfbef; physw4 must be updated). The update failed on the USB 2.0 system stick: a 64 MiB
   slot sync exceeded 60 s and the retry's rename over the unsynced bundle hung /var for 3 min; fixed in 6cb46ae
   (sync per 4 MiB, bundles written back before the rename). Workaround for updating from cc68085:
   vm.dirty_bytes=8 MiB. Not yet verified on the device.
9. Proposals decided 2026-10-04: no SFTP on the CLI SSH server (files are copied with scp), no diagnose hint for a
   USB system disk.
12. **Commands checked** (2026-10-04): TestEveryCommand runs all ~125 operational commands bare and with `?`
   against fakes implementing every optional interface (no panic, internal error, broken format verb or blocking);
   the spec's command list was compared with the CLI tree: only `show bfd session` was missing, now implemented
   (stack-wide: every member's cer-bfdd; address, state, interface, detect time, interval, multiplier; `extensive`:
   clients, member, up time, discriminators, counters). BGP's BFD sessions no longer name a pseudo-interface.
   Clean-up: staticcheck (unused code, empty branches) clean; Kate swap files ignored.
11. **Update resiliency review** (requested 2026-10-04), fixed and unit-tested: (a) confirming a healthy new slot
   is retried while the disk does not answer (before: given up, the boot loader rolled the healthy version back at
   the next reboot, and the member stayed "updating"); (b) a failed reboot (rollback or install) is retried, and
   System.Reboot falls back to `systemctl reboot --force` and then the kernel's reboot after a sync (before: a
   broken new version kept running and every later update was refused as "in progress"); (c) the health time
   counts from the boot, not from the update daemon's (re)start; (d) an install interrupted after the update
   record but before the boot state (power loss) is reported as such and puts no configuration back (before:
   "the new system did not start"). Reviewed, no change needed: slot invalidated before it is written, record
   before boot state, boot state last; switchd's rolling update stops on every error, master last.
10. **System timeouts** (requested 2026-10-04): done, `system timeouts { disk-operation; kernel-call; slot-write;
   software-transfer; software-install; member-update; health-check; config-check }` (reference 5.1, defaults as
   before). switchd applies them on every commit (hwio deadlines atomic now, slot writes, update waits); the update
   daemon reads them from the active configuration at start and with every request (so a raised value holds over
   the reboot: the health check of the new version). Unit-tested. Not covered: the cer- daemons keep the default
   disk/kernel deadlines (they rarely touch a disk).

## Phase 9b: BGP and the full `show route` (started 2026-10-04)
- **`show route` in full** (reference 5.14): done 2026-10-04, unit-tested. From cer-ribd's RIB (every route, best
  first) instead of the kernel: Junos layout per table (`inet.0: n destinations, m routes (...)`), `*` active, age,
  metric/tag/localpref, AS path, next hops (`>`), Local/Discard; `terse`, `detail`/`extensive` (state, inactive
  reason, OSPF area/path type/tag, BGP attributes), `summary` (cer-ribd routes.summary); filters `<address>`
  (longest match), `<prefix> [exact|longer]`, `protocol`, `next-hop`, `active-path`, `table`, `instance <n>|all`;
  `member <id>`/`all-members` run it on that member (its replicated RIB). Typed route attributes (rib.Attrs);
  OSPF fills area, path type and tag. Open: `hidden` (needs BGP import rejects), `receive-protocol`/
  `advertising-protocol bgp` (with cer-bgpd), marking routes that differ from the kernel.
- **BGP: own core, GoBGP's packet codec only** (decided 2026-10-04, PLAN Phase 9b; the GoBGP server was rejected:
  Junos policy semantics, hitless changes, gRPC/memory). Step 1 done 2026-10-04: `pkg/bgp` (deps: the codec only).
  Sessions over any net.Conn (reader/writer goroutines, one event loop), FSM with collision resolution (RFC 4271
  §6.8, also against an established session the neighbour decided against: replaced without a flap, paths kept
  until End-of-RIB), capabilities (4-byte AS with AS_TRANS/AS4_PATH for 2-byte neighbours, IPv4/IPv6 unicast,
  route refresh, graceful restart), RFC 7606 error handling (treat-as-withdraw / attribute discard), Adj-RIB-In
  (as received + after import policy: hidden), decision process (localpref, AS path length, origin, MED within
  the neighbour AS, eBGP over iBGP, router/originator id, cluster list, address), multipath per neighbour
  (+multiple-as, 16 paths), route reflection (originator id, cluster list, loop checks), eBGP rules (prepend
  local-as, next hop self, no localpref, MED not passed on unless set by policy, remove-private),
  no-export/no-advertise, only active routes announced (SetLocal inactive list), originated routes need an export
  policy, graceful restart helper (stale until End-of-RIB or restart time) and R bit when restarting, End-of-RIB,
  hitless Configure (only neighbours whose session settings changed are reset), SetPolicy without route refresh,
  show/clear data (Status, AdjIn, AdjOut, Clear hard/soft/soft-inbound). Tests: real TCP on 127.0.0.x (eBGP,
  route reflection, import policy and hidden, multipath and AS loop, hitless changes, graceful restart, bad peer
  AS), codec round trips. Differences from Junos so far: local-as prepends only the local-as (Junos also the
  global AS unless `private`); IGP metric to the next hop is not compared.
- Step 2 done 2026-10-04: **cer-bgpd** (internal/bgpd, cmd/cer-bgpd; supervised unit "bgp", CAP_NET_BIND_SERVICE,
  stop stage 0 with 10 s). One speaker per instance on the master; config from switchd (bgpcfg.go: effective
  neighbour settings, hold time 90, multihop TTL 64, next hop self addresses of the other family from the unit
  facing the neighbour). Linux sockets: dual-stack [::]:179 bound to the instance VRF, TCP MD5 per neighbour on the
  listener and the dialer (IPv4 neighbours v4-mapped), TTL 1 eBGP / 255 iBGP / multihop, **Multipath TCP off**
  (Go opens listeners as MPTCP, which has no TCP MD5: found by the MD5 test). Policies through internal/policy
  (default import accept; default export: BGP routes only, others need `from protocol`); routes to cer-ribd per
  neighbour source (every path in show route; rank, attributes), Full after End-of-RIB from every neighbour,
  replicated to the members (bgp-routes); exports every 5 s from cer-ribd's active routes of other protocols.
  cer-ribd resolves BGP next hops through the longest active non-BGP route (connected: on that interface; behind
  OSPF/static: its next hops; only through BGP or the own address: not installed). Protection: TCP 179 from the
  configured neighbours. Status/adj/clear methods (bgp.status, bgp.adj, bgp.clear) for step 3. Tests: daemon
  against a speaker over loopback (routes per source, import reject = hidden, localpref, static export, default
  export rejects OSPF, non-master stops), TCP MD5 on real sockets, next-hop resolution. Open: a BGP route with an
  unresolvable next hop is still "active" in the RIB (only not installed; Junos hides it), import `preference`.
- Step 3 done 2026-10-04: `show bgp summary|neighbor [<ip>]|group [<name>] [instance <n>]` (Junos layout: peers,
  tables, Active/Received/Accepted per table, capabilities, policies, last error, counters), `clear bgp neighbor
  [<ip>] [soft|soft-inbound] [instance <n>]`, `show route receive-protocol|advertising-protocol bgp <ip>
  [<prefix>] [detail]`, `show route hidden` (paths rejected by import policy or a loop check). Unit-tested.
- Step 4 done 2026-10-04: commit check warns per neighbour "bgp neighbor X: this change resets the session
  (<settings>)" for peer-as, local-address, local-as, authentication-key, type, family, multihop, hold-time,
  passive, graceful-restart (spec updated: graceful-restart is in the OPEN), and for a changed autonomous-system or
  router-id (every session of the instance); model.ChangeWarnings(active, candidate), called by the commit engine.
  Route-reflector client status changes in place (no reset).
- Step 5 done 2026-10-04: BFD for BGP. The master's cer-bfdd runs a session per neighbour with
  bfd-liveness-detection (client "bgp"): single-hop for a connected neighbour, multihop (UDP 4784) from the local
  address for eBGP multihop and iBGP beyond the connected subnets. Up -> down ends the BGP session at once (Cease,
  subcode 10 BFD down) and no new session starts (dial or accept) until BFD is up again; a BFD session that never
  came up changes nothing; BFD removed from the configuration releases the neighbour. Tests: speaker and daemon.
  Found by the race detector on the way: the instance name was read without the lock (now immutable).
- Step 6 done 2026-10-04: sessions to neighbours on routed ports of other members (switchd marks the owning member:
  routed port or single-member bundle with the neighbour's subnet; irb and MC-LAG: none). The owner's cer-bgpd
  listens in the VRF for those neighbours (TCP MD5, TTL) and dials them for the master (bgp-relay-dial); the bytes go
  as ordered stack calls (bgp-relay-open/data/close, 16 KiB chunks, sequence-checked); the master's speaker sees an
  ordinary connection with the real addresses (next hop self = the owner's port address). Test: master, owner and a
  router behind the owner's port, both directions of connection set-up. The race detector found a send on a closed
  channel in the first version (now a done channel).
- BFD for relayed neighbours (done 2026-10-04): the owner runs the session in its cer-bfdd (it has the same
  configuration) and reports state changes to the master in order with a failure count (bgp-bfd-state; resent to a
  new master); the master runs BFD only for neighbours it reaches itself.
- Interop (done 2026-10-04): `make interop-bgp` runs pkg/bgp against the GoBGP server in one process (module
  test/interop/gobgp of its own: GoBGP's server never becomes a cerOS dependency): session both ways, 4-byte AS,
  IPv4 with MED, communities, large communities, IPv6 over an IPv4 session (next hop of the other family),
  withdrawals both ways, route refresh without reset. FuzzUpdate: received UPDATE bodies through GoBGP's parser and
  our decoder (RFC 7606 handling) without panics (~750k inputs, none found).
- Open (BGP): RFC 7606 tests with
  malformed attributes; interop tests with FRR/GoBGP in network namespaces and in the lab (lab down since
  2026-10-04); a route with an unresolvable next hop is not hidden in the RIB; import `then preference`; local-as
  prepends only the local AS.

## Next (in order)
- Done 2026-09-30: maintenance mode (`request system maintenance-mode enter [force]|exit [member <id>]`): drain flag in
  the mesh LSAs (0 byte; transit avoided where another path exists, never master), mastership handed on, MC-LAG legs
  reported down to the peer 300 ms before the hold, learned addresses moved to the peer tunnel before LACP says
  "not in sync" and again before the last port leaves; LACP keeps a held port in the kernel bundle until the partner
  stops distributing (DrainWait 2 s). Persistent (`<state>/maintenance`). Reboot/halt/power-off, member removal (RPC
  "drain") and system shutdown (SIGTERM while `systemctl is-system-running` = stopping) drain first. Lab: TestMCLAG
  drains sw2 and the master sw1 under 10 ms pings: 0 lost.
- Done 2026-09-30: RSTP, stack = one bridge (internal/rstp state machines; daemon/rstp.go owner = lowest reachable
  member, snapshot copy every second, user-space STP via /sbin/bridge-stp). TestRSTP: loop-23 becomes
  designated/backup, owner stop/start keeps every port state. Open RSTP items: bpdu-block (model only), clear
  spanning-tree commands, a lab test with an external RSTP bridge (mstpd on srv1) and an MC-LAG port with BPDUs.
- Done 2026-09-30: family inet dhcp (internal/dhcp: RFC 2131 client on AF_PACKET, leases merged into the unit's
  addresses and the instance's default route unless a static default exists; show dhcp client binding).
  TestDHCPClient (busybox udhcpd on srv1): bind, renew, expiry, re-bind, replace by static.
- Done 2026-09-30: port mirroring (dataplane/mirror*.go: tc clsact matchall/flower + mirred, before the storm
  filters, classification continues; per-VLAN via vlan_id and num_of_vlans 0). TestPortMirroring (capture on sw1's
  output port: the lab's OVS link learns the mirrored MACs).
- Done 2026-09-30: VLAN mtu filter (nft bridge table switchd_vlanmtu, forward+input, tagged by vlan id, untagged by
  ibrpvid; counters in show vlans extensive). TestVLANMTU (access ports; the tagged path is not lab-tested yet).
- Done 2026-09-30: card number lifecycle (inventory: card info with driver/ports/MACs/last seen; absent cards listed
  in show chassis hardware; model change noted; moved card recognised by its MACs; request chassis card <n>
  renumber <m> | forget, with confirmation naming affected interfaces). Unit-tested (a VM cannot move PCI slots).
  The user's order (1,3,2,4,5) is done; kernel messages to syslog stay delayed.
  Earlier plan:
  port mirroring, cleanup (card numbers, VLAN MTU filter; kernel messages to syslog delayed). Parked for discussion
  with the user: management interface design, internal VLAN 4094.
- Done 2026-09-30 (later, user requests): full names + speed in `show virtual-chassis vc-port`; `show chassis hardware`
  covers all members by default; `?`/Tab offer every member's ports (ports RPC, cached 30 s). Local names already
  used the member id.
- Done 2026-09-30: stack tunnels + MC-LAG on the ring, path MTU probes and warnings, Wireshark dissectors
  (tools/wireshark), swcli banner after `?`/Tab, lab tests TestStackJumbo (plain, QinQ, host VXLAN) and
  TestConfigAcrossMembers. Full lab suite: 30 tests pass.
- Done 2026-09-30/10-01: chassis management (cme on the master, CLI forwarded to the master, start shell on the
  master), stack-wide interface listings, vc-port set <interface>, LLDP (one system to the outside), host names from
  one place (member name vs. chassis name), software updates through the stack (spec 3.6: package, check-config,
  distribution, member by member drained, master last, automatic return after 3 failed starts, transit check with
  force, older members ignore unknown statements). Lab: sw1-sw3 + physw4 updated 4c3e25e -> b131b9e -> 2f76bbb by
  the stack itself.
0a. Done 2026-10-01 (afternoon; deployed and checked on the lab stack):
   - IGMP/MLD snooping (Phase 8b, reference 5.5): on by default, per-VLAN contexts of the bridge (IGMP and MLD
     together), stack tunnels and VXLAN ports are permanent router ports, querier/version/immediate-leave/
     multicast-router-interface, MC-LAG group refresh on the peer's leg, show igmp|mld snooping membership|vlans.
     Lab: snooping on with the router ports set; no multicast traffic test yet (srv1/srv2 not reachable).
   - VXLAN (Phase 9, reference 5.7): the stack is one VTEP (switch-options vxlan source-address on swvtep of every
     member), swvx<vni> ports, head-end replication, no stack-tunnel -> VXLAN forwarding (nftables), remote MACs
     shared between members (vxlanSync), protection accepts the remote VTEPs, show vxlan [remote-vtep]. The stack
     tunnels move to UDP 4790 while VXLAN uses 4789 (shared port impossible across VRFs; found in the lab).
     Lab: objects created and removed cleanly on all 4 members; no traffic test with a real remote VTEP yet.
   - show system bottlenecks / request system diagnose (Phase 13, internal/diag).
   - Update daemon switchd-update (reference 3.6): installs, restarts switchd, rolls back once; lab: the update
     to ea234ce went through the daemon on all members.
   - Fixes: show route instance lists the instances; per-member errors without a misplaced caret.
   - Repository prepared for GitHub: README, .gitignore, CI workflow (vet, gofmt, unit tests, cross build).
   Open: end-to-end tests with srv1/srv2 (multicast receivers, a remote VTEP) once they are reachable again; the
   lab test harness still addresses sw2/sw3 directly; a LICENSE (the user's choice).

0. Review and cleanup (2026-10-01). Done and deployed to the lab stack (a827e79, all 4 members):
   - MC-LAG without domains (spec 5.6): a bundle with ports on two members is an MC-LAG; `mclag delay-restore` is
     the only setting; stored configurations converted (domain/flag removed); one LACP system id for the stack
     (derived from the stack id); minimum-links counts both members' ready ports; a member has one peer (E).
   - Safety: reserved routing-instance names (switchd's devices, ae*, default, ...) and port kernel names; syncVRF
     never deletes a non-VRF device; VLAN `all` reserved; W for bpdu-block, web-management, VXLAN (not implemented).
   - Show: stack-wide `show mclag` (per pair, both views); `show lacp` per member actor system and slow-partner
     warning; `show interfaces` merges MC-LAG bundle parts and lists member ports; `show log` names the member;
     `show system limits` own stacking links; `show route` hides swstack.
   - LLDP on bundle members: aggregated port id N+1, in-bundle state from LACP at run time, the ae's PVID.
   - No IPv6 on the bridge, its ports, bundle members, tunnels; stale neighbours of bare ports flushed.
   - switchd keeps its systemd unit current (embedded) and masks the OS network services / ends DHCP clients
     (lab: sw2/sw3 resolv.conf and address fights stopped).
   - Reconciliation churn fixed: origin block rules (library reads the rule action as 0) and static routes
     (RTN_UNICAST) were re-installed every 30 s; the log now names the steps of a routed-interface update.
   - Lab check: the UniFi still reports Defaulted on 23/24 although our LACPDUs/LLDPDUs leave the ports correctly
     (decoded): to be checked on the UniFi side (and sw1's Proxmox bridge vmbr802 path).
   Open from the review:
   - The lab test suite still addresses sw2/sw3 directly (10.5.176.96/.97, gone since the single cme address):
     rework the harness (reach members through the stack, e.g. `start shell` on the master after a mastership
     switch, or a jump through srv1) before TestMCLAG etc. can run again; TestMCLAG's expectations are updated
     for the new show mclag already.
   - Not yet reviewed line by line: config package, stack (manager/control/mesh/link/pki), rstp, dhcp, ntp,
     syslog, access, swcli, software. Reviewed: model, schema, dataplane, daemon (ops, stackops, mclag, lacp,
     lldp, applier), cli (operational, lacp), commit engine, rpc server.
   - LACP port numbers wrap for cards >= 16 or ports >= 64 (member*1024 + card*64 + port).
   Ideas (user, 2026-10-01): a separate per-member update daemon (install, restart, verify, rollback; switchd
   orchestrates); f/g of the earlier list dropped (lab topology).
1. Phase 7b rest: protocol version window (versioned stack messages), signed packages.
2. Wireshark: decode Raft msgpack (AppendEntries/RequestVote); the "ctl" JSON RPC payloads are already shown.
3. Open RSTP items: bpdu-block (model only), clear spanning-tree commands, lab tests with an external RSTP bridge
   (mstpd on srv1) and an MC-LAG port with BPDUs. VLAN MTU filter: lab-test the tagged path.
4. Kernel messages to syslog (delayed).
5. Later phases: IGMP, VXLAN (control plane), GoBGP (full show route), encryption, polish, 802.1X, diagnostics.

## Questions for the user
1. The protection filter on data L3 addresses is fixed (ping/ND/replies only). Do you want a Junos-like
   configurable filter (firewall filter on lo0) later, e.g. to allow SSH on a data irb deliberately?

Answered (2026-09-30): management is administration only (no stack sync over it); the CLI SSH server runs inside
mgmt_ceros when a management instance is set; two-member stacks: `request virtual-chassis force-master`; a removed
member becomes member 1; operational commands stay local by default (`member <id>` forwards); two-member split
forwards at all costs (stack tunnels replace the peer-link).

## Notes
- The dev machine is only for development: no network changes here; lab = Proxmox VMs (PLAN.md §11).
- Spec first: update docs/config-reference.md before implementing a feature.
