# Handoff (2026-10-05)

Branch `claude/relaxed-cori-qe4lur`, everything committed and pushed. Details: STATUS.md (sections of 2026-10-04 late
and 2026-10-05), PLAN.md (15b, 9c "Graceful restart", 10.7/10.8, 17.4, 18, Phase 12 plan, "Item 13").

## Lab
- The user switched the lab off on 2026-10-04 night. sw2/sw3 (firmware) are mine to test when it is back; sw1 + physw4
  are the user's stack: never deploy there unless asked.
- Waiting for the lab (in this order): deploy the current build to sw2/sw3 (check-config on the lab config first);
  memory slots, swap off, `request system reload` (ports come back), stacking MACsec `mode on`; the relay/host-key
  fixes; USB storage with a virtual USB disk; two parallel stacking cables; the lab harness rework (it still
  addresses members directly); OSPF/BGP/BFD interop with FRR (incl. graceful restart both ways).

## Done 2026-10-04 late / 2026-10-05 (unit-tested, not on a device)
Alarms name their member; MACsec status and multicast memberships through netlink (no `ip -j`/`bridge -j`); parallel
stacking cables documented; USB storage (system disk excluded); BGP to cer-ribd/members as deltas (no table copy;
slot costs lowered); memory setup uses the smallest member; MACsec offload on client ports and on bundle members;
OSPF graceful restart (helper + restarting); REST API 18.1-18.3 (CLI, config sessions, state, events, health,
metrics, tokens, OpenAPI).

## Open, waiting for the user's decisions (PLAN "Phase 12" plan and "Item 13")
802.1X (RADIUS only? dynamic VLAN in multiple mode?), modular code base (when?), arm64 (which hardware?), Secure Boot
(own keys in db, or shim + MOK?), delta updates (block size after measuring reproducible builds).

## Open without decisions
cer-ribd installs the whole active table on every change (PLAN 15b.4); OSPF GR across a mastership change (9c GR 3);
REST API link-change events and `member=`; remaining tool-output readers (dataplane/mcast_linux.go `bridge -j vlan`,
`ip -j link`; vlanmtu `nft -j`); Tab completion of `usb:` paths; `software add usb:` from a non-master member.
