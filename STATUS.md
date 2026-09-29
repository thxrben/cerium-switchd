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

## Next (in order) — see PLAN.md Phase 4b
1. NIC capability checks at commit (ethtool link modes, pause, offloads; per-port speed class), show system offload.
2. Then Phase 5 stacking (PKI, stacking transport, topology, Raft, per-member apply).

## Notes
- The dev machine is only for development: no network changes here; lab = Proxmox VMs (PLAN.md §11).
- Spec first: update docs/config-reference.md before implementing a feature.
