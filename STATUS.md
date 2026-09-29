# Status / where to continue

Last updated: 2026-09-29.

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
- Property test verified by mutation (deleting VLANs after adding them is caught as a leak).

## Next (in order)
1. VLAN MTU filter, flow control (ethtool ioctl), storm-drop counters in show interfaces; hardware-offload watchdog.
2. Management plane (VRF mgmt, IRB-like VLAN interface, static/DHCP), syslog.
3. `set … authentication plain-text-password`; `start shell`; swcli as login shell (Phase 4).

## Notes
- The dev machine is only for development: no network changes here; lab = Proxmox VMs (PLAN.md §11).
- Spec first: update docs/config-reference.md before implementing a feature.
