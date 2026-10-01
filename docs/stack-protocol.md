# Stacking link protocol

Internal wire format of the stacking plane (config reference 5.2, PLAN.md §3). Every stacking cable carries one
**link** between two members. A link is a reliable, ordered byte stream (like TCP, without IP) exposed to the rest of
switchd as a `net.Conn`; TLS 1.3 with mutual authentication runs on top of it. Nothing here is routed or bridged.

## Frames

Untagged Ethernet frames, EtherType `0x88b5` (IEEE local experimental), destination = the peer's MAC once known,
else the broadcast address. Frames are never sent on, or accepted from, any port that is not a designated stacking port.

Payload (all integers big-endian):

| Offset | Size | Field |
|---|---|---|
| 0 | 2 | magic `0x5354` ("ST") |
| 2 | 1 | version (1) |
| 3 | 1 | type: 1 = HELLO, 2 = DATA, 3 = ACK, 4 = RESET, 5 = BFD, 6 = PROBE, 7 = PROBE-REPLY |
| 4 | 4 | sender epoch (random per link instance; changes when a side restarts) |
| 8 | 4 | receiver epoch as last seen (0 = unknown) |
| 12 | 4 | sequence number of the first payload byte (DATA) |
| 16 | 4 | cumulative acknowledgement: next byte expected from the peer |
| 20 | 2 | receive window in bytes / 64 |
| 22 | 2 | payload length |
| 24 | n | payload (DATA; BFD control packet) |

* **Handshake**: both sides send HELLO (with their epoch) every 100 ms until they have seen the peer's epoch echoed.
  Then the link is up. Sequence numbers start at 0 for each epoch pair.
* **Restart detection**: a frame whose receiver epoch is neither 0 nor the own epoch, or whose sender epoch changes,
  means the peer restarted: the stream is closed (the TLS session with it), and a new one starts with the new epochs.
* **Data**: the stream is cut into frames of at most **1500** − 24 bytes, **whatever the port's MTU is**: the stacking
  protocol must work over every cable, including paths that cannot carry jumbo frames (a bridge or converter in
  between, a switch port with a smaller MTU). Only the client traffic in the stack tunnels uses larger frames. The sender keeps up to the peer's window
  in flight. ACKs are cumulative; every DATA frame also carries an ACK. The receiver acknowledges at the latest after
  2 frames or 10 ms.
* **Loss**: retransmission timeout starts at 50 ms (stacking cables are short), adapts to the measured round trip
  (RFC 6298 style, minimum 10 ms, maximum 1 s), and doubles on each timeout. Three duplicate ACKs trigger a fast
  retransmit. Frames beyond the window or already acknowledged are dropped (duplicates are harmless).
* **Liveness (BFD)**: when a side has sent nothing for its interval, it sends an ACK frame. HELLO payload carries
  the sender's interval and multiplier (2 bytes each, big-endian, milliseconds / count); both sides use the larger
  interval and multiplier. No frame for interval × multiplier ends the link.
* **Close**: RESET, or the liveness timeout.
* **Path MTU** (PROBE / PROBE-REPLY): once the link is up, and every 5 s, each side sends one PROBE per candidate size
  above 1500: the port's MTU, and 9000, 4000 and 2000 if they are smaller. A PROBE is a frame padded to exactly that
  Ethernet payload size; the receiver answers with a small PROBE-REPLY carrying the size it received. The largest
  size answered within 500 ms is the **path MTU** of the cable (at least 1500 once known). It is shown by
  `show virtual-chassis mtu` (column Verified), and a warning is logged when it is smaller than the largest data
  `mtu` + 58 needs (config reference 5.2). Probes do not carry stream data and are not retransmitted.
* A link never delivers bytes twice or out of order, and never delivers bytes across an epoch change.

## Keys and joining

Minimal on purpose (nothing expires, nothing depends on the clock):

* **Stack key**: an Ed25519 key pair created by the first member. Every member keeps a copy (it is replicated with
  the configuration over TLS), so any leader can admit new members. Its self-signed certificate is the trust anchor.
* **Member key**: every switch creates its own Ed25519 key pair at first start. When it joins, the stack key signs a
  certificate for it: subject `member-<id>`, the member's public key.
* Certificates are valid from 2000-01-01 to 9999-12-31 23:59:59 UTC (RFC 5280's "no expiry" date). Verification uses
  the current time clamped into that range, so a switch whose clock is wrong (no battery-backed clock) still works.
  There is no renewal and no revocation list.
* **Who may talk**: after the TLS handshake (TLS 1.3, both sides present certificates signed by the stack key) the
  peer's member id and public key must match the replicated member list. A removed member fails this check.
* **Join tokens**: `request virtual-chassis member add <id> token` on the stack prints a one-time token (128 bits,
  valid 1 hour). On the new switch, `request virtual-chassis join token <t>` starts the join over its stacking ports:
  1. TLS 1.3 with the new switch's self-signed member certificate; the stack side presents its stack certificate.
     The new switch cannot verify the stack yet, and the stack does not trust the new switch yet.
  2. The new switch sends `HMAC-SHA256(token, "join" | member-public-key | stack-public-key-as-seen)`. The stack checks
     it with the same token and the keys it saw in the handshake, so the token proves both sides saw the same keys
     (no man in the middle) without ever being sent.
  3. The stack answers with the member certificate, the stack key and certificate, the configuration and the
     member id, plus `HMAC-SHA256(token, "admit" | …)` so the new switch knows it talks to the stack that issued the token.
  4. The token is used up. The member reconnects with its signed certificate.

## Sessions

When a link is up, each side first writes one byte: `M` (member session) or `J` (this switch is joining with a token).
* `M`/`M`: TLS 1.3 between members; the side with the lower MAC address is the TLS client. Then each side sends a
  hello (JSON line: member id, host name, local port). Stack messages follow on the same connection.
* `J`/`M`: the joining side is the TLS client with its self-signed certificate; the stack side presents the stack
  certificate. The join exchange of "Keys and joining" follows (JSON lines). Afterwards the link is closed; the new
  member reconnects with `M` once switchd has restarted with its new identity.
* A switch that joins gets the stack's current configuration with the answer (continuous replication follows with
  Raft). Its own previous configuration is kept in `/var/lib/switchd/config.pre-join-<time>`; switchd restarts to take
  the new member id (interface names change from `1/…` to `<id>/…`).

## Mesh (topology, relay, streams)

After the hellos, a member session carries **mesh messages**, each `uint32` length-prefixed (big-endian) and then:

| Size | Field |
|---|---|
| 1 | type: 1 = LSA, 2 = OPEN, 3 = DATA, 4 = CREDIT, 5 = CLOSE, 6 = RESET |
| 1 | hop limit (starts at 16; a message reaching 0 is dropped) |
| 1 | source member |
| 1 | destination member (0 for LSA: not forwarded as such, flooded) |
| 4 | stream id (chosen by the opening side; odd = opened by the lower member id) |
| 4 | sequence number (DATA: per stream and direction, from 0; CREDIT: bytes granted) |
| n | payload |

* **LSA** (link-state announcement): payload = sequence number (8 bytes), then the member ids of the origin's current
  neighbours (one byte each). Sent on every neighbour change and every 5 s; a member re-floods an LSA it has not seen
  (higher sequence number for that origin) to all its other sessions. LSAs older than 20 s are dropped. Paths are
  the shortest in hops (ties: lower next-hop member id), recomputed on every change.
* **Streams**: OPEN carries a service name (e.g. `raft`); the destination answers with CREDIT (initial window 256 KiB)
  or RESET (no such service). DATA is only sent within the granted credit; the receiver grants more as the
  application reads. A DATA message with an unexpected sequence number (lost on a failed path) resets the stream.
  CLOSE ends the sending direction; RESET ends the stream at once.
* Messages for a destination without a path are dropped (streams to it are reset); nothing is buffered for members
  that are gone.
* When the path to a member changes, streams to it are reset as well: messages may have been lost on the old path,
  and a stream that waits for an answer would not notice. Their users (Raft, RPC) reconnect over the new path.

## Stack tunnels (data between members)

Client frames between members do not use the stacking protocol: they travel in VXLAN over an internal IPv4 network
on the stacking ports (config reference 5.2). All of it is fixed; nothing is configurable.

* **Instance**: VRF device `swstack`, routing table 999. Its only members are the stacking ports (IPv6 stays
  disabled on them; IPv4 forwarding is on for them only, reverse-path filtering off). The member's address
  `169.254.64.<member>/32` sits on the VRF device.
* **Neighbours**: no ARP. For every stacking link whose session is up (member session, not "other stack"), switchd
  adds a permanent neighbour entry `169.254.64.<neighbour>` → the MAC the link learned from the neighbour's frames,
  on that port, and removes it when the session ends.
* **Routes** (protocol 250, table 999): for every reachable member `m`, `169.254.64.<m>/32` with one next hop per up
  link to each neighbour that lies on a shortest path to `m` (hops of the mesh topology, all equal-cost first hops,
  so two parallel cables are both used), each `via 169.254.64.<neighbour> dev <port> onlink`. Recomputed with the
  mesh topology; a route whose member becomes unreachable is removed. An `unreachable` default route (metric 1000000) makes lookups for an unreachable member fail inside the instance instead of falling through to the main
  table (which would send tunnel packets out of the management port).
* **Tunnels**: per other switch member `m`, the VXLAN device `swvc<m>`: VNI `32 × min(self, m) + max(self, m)`,
  local `169.254.64.<self>`, remote `169.254.64.<m>`, UDP 4789 (4790 while VXLAN to remote VTEPs uses 4789, reference 5.7), lower device `swstack`, TTL 16, outer DF set,
  no VXLAN learning, MTU = the stack MTU minus 58 (config reference 5.2). The device is a port of `swbr0`: `isolated`
  (never forwards to another tunnel), VLANs tagged, no PVID; learning on except towards the MC-LAG peer.
* **Stacking port MTU**: the NIC maximum, at most 16044 (Linux MTU; frame 16058), set when the port is designated.

## Stack control (Raft)

Every member runs Raft (hashicorp/raft) over mesh streams (service `raft`); the Raft server id and address of a member
are `member-<id>`. The Raft leader is the **master**.

* **Replicated state** (the Raft state machine): the configuration revisions (the last 50), the pending confirmation,
  the shared candidate, the member list (id → public key) and open join tokens (id, token, expiry).
  * Each member keeps the state in its local configuration store (`/var/lib/switchd/config`), written by the state
    machine. Reads are always local: a member without master or majority still starts with, and forwards with, the
    last configuration it knows.
  * The state machine records the Raft index it applied last; entries at or below it (replayed after a restart) and
    older snapshots are ignored, so the local store never goes back.
  * Writes (commits, the pending state, the shared candidate, members, tokens) are Raft entries. A member that is not
    the master forwards them to the master (mesh service `ctl`).
* **Bootstrap**: a switch that created its stack (it never joined another stack) and has no Raft state forms a
  one-voter Raft cluster. Its first entry (`load`) carries its existing configuration history, the member list and
  the cluster id into Raft. A member that joined waits until the master adds it.
* **Voters**: up to 7 members are voters, chosen by `mastership-priority` (higher first, ties: lower id); the others
  are non-voters (they get the state, but do not vote). The master adjusts the voter set when members join or leave,
  or priorities change.
* **Election**: Raft elects the master. A newly elected master hands mastership to a reachable, up-to-date voter
  with a higher `mastership-priority` if there is one; a master with priority 0 always hands it on. Afterwards the
  master stays until it fails or is switched explicitly (no preemption when a higher-priority member returns).
* **Member list**: stacking sessions and mesh streams are accepted only from members in the list (with their key).
  A switch that has not yet received the state accepts any member certificate of the stack.
* **Removal**: the master moves mastership away first if needed, removes the member from the list, then from the Raft
  configuration. A member that sees itself removed from the list creates a new stack of its own (new keys, same member
  id, its last configuration) and restarts switchd.

## Commits in a stack

* **Configuration mode runs on the master.** `configure` on another member relays the configuration session to the
  master over a mesh stream (service `cli`, first line: user, class, originating member); everything until the
  session leaves configuration mode goes there. Operational commands run on the member the user is logged in to.
  Without a reachable master, `configure` fails with a message (operational commands keep working).
* When mastership moves, configuration sessions on the old master end with a notice; the shared candidate is kept
  (it is replicated), private candidates and exclusive locks are lost.
* **Apply**: the master applies a commit on every reachable member in parallel (mesh service `ctl`, full
  configuration, timeout 120 s) and on itself, and prints the result per member. If a member fails, all members
  return to the previous configuration and the commit fails. Unreachable members are reported as pending.
* **Catch-up**: a member compares the configuration it applied with the replicated active configuration whenever
  the replicated state changes, and every 10 s. If they differ and the master has not sent an apply for 60 s (a
  commit in progress), it applies the replicated configuration. This covers members that were unreachable during a
  commit and a master that failed in the middle of one.
* The confirmation timer runs on the master; a new master re-arms it from the replicated pending state (and rolls
  back at once if the deadline has passed).
