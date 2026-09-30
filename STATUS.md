# Status / where to continue

Last updated: 2026-09-30 (night).

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
0. Bugs reported by the user 2026-10-01 (next):
   - show ethernet-switching table is empty although addresses are learned; show arp shows too few entries.
   - show lacp interfaces: check the table (4/0/3 appears as Actor and Partner rows: that is the Junos layout, but
     verify the merge of MC-LAG bundles and kernel/config port names).
   - sw1 still has the old switchd.service (no ExecStopPost for cme); the unit is not part of the package: switchd
     should install/refresh its own unit (or the package carries it).
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
