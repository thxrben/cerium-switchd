# Review: Juniper "Virtual Chassis Technology Best Practices" vs. cerOS

Source: `docs/VC-Bestpractice-guide.pdf` (Juniper, EX4200, 2011, 29 pages), read in full on 2026-10-01. Licensing
and ordering are left out, as the user asked. This lists where cerOS (reference 5.2, 1.8, 3.6) **differs** from the
guide. "Intended" means the difference is a decision already in the spec; "gap" means cerOS lacks something the guide
describes; "decide" means it needs the user's call.

## Matches (no action)
- One logical device, one configuration, one management address (VME ↔ `cme`, 1.8), configuration mode on the
  master, consoles on any member reach the master (guide p. 17 note).
- Roles master / backup / linecard (p. 6–7). `show virtual-chassis` shows role, priority, status.
- Mastership priority, default 128 (p. 8). Recommendation "same priority for master and backup, no preemption"
  is the cerOS default behaviour anyway: a working master is never preempted (5.2).
- Members keep their id across reboots (p. 9).
- Interface names `member/pic/port` (p. 6).
- VC ports on uplinks (extended VC, p. 13): any port can be a stacking port (`request virtual-chassis vc-port set`,
  including the Junos `pic-slot … port …` form).
- Ring cabling recommended (p. 12); shortest-path forwarding inside the VC (p. 10, "Forwarding Path").
- LAG across members (p. 14): MC-LAG bundles (5.6).
- Software upgrade of all members from the master (p. 19), and of a single member (`member <id>`, p. 10).
- Software compatibility check when a member joins (p. 10).

## Differences

| # | Topic | Guide | cerOS today | Kind |
|---|---|---|---|---|
| 1 | Member id range | 0–9, standalone switch is member 0 | 1–16, standalone is member 1 | intended (5.2 says so) |
| 2 | Members per VC | up to 10 | up to 16 | intended |
| 3 | Election tie-breakers (p. 8) | priority → last master → member count in a merge → longest uptime (≥ 1 min apart) → lowest MAC | Raft election, then handoff to the highest priority, ties: **lowest member id**; no "last master", no "longest member", no MAC | decide: the Raft leader is whoever wins the vote; after that we hand over by priority/member id. Adding "longest in the VC" as a tie-breaker is possible; "last master" is implicit (no preemption) |
| 4 | Backup role | the backup is a hot standby (GRES: kernel and forwarding state synchronised; the PFEs keep forwarding on switchover) | every member has the Raft log and forwards on its own; "backup" is only the next-priority voter. A master failure never stops forwarding (stronger than non-GRES Junos) | intended, but **document** that GRES is implicit/always on |
| 5 | Line card behaviour (p. 7) | line cards run no control protocols; the master computes forwarding and programs the PFEs | each member runs its own data plane from the replicated configuration; LACP/LLDP/RSTP run per member (RSTP has one owner) | intended (distributed design) |
| 6 | Member id assignment (p. 9–10) | the master assigns the next lowest free id; ids of removed members are **not** reused until `request virtual-chassis recycle member-id <id>` | the operator picks the id: `request virtual-chassis member add <id>` issues a token bound to that id, and `join token <t>` on the new switch takes it; any free id can be chosen, also a removed member's | different, intended (explicit ids instead of automatic ones). `recycle` is unnecessary as long as ids are always chosen; **decide** whether `member add` without an id should pick the lowest free id (Junos-like) |
| 7 | `request virtual-chassis renumber member-id <old> new-member-id <new>` (p. 10, 19) | renumbering a member (e.g. a replacement takes over the old id and so the old member's configuration) | not available; a replacement joins with `member add <old id>` | **gap**: the replacement workflow (p. 18–19) works only by choosing the id at join time; add `renumber` (rewrites the member's interfaces like leave.go does) |
| 8 | Replacing a switch (p. 18) | same switch (same MAC) gets its old id and config back automatically | a re-installed switch must join again with a token (new keys) | decide: token required on purpose (security); document the replacement procedure |
| 9 | Pre-provisioning (p. 16–17) | `virtual-chassis preprovisioned`, `member <id> serial-number <sn> role routing-engine\|line-card`: only listed switches can join, roles fixed (priority 129 for RE, 0 for LC) | joining needs a one-time token; roles follow `mastership-priority`; no serial numbers | decided (user, 2026-10-01): not needed; join tokens bound to a member id cover it |
| 10 | Split detection (p. 15) | after a split only one part stays active (the other parts become inactive, line cards only); `no-split-detection` for two-member VCs | majority rule: the minority part keeps forwarding but cannot commit; for 3+ members MC-LAG legs of the minority are held; a two-member split forwards on both sides; `force-master` overrides | intended, partially different: the minority still forwards single-homed ports (Junos would stop them). **decide** whether a `split-detection` knob is wanted |
| 11 | Automatic software update of joining members (p. 10) | `set virtual-chassis auto-sw-update package-name <path>`: a joining member with another version is upgraded automatically; until then it gets an id but forwards nothing | a joining member with another version joins and works (mixed versions, 3.6); `request system software add … member <id>` updates it by hand | **gap**: `virtual-chassis auto-sw-update` (now easy: the master holds the bundle; the member reboots into it) |
| 12 | Software of a member with a different version (p. 10) | "does not become a functional member and does not forward data" | forwards; statements it does not know are left out (3.6 mixed versions) | intended (rolling upgrades without loss) |
| 13 | `commit synchronize` (p. 19, 27) | needed so the backup gets the config | every commit is replicated by Raft to all members | intended; accept `commit synchronize` as an alias of `commit` (Junos muscle memory) — **small gap** |
| 14 | `show virtual-chassis status` | shows serial number, model, neighbour list per member | `show virtual-chassis` has id, host name, role, priority, status; neighbours are in `show virtual-chassis vc-port` | **gap**: add serial number and model (DMI) and the neighbour list |
| 15 | `show virtual-chassis vc-port statistics [member <id>]` (p. 21) | per VC-port traffic counters | not available | **gap** |
| 16 | Factory default (p. 20) | `load factory-default` loads a default configuration: Ethernet switching on all ports, LLDP and RSTP on, syslog and commit settings | factory default = empty configuration, all ports down (safety: no loops before configuration); `request system zeroize` erases everything | intended (ports down until configured); **decide** whether `load factory-default` should exist (e.g. as the empty configuration plus management defaults) |
| 17 | Root authentication | `set system root-authentication plain-password` (p. 15) | added today: `system root-authentication { encrypted-password; ssh-key }`; `plain-text-password` in the CLI | matches (`plain-text-password` works below `system root-authentication` as for users; tested) |
| 18 | `request system software add <pkg> reboot` | `reboot` keyword | always reboots (image) | accept the `reboot` keyword as a no-op — **small gap** |
| 19 | Master/backup placement advice (p. 11) | "evenly spaced by member hop" | not checked | possible: a commit warning when master and backup (top-2 priorities) are neighbours in a ring of 4+ — optional |
| 20 | VC port LAG (p. 6) | several VC ports between the same two members form a LAG | parallel stacking links are used together (ECMP over equal paths, 5.2) | matches in effect |
| 21 | VCP cable length / dedicated VCP ports | hardware specific | n/a (any NIC) | n/a |

## Suggested order
1. Small aliases: `commit synchronize`, `request system software add … reboot` (13, 18).
2. `show virtual-chassis` with serial number, model and neighbours; `vc-port statistics` (14, 15).
3. `request virtual-chassis renumber` and `recycle` (6, 7) — needed for the replacement workflow.
4. `virtual-chassis auto-sw-update` (11).
5. The user decided (2026-10-01) to keep the remaining differences; pre-provisioning (9) is not needed.
