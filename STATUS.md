# Status / where to continue

Last updated: 2026-09-29.

## Done
- Phase 1.1–1.3: schema, config tree, set/curly/JSON formats, diff (tested + fuzzed).
- Commit-check validation (`internal/model`), interface-range, Junos MTU, three-plane stacking design.
- Spec: `docs/config-reference.md` (tests keep index, prose coverage and examples valid).
- Directives: `inactive:` / activate / deactivate (all formats, diff `!` lines, `Tree.Active()` used by
  `model.Build`), `load merge|replace|override|set` (atomic, `replace:`/`delete:`), `copy`, `rename`.
- Plan §4.14: hitless reconfiguration (diff-driven, no link down, tighten-before-loosen, planner property tests).

## Next (in order)
1. `internal/version` package (Makefile LDFLAGS already reference it).
2. Commit engine (`internal/commit`): revision store (50 revisions, file store first), candidates
   shared/private/exclusive with locks, commit check, commit / comment / and-quit, confirmation
   (mode required|optional, timer restart, rollback target = last confirmed, persisted pending state,
   stricter-policy rule, automatic-rollback revision), rollback n, apply pipeline interface with
   per-member results and revert on failure (validate → plan → apply, §4.14).
3. CLI engine (server side): parsing, completion, `?`, pipes, permissions, panic recovery.
4. JSON-lines RPC over a unix socket (SO_PEERCRED), `swcli` client, `switchd --dry-run` → Checkpoint A.

## Notes
- The dev machine is only for development: no network changes here; lab = Proxmox VMs (PLAN.md §11).
- Spec first: update docs/config-reference.md before implementing a feature.
