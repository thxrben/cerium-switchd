#!/bin/bash
# BFD interop: switchd's BFD (rtest) against FRR bfdd, single-hop IPv4 and IPv6.
source "$(dirname "$0")/lib.sh"
trap cleanup EXIT
cleanup
build_rtest

net bfd 10.250.1.0/24 fd00:250:1::/64
frr_conf frr zebra bfdd <<'EOF'
bfd
 peer 10.250.1.12
  receive-interval 100
  transmit-interval 100
  detect-multiplier 3
 !
 peer fd00:250:1::12
  receive-interval 100
  transmit-interval 100
 !
!
EOF
cat > "$WORK/me.json" <<'EOF'
{"bfd": [{"peer": "10.250.1.11", "interval_ms": 100, "multiplier": 3},
         {"peer": "fd00:250:1::11", "interval_ms": 100, "multiplier": 3}]}
EOF
frr frr bfd 10.250.1.11 fd00:250:1::11
rt me bfd 10.250.1.12 fd00:250:1::12
start frr me

say "sessions come up (IPv4 and IPv6)"
up() { [ "$(status me | grep -c '"State": 3')" = 2 ]; }
wait_for 30 "both BFD sessions up on our side" up
wait_for 10 "FRR sees 10.250.1.12 up" sh -c "docker exec $PREFIX-frr vtysh -c 'show bfd peer 10.250.1.12' | grep -q 'Status: up'"
wait_for 10 "FRR sees fd00:250:1::12 up" sh -c "docker exec $PREFIX-frr vtysh -c 'show bfd peer fd00:250:1::12' | grep -q 'Status: up'"
vtysh frr "show bfd peer 10.250.1.12" | grep -E "Status|Remote timers|Local timers" -A3 | head -12

say "the detection time follows the negotiated timers (300 ms)"
status me | grep -q '"Detection": 300000000' || fail "detection time: $(status me)"

say "FRR stops: our side detects it"
docker pause "$PREFIX-frr" >/dev/null
t0=$(date +%s%N)
down() { [ "$(status me | grep -c '"State": 3')" = 0 ]; }
wait_for 10 "sessions down after FRR stopped" down
docker unpause "$PREFIX-frr" >/dev/null
echo "detected within $(( ($(date +%s%N) - t0) / 1000000 )) ms (status file refreshes every second)"
status me | grep -q '"Diag": 1' || fail "diag is not 'time expired': $(status me)"
wait_for 30 "sessions up again" up

say "all BFD interop tests passed"
