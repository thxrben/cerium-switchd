# Wireshark dissectors for switchd

`switchd.lua` decodes switchd's own protocols (docs/stack-protocol.md):

| Layer | Where | What you see |
|---|---|---|
| `swstack` | EtherType `0x88b5` on stacking ports | HELLO, DATA, ACK, RESET, BFD, PROBE / PROBE-REPLY (path MTU); epochs, sequence numbers, windows; retransmissions and gaps flagged; the session mode byte (`M` / `J`) and the TLS records of the byte stream, reassembled |
| TLS 1.3 | inside the stacking stream | decrypted with the key log (below) |
| `swmesh` | member sessions (ALPN `swstack/1`) | neighbour hello, LSAs (topology), OPEN / DATA / CREDIT / CLOSE / RESET with the service (`raft`, `ctl`, `cli`); the RPC services as JSON lines |
| `swjoin` | join sessions (ALPN `swjoin/1`) | the join exchange (JSON lines) |
| stack tunnels | VXLAN between `169.254.64.<member>` | an annotation: from member, to member, VNI |

LACP, VXLAN, ARP and everything else is decoded by Wireshark itself.

## Use

```
wireshark -X lua_script:tools/wireshark/switchd.lua capture.pcap
tshark    -X lua_script:tools/wireshark/switchd.lua -r capture.pcap
```

Or copy `switchd.lua` to Wireshark's personal plugin folder (`~/.local/lib/wireshark/plugins/`).

Capture on a switch (the frames are Ethernet frames on the stacking port):

```
tcpdump -i <stacking-port> -w stk.pcap ether proto 0x88b5 or udp port 4789 or udp port 4790

(The stack tunnels use UDP 4790 while VXLAN to remote VTEPs uses 4789. Wireshark takes 4790 for VXLAN-GPE: use
*Decode As… → UDP port 4790 → VXLAN* for those captures.)
```

Start the capture **before** the link comes up (restart switchd, or unplug and replug the cable): a stream that
starts mid-capture cannot be aligned to TLS records and is only annotated.

## Decrypting TLS

The stacking TLS sessions are protected; to read the mesh messages, let switchd write the session keys (for
debugging only, the file lets anyone decrypt the capture):

```
mkdir -p /etc/systemd/system/switchd.service.d
printf '[Service]\nEnvironment=SWITCHD_TLS_KEYLOG=/tmp/switchd-keys.log\n' > /etc/systemd/system/switchd.service.d/keylog.conf
systemctl daemon-reload && systemctl restart switchd
```

Then give Wireshark the file (Preferences → Protocols → TLS → (Pre)-Master-Secret log filename, or
`-o tls.keylog_file:switchd-keys.log`). Remove the drop-in afterwards. switchd logs a warning while key logging
is on. Both members may write their own file; one member's file is enough for a capture on its cable.

The Raft stream (`raft`) is shown as its stream data (msgpack); the `ctl` and `cli` streams as JSON.
