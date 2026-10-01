#!/bin/bash
# Shared helpers of the interoperability tests: switchd's routing protocols
# (cmd/rtest) against FRR in docker containers on private docker networks.
# Nothing touches the host's network configuration.
set -euo pipefail
cd "$(dirname "$0")/../.."
FRR_IMAGE=${FRR_IMAGE:-quay.io/frrouting/frr:10.1.1}
PREFIX=ceros-interop
WORK=build/interop
mkdir -p "$WORK"

say() { echo "=== $*"; }
fail() { echo "FAIL: $*"; dump; exit 1; }

build_rtest() {
  CGO_ENABLED=0 go build -o "$WORK/rtest" ./cmd/rtest
}

# net <name> <subnet4> [<subnet6>]
net() {
  docker network rm "$PREFIX-$1" >/dev/null 2>&1 || true
  if [ -n "${3:-}" ]; then
    docker network create --ipv6 --subnet "$2" --subnet "$3" "$PREFIX-$1" >/dev/null
  else
    docker network create --subnet "$2" "$PREFIX-$1" >/dev/null
  fi
}

# frr <name> <net> <ip4> [<ip6>]  (more networks: connect)
frr() {
  local name=$1 net=$2 ip=$3 ip6=${4:-}
  docker rm -f "$PREFIX-$name" >/dev/null 2>&1 || true
  local args=(--ip "$ip")
  [ -n "$ip6" ] && args+=(--ip6 "$ip6")
  docker create --name "$PREFIX-$name" --hostname "$name" --privileged --network "$PREFIX-$net" "${args[@]}" \
    -v "$PWD/$WORK/$name:/etc/frr" "$FRR_IMAGE" >/dev/null
}

# rt <name> <net> <ip4> [<ip6>]: a container running rtest
rt() {
  local name=$1 net=$2 ip=$3 ip6=${4:-}
  docker rm -f "$PREFIX-$name" >/dev/null 2>&1 || true
  local args=(--ip "$ip")
  [ -n "$ip6" ] && args+=(--ip6 "$ip6")
  docker create --name "$PREFIX-$name" --hostname "$name" --privileged --network "$PREFIX-$net" "${args[@]}" \
    -v "$PWD/$WORK:/w" --entrypoint /w/rtest "$FRR_IMAGE" -c "/w/$name.json" -status "/w/$name-status.json" >/dev/null
}

connect() { docker network connect --ip "$3" "$PREFIX-$2" "$PREFIX-$1"; }
start() { for c in "$@"; do docker start "$PREFIX-$c" >/dev/null; done; }
vtysh() { local c=$1; shift; docker exec "$PREFIX-$c" vtysh -c "$*"; }

# frr_conf <name> <daemons...>: writes daemons; frr.conf comes from stdin
frr_conf() {
  local name=$1; shift
  mkdir -p "$WORK/$name"
  {
    for d in zebra bgpd ospfd ospf6d bfdd staticd; do
      if [[ " $* " == *" $d "* ]]; then echo "$d=yes"; else echo "$d=no"; fi
    done
    echo 'vtysh_enable=yes'
    echo 'zebra_options="  -A 127.0.0.1 -s 90000000"'
  } > "$WORK/$name/daemons"
  cat > "$WORK/$name/frr.conf"
  touch "$WORK/$name/vtysh.conf"
  chmod -R a+rwX "$WORK/$name"
}

# wait_for <seconds> <description> <command...>
wait_for() {
  local secs=$1 what=$2; shift 2
  for _ in $(seq "$secs"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timeout: $what"
}

status() { cat "$WORK/$1-status.json" 2>/dev/null; }

dump() {
  for c in $(docker ps -a --filter "name=$PREFIX-" --format '{{.Names}}'); do
    echo "--- $c (last log lines)"
    docker logs --tail 30 "$c" 2>&1 | sed 's/^/  /'
  done
}

cleanup() {
  docker ps -a --filter "name=$PREFIX-" --format '{{.Names}}' | xargs -r docker rm -f >/dev/null
  docker network ls --filter "name=$PREFIX-" --format '{{.Name}}' | xargs -r docker network rm >/dev/null
}
