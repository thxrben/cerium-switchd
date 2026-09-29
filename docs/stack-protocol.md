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
