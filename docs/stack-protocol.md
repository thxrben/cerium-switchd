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
| 3 | 1 | type: 1 = HELLO, 2 = DATA, 3 = ACK, 4 = RESET, 5 = BFD |
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
* **Data**: the stream is cut into frames of at most link MTU − 24 bytes. The sender keeps up to the peer's window
  in flight. ACKs are cumulative; every DATA frame also carries an ACK. The receiver acknowledges at the latest after
  2 frames or 10 ms.
* **Loss**: retransmission timeout starts at 50 ms (stacking cables are short), adapts to the measured round trip
  (RFC 6298 style, minimum 10 ms, maximum 1 s), and doubles on each timeout. Three duplicate ACKs trigger a fast
  retransmit. Frames beyond the window or already acknowledged are dropped (duplicates are harmless).
* **Close**: RESET, or no frames for the BFD detection time.
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
