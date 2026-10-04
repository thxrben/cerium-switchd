# Handoff (2026-10-04, paused for LACP/LAG debugging on physw4 + UniFi)

Branch `claude/relaxed-cori-qe4lur`, everything committed and pushed. Details: STATUS.md section
"Requested 2026-10-04", PLAN.md phases 10, 10b, 14-17, 17b.

## Where it stands
- Done, unit-tested, NOT on the lab yet: no swap (14), RAM-only bundles + REST upload (17.1-3), one update at a
  time (17b), memory slots incl. OSPF/BGP enforcement and smaller routes (15), `request system reload` (16),
  `show system limits` used/available fix, MACsec on stacking links (10, stack-wide on by default - to be changed).
- Open, in this order:
  1. **MACsec rework per PLAN 10b**: off by default; auto-on per stacking link only when both ends offload;
     optional software MACsec; per-link setting, mixed links between the same members; per-link MTU overhead;
     migration to a new offloading VC port. Spec first (reference 5.2), then stackmacsec.go/model/stacknet.
  2. **Lab deploy + tests** (the user allowed free deploys; never touch physw4). The lab VMs run the old
     single-binary install (/usr/local/sbin/switchd + swcli symlink, 05240a8). Deploy: `make build`, tar bin/,
     copy to the master (`ssh root@10.5.176.95`), from there to sw1/sw3 with agent forwarding
     (`ssh -A`, then `ip vrf exec swstack scp/ssh root@169.254.64.1` = sw1, `169.254.64.3` = sw3), install every
     program into /usr/local/sbin, `systemctl restart switchd`; order sw3, sw1, then the master sw2. Keep
     /usr/local/sbin/switchd.05240a8 for rollback. Check the lab config with `go run ./cmd/switchd check-config`
     first (it passed for 51b37de). Test: daemon split, swap off (lab has /dev/sda5 swap), memory slots, reload
     (ports must come up again), stacking MACsec (needs root: real key install), REST upload.
  3. MACsec on client ports (data plane moves bridge/L3/RSTP/MC-LAG/mac-limit/storm of a secured port to the
     macsec device wpa_supplicant creates after MKA; MACsec per member port of a LAG, not on ae).
  4. USB storage (17.4), multicast count in show system memory, cer-bgpd full-table JSON transient (delta).
