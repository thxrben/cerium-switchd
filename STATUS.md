# Status / where to continue

Last updated: 2026-09-29 (late).

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
  routing instances (system management-instance + routing-instances mgmt_junos, per-member irb addresses),
  data routing instances (virtual-router VRFs), protection filter (nftables) for data L3 addresses, show route.
  Old configs converted on read (internal/daemon/upgrade.go). physw4 is switched off (user), deploy to sw1 only.

## In progress: Phase 5 stacking
- Done: link protocol (docs/stack-protocol.md, internal/stack/link) with BFD-style liveness; AF_PACKET I/O
  (verified sw1 ens20 <-> sw3 ens19, stk-13); stack keys (internal/stack/pki: Ed25519, no expiry, join tokens).
- Spec: Junos VC syntax (request virtual-chassis vc-port set pic-slot <card> port <port>, show virtual-chassis),
  mastership switch + member remove (decommissioning).

## Next (in order)
1. Phase 5: stack manager in switchd (VC ports, show virtual-chassis [vc-port]; stacking ports: up, no IP,
   no bridge), join (tokens), topology + relay, Raft store (with leadership transfer), per-member apply,
   stack-wide show commands. Lab cabling: hSw2 uses stk-12 (sw1 ens19 / sw2 ens19) and TestHotplug uses
   stk-13 (sw1 ens20); move them before the stacking lab tests.
2. Open items from Phase 3/4: family inet dhcp, VLAN MTU filter (eBPF), kernel messages to syslog, OS takeover
   (4.15), card number lifecycle (PLAN Phase 4b), switchd's own DNS/NTP through mgmt_junos.

## Questions for the user (collected while they are away)
1. Should the management network be an opt-in *backup* path for stack sync (TLS-protected) when all stacking
   cables between two members are cut? Current design: no (stack traffic only on stacking ports, like Junos VC).
2. The protection filter on data L3 addresses is fixed (ping/ND/replies only). Do you want a Junos-like
   configurable filter (firewall filter on lo0) later, e.g. to allow SSH on a data irb deliberately?
3. The CLI SSH server (port 2222) still listens on all addresses; with management-instance the filter blocks
   it on data L3 addresses. Should it additionally listen *only* inside mgmt_junos (then it is unreachable
   through OS-managed NICs outside the instance, e.g. before the management port is moved)?

## Notes
- The dev machine is only for development: no network changes here; lab = Proxmox VMs (PLAN.md §11).
- Spec first: update docs/config-reference.md before implementing a feature.
