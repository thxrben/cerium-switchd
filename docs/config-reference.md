# cerOS Configuration Reference

This document is the **specification** of the switch configuration: the syntax, what every statement does,
how statements interact, and which combinations `commit check` rejects (**error**) or reports (**warning**).
The code follows this document. Where they disagree, that is a bug.

The style follows Junos. It is not Junos-compatible: statements, defaults and some semantics differ, and the
differences are called out where they are likely to surprise.

Contents

1. [Conventions](#1-conventions)
2. [Configuration formats](#2-configuration-formats)
3. [Editing, directives and loading](#3-editing-directives-and-loading)
4. [Commit model](#4-commit-model)
5. [Statement reference](#5-statement-reference)
   - [5.1 system](#51-system)
   - [5.2 virtual-chassis](#52-virtual-chassis)
   - [5.3 interfaces](#53-interfaces)
   - [5.4 vlans](#54-vlans)
   - [5.5 protocols](#55-protocols)
   - [5.6 mclag](#56-mclag)
   - [5.7 switch-options](#57-switch-options)
   - [5.8 routing-options](#58-routing-options)
   - [5.9 routing-instances](#59-routing-instances)
   - [5.10 forwarding-options](#510-forwarding-options)
   - [5.11 policy-options](#511-policy-options)
   - [5.12 BFD](#512-bfd)
   - [5.13 protocols ospf, protocols ospf3](#513-protocols-ospf-protocols-ospf3)
   - [5.14 protocols bgp](#514-protocols-bgp)
6. [Frame handling summary](#6-frame-handling-summary)
7. [Complete examples](#7-complete-examples)
8. [Implementation status](#8-implementation-status)
9. [Statement index (generated)](#9-statement-index-generated)

---

## 1. Conventions

### 1.1 Notation

* `keyword`: literal text.
* `<value>`: a value you supply. Its type is described in 1.2.
* `[ a b c ]`: a set of values (see *leaf-list* below).
* `a | b`: exactly one of the alternatives.

Statement kinds:

| Kind | Example | Meaning |
|---|---|---|
| container | `system { … }` | Groups statements. It disappears automatically when its last child is deleted. |
| presence container | `protocols rstp;` | Its **existence** has meaning, even when empty (e.g. RSTP is enabled). |
| list | `interfaces 1/0/0 { … }` | Keyed entries. Entries are sorted naturally (`1/0/2` before `1/0/10`). |
| leaf | `mtu 9216;` | One value. Setting it again replaces the value. |
| leaf-list | `members [ 10 20 ];` | A set of values. `set` **adds** values (duplicates are ignored), `delete … <value>` removes one value, and `delete …` without a value removes all. The order of insertion is kept. |
| flag | `disable;` | Present or absent. |

**Mutually exclusive statements** (for example `lacp active` / `lacp passive`, or `management address` / `management dhcp`):
setting one silently removes the other.

**Abbreviations:** keywords may be abbreviated to any unique prefix (`sh int` → `show interfaces`,
`set int 1/0/0 unit 0 fam eth` → `family ethernet-switching`). Keyword enum values may also be abbreviated
(`interface-mode tr` → `trunk`). User-chosen names and numbers are never abbreviated.

### 1.2 Value types

| Type | Format | Notes |
|---|---|---|
| `<interface-name>` | `<member>/<card>/<port>`, `ae<N>` or `irb` | `1/0/3`, `2/1/0`, `ae0`…`ae4095`. Junos-style numbering without a `ge-`/`xe-` prefix: the member is the stack member id (1–16), card and port are numbered from 0 (1.6). `irb` holds the VLAN IP interfaces (5.3.3). |
| `<unit-name>` | `<interface-name>.<unit>` | A logical unit, e.g. `irb.10`, `1/0/6.100`. |
| `<vlan>` | VLAN name, id (`10`), range (`10-20`) or `all` | Where only one VLAN is allowed, ranges and `all` are rejected. |
| `<vlan-id>` | 1–4094 | |
| `<vni>` | 1–16777214 | |
| `<mtu>` | 256–16000 | See 1.3. |
| `<ip-address>` | IPv4 or IPv6 | Canonicalised (`2001:DB8::1` → `2001:db8::1`). Zones are not allowed. |
| `<address/prefix>` | `10.0.0.1/24`, `2001:db8::1/64` | |
| `<mac-address>` | `02:00:00:00:00:01` | Canonicalised to lower case. |
| `<hostname>` | RFC 1123 | |
| `<name>` | letter followed by letters, digits, `_ . -`; max 64 | Used for VLAN and analyzer names. |
| `<text>` | any printable text, max 255 bytes | Quote it if it contains spaces: `description "uplink to core"`. |

No value may contain control characters (including terminal escape sequences) or invalid UTF-8. This keeps
every configuration printable and re-loadable, and prevents escape-sequence injection into `show` output.

### 1.3 MTU semantics (Junos-style)

`mtu` is the **maximum frame size in bytes including the 14-byte Ethernet header**, excluding the FCS and
excluding 802.1Q tags. Each VLAN tag may add 4 bytes on top, so a trunk with `mtu 1514` carries tagged frames of 1518 bytes.

* The default is `mtu 1514`, a standard Ethernet frame with 1500 bytes of payload.
* The value on a server's NIC (the Linux MTU) is the payload only. A host with `mtu 9000` therefore needs switch ports
  with at least `mtu 9014`. `mtu 9216` is the usual "maximum jumbo" setting and fits hosts up to 9202.
* Internally, the kernel MTU of a port is `mtu − 14`. Hardware limits are reported in the same (frame) convention.
* The same convention applies to `vlans <v> mtu`.

### 1.4 Stack members and interface ownership

* Every switch is a **stack member** with an id from 1 to 16. A switch without any `virtual-chassis` configuration is member 1.
* Interface names carry the member id, so the whole stack is configured in one place.
* **switchd owns every network port of the switch.** There are no operating-system defaults: the OS network
  configuration (DHCP on an installer NIC, networkd/ifupdown/NetworkManager files) is disabled, and a blank switch has
  no management interface and no address at all. switchd does this itself when it starts: it masks the OS network
  services (`networking`, `systemd-networkd`, `NetworkManager`, `dhcpcd`; effective from the next boot, so nothing
  is taken down while it runs), ends running DHCP clients (`dhclient`, `dhcpcd`, `udhcpc`), and logs what it disabled.
  Their configuration files stay untouched. switchd also keeps its own systemd unit current (a software update can
  change it). The first access is the local console
  (serial or display, 5.1 `system ports`).
  * A port that is not configured (not under `interfaces`, not selected by an `interface-range`, not a stacking port)
    is administratively down, outside the bridge, without addresses and with IPv6 disabled. New NICs never start
    switching traffic unless a wildcard `interface-range` selects them on purpose (5.3.1).
  * An interface that is configured but not physically present (not plugged in, or on a member that has not joined yet)
    keeps its configuration. The configuration is applied as soon as the interface appears. `commit check` warns about this.
  * An interface that is **removed** from the configuration (or no longer selected by a range) is **released**: it is
    taken administratively down first, then removed from the bridge or bundle, and like every unconfigured port it
    carries no addresses and no description. It does not return to the state before switchd managed it, because a released port that stayed up
    could leak traffic into whatever network it is cabled to.
  * switchd creates and owns the bridge `swbr0` and one bond device per `ae` interface (named like the `ae`). It never
    modifies other bridges, bonds or VLAN devices.

### 1.5 The three planes

Every port belongs to exactly one plane. Traffic never crosses from one plane to another inside the switch.

| Plane | Ports | Carries | IP |
|---|---|---|---|
| **Data plane** | Switch ports (`unit 0 family ethernet-switching`), bundles, VXLAN tunnels, and the routed interfaces of the default and data routing instances (irb, routed ports) | Client traffic, routed between VLANs where configured (5.3.2, 5.3.3) | only on routed interfaces |
| **Stacking plane** | Dedicated **stacking ports** (VC ports), direct 1:1 cables between members, cabled as a ring | Stack configuration, member state, MC-LAG synchronisation, BFD (the stacking protocol, untagged), and client traffic between members inside the **stack tunnels** (5.2) | internal only: a hidden routing instance that carries the stack tunnels, never configurable or reachable from other planes |
| **Management plane** | The management instance (`system management-instance <instance>`): the chassis management interface `cme` on the members' management ports, and optionally in-band irb units (1.8) | Administration only: SSH and the CLI, web/API, ping, syslog, NTP, DNS, software updates | yes, in the management instance only, and only on the master |

Rules that follow from this:

* **Stacking protocol frames on data ports are client traffic.** The stacking protocol uses untagged frames with
  EtherType `0x88b5`. switchd receives these frames *only* on designated stacking ports. On access, trunk or VXLAN ports, a frame with this EtherType is **never interpreted and never
  influences stacking or MC-LAG**. It is switched like any other frame and is not dropped.
* The protocols a switch port legitimately terminates are the data plane's own link-local protocols. LACP is consumed on
  bundle members. BPDUs are consumed when RSTP runs, and trigger `bpdu-block`.
* **No IP on switch ports.** switchd disables IPv6 (link-local addresses, router solicitations, neighbour discovery)
  on them and never assigns addresses. IP exists only on routed interfaces (5.3.2, 5.3.3). Stacking ports carry only
  the internal addresses of the hidden stack instance (5.2), never configured ones, and IPv6 stays disabled on them.
* **Stack traffic never uses the management network.** Configuration sync, CLI sessions forwarded to the master,
  commands for other members, time and software distribution, MC-LAG synchronisation and client traffic between
  members run only over stacking ports (the stacking protocol, 5.2). The management network carries only
  administration of the switch, and no IP traffic of the management plane crosses stacking ports.
* Routing instances are separate routing tables: nothing is routed between the management instance and the data
  plane, or between two instances.
* **Services live in the management instance, on the master.** With `system management-instance`, the master's own
  traffic (syslog, NTP, DNS lookups, software downloads) goes out through the management instance (1.8). The addresses of data routed interfaces are protected:
  they answer ping, ARP and neighbour discovery, the routing protocols configured on that interface (OSPF on its OSPF
  interfaces, BGP from its configured neighbours, BFD from the neighbours of a BFD session), replies to connections the
  switch opened, and nothing else. So the SSH servers (the OS's and the CLI's) are reachable only through the
  management interfaces, or through the OS's own interfaces that switchd does not manage.
* An in-band management VLAN on the data trunks is possible (an irb unit in the management instance). Management then
  shares the fate of the data plane. Commit confirmation and the serial console are the safety nets. A dedicated port avoids that.

---

### 1.6 Interface numbering

Physical ports are named `<member>/<card>/<port>`, like Junos (`ge-0/1/2` becomes `1/1/2`):
* **card**: the NIC the port belongs to. On PCI systems this is the PCI device (the address without the function
  number): the card at `01:00` is card 0, the one at `04:00` card 1, the onboard NIC at `07:00` card 2. Cards are
  numbered in PCI address order. USB and other non-PCI NICs (e.g. the SoC ports of ARM boards) come after the PCI cards.
* **port**: the port on that card, from 0, in the order of the PCI function and then the port number the driver
  reports (`01:00.0` → `1/0/0`, `01:00.1` → `1/0/1`).
* Card numbers are **fixed on first sight** and stored on the member. A card added later gets the next free number,
  even in a lower PCI slot, so existing port names never change. A card that is replaced in the same PCI slot keeps
  its number. SR-IOV virtual functions are not switch ports.
* In a VM every virtual NIC is its own PCI device, so every port is `…/<card>/0`.
* `show chassis hardware` lists every port with its Linux name, PCI address, driver and MAC address, and below it
  the known cards that are **absent** (number, bus address, driver, port count, when last seen): their numbers stay
  reserved, and the configuration of their ports stays until you delete it.
* A card that **changed model** in its slot (other driver or port count) keeps its number; the change is logged
  (warning) and shown under the ports. Configured ports that no longer exist are reported by the commit check.
* A card that **moved to another slot** gets a new number; switchd recognises it by the MAC addresses of its ports,
  logs it and shows the hint `request chassis card <new> renumber <old>`.
* `request chassis card <n> renumber <m>` (super-user, after a `[yes,no] (no)` question that names the configured
  interfaces whose ports change) gives present card n the number m, which must be free or belong to an absent card
  (that card is forgotten). The ports are renamed at once, so the configuration of `…/<m>/…` applies to them.
* `request chassis card <n> forget` releases the number of an absent card (after a question). Both commands accept
  `member <id>`.
* Linux interface names are not used in the configuration. A configuration that still uses the older form
  `<member>/<linux-name>` (e.g. `1/ens19`) is converted automatically when it is loaded from the member's storage
  (active configuration, rollback revisions and the shared candidate), as long as the port exists.

### 1.7 Hardware capability checks

`commit check` (and every commit) compares the configuration with what each port's NIC and driver can do, on every
member whose hardware is known. The facts come from the kernel (ethtool: supported link modes, pause support,
features such as `vlan-challenged` and `hw-tc-offload`). Where a driver reports nothing, no check is made.
* E: `mtu` above the NIC's maximum (5.3.2).
* E: a trunk port, `native-vlan-id`, or routed subinterfaces (`vlan-tagging`) on a NIC that cannot carry VLAN tags
  (`vlan-challenged`).
* W: `flow-control` or `no-flow-control` on a NIC without pause-frame support (the setting has no effect).
* W: an `ae` bundle whose member ports have different maximum speeds (e.g. 1G and 10G): traffic is hashed evenly,
  so the slowest port limits each flow's share. For MC-LAG bundles this is checked across both members (5.6).
* `show system offload` lists per port: maximum speed, pause support, switchdev (hardware switch), tc offload,
  VLAN filter offload, checksum/TSO/GRO, MACsec encryption offload (`macsec-hw-offload`; without it MACsec is
  encrypted by the CPU), and whether switchd's rules on the port are in hardware.
* The PCIe link of each NIC (negotiated vs. possible speed and width) is part of the system diagnostics (PLAN.md Phase 13).

### 1.8 Managing the virtual chassis

The stack is managed as one switch through one address, as a Junos Virtual Chassis is through its VME interface.

* **Management ports.** Any port can be a management port: `set interfaces <port> management` (5.3.4). A member can
  have several, and a member without one is fine. Management ports are never bridged, carry no VLANs and are not
  connected to each other through the stack.
* **One management address: `cme`.** The chassis management interface `cme` (5.3.4) holds the stack's management
  addresses. It exists **only on the master** (the member the stack's Raft election makes master), on the first of
  its management ports (by name) whose link is up; as with VRRP, the address is present on exactly one member at any
  time. On the other members the management ports are up (link established) but have no address and ignore every
  frame they receive. The management ports of different members are never bridged or otherwise connected.
* **The address follows mastership.** The mastership election in the stacking protocol decides where it is, so no
  election traffic appears on the management network. A member that stops being master removes the address before
  the new master adds it. If the master is alive but none of its management ports has a link, the address stays on
  the master and cannot be reached until a link returns; in-band management or a console still work.
  * The MAC address of `cme` is derived from the stack id and is the same on every member, so a move is only a MAC
    move for the management network. The new master announces it (gratuitous ARP, unsolicited neighbour
    advertisements). Duplicate address detection is off on `cme`.
  * **Every management function is on the master:** the CLI SSH server, the web interface, the management instance's
    routes, syslog forwarding, NTP and DNS. When the master fails and another member is promoted, the new master
    takes the address and all of them over.
  * In-band management (irb units in the management instance) follows the same rule: its addresses exist only on
    the master.
* **The master works for the stack.** Only the master resolves names, synchronises with NTP servers, forwards syslog
  messages and downloads software. The other members:
  * receive the time from the master over the stacking protocol (every 16 seconds; stepped above 128 ms, slewed below),
  * send their log messages to the master, which forwards them with the member's host name as HOSTNAME,
  * receive software updates from the master (an update on the master installs on all members),
  * run no NTP client, resolver or syslog forwarder of their own.
  Nothing is routed through the stack: the master performs these operations itself and passes the results on.
* **The CLI always works on the master.** A CLI session (SSH, serial console or display) runs on the member you are
  connected to, but every command is sent to the master over the stacking protocol and runs there; `member <id>`,
  `all-members` and `local` (the member you are connected to, 3.5) choose where operational commands take effect.
  The prompt shows the master's host name, and the banner line names the member you are connected to
  (`{master:1, connected to member 2}`).
  * This needs no management address and no `system services ssh` anywhere: it works through the local consoles
    even when every management port is down.
  * **When the master is unreachable** (the member is alone, or in a minority partition), the CLI shows
    `*** master not reachable (member <n>) … ***`, keeps trying, and offers a local shell to users allowed to
    `start shell`, as when switchd is not available (3.1).
  * `start shell` opens a shell **on the master**; `start shell local` opens one on the member you are connected to.
    On the local consoles you are root (5.1 `system ports`) and may open either. Through SSH it needs root or the
    super-user class (4.3).

### 1.9 Processes

The switch software is a set of programs. **switchd** is the switch daemon: configuration, commit, the CLI, stacking
and the data plane's structure. Each protocol and service runs in a program of its own, a **cer- daemon**, so that a
fault in one of them ends only that program: it is restarted at once, the kernel keeps forwarding meanwhile (bundles,
bridge port states, routes and filters stay as they are), and the restarted daemon continues from where it stopped.

| Program | Does | Runs |
|---|---|---|
| `switchd` | configuration, commit and rollback, the CLI, stacking (membership, mastership, Raft, stacking BFD, stack tunnels, relay between members, software distribution), VXLAN control, the data plane's structure, the management services' placement, starting and watching the cer- daemons | always |
| `cer-lacpd` | LACP (5.3.2): which ports of each bundle carry traffic | always |
| `cer-mclagd` | MC-LAG (5.6): leg states with the peer, split horizon, MAC synchronisation, failover (holds), multicast groups on MC-LAG bundles | always |
| `cer-rstpd` | RSTP (5.5): one bridge for the stack, port roles and states | always |
| `cer-lldpd` | LLDP (5.5) | always |
| `cer-syslogd` | remote syslog (5.1): reads the journal and forwards; members hand their messages to the master | always |
| `cer-ntpd` | NTP client on the master, time from the master on the other members (5.1) | always |
| `cer-dhcpcd` | DHCP clients of `family inet dhcp` (5.3.2) | always |
| `cer-ribd` | the routing table (5.8): static, connected, DHCP and protocol routes, preferences, ECMP; installs routes | always |
| `cer-bfdd` | BFD (5.12) for the routing protocols | while BFD is configured |
| `cer-ospfd` | OSPF and OSPFv3 (5.13) | while configured |
| `cer-bgpd` | BGP (5.14) | while configured |
| `switchd-update` | software installation and rollback (3.6) | always |
| `swcli` | the CLI client (login shell) | per session |

**switchd starts and watches the daemons.** It writes a systemd unit for each (`cer-<name>.service`) with the program,
its arguments and its scheduling (below), starts the daemons it needs, stops those that are no longer needed, and
checks every second that they run. systemd executes them; this way a daemon keeps running while switchd itself
restarts (a restart of switchd is hitless, and so is one of a daemon). Daemons tell systemd that they are alive
(watchdog); one that hangs for 10 s is ended and restarted like one that crashed. A daemon that ends unexpectedly is
started again after 0.2 s (`cer-lacpd` and `cer-bfdd` after 0.1 s), however often that happens.

**Stopping.** When switchd is stopped (the system shuts down, `request system reboot|halt|power-off`, or
`systemctl stop switchd`), it first drains the member where that applies (5.2), then stops the daemons in this order,
the daemons of one step in parallel, each with a budget to finish its work: the routing protocols (they close their
sessions: BGP 10 s, OSPF 5 s), BFD (its sessions end with AdminDown, so neighbours do not count a failure; 2 s),
then MC-LAG (its legs leave their bundles as in maintenance mode when the peer can take over, and MAC
synchronisation ends; 5 s) together with RSTP, the routing table, LLDP (shutdown LLDPDUs), DHCP (the leases are kept
for the next start) and NTP (3 s each), then LACP (3 s), and syslog last (it sends what is still queued, the
shutdown's own messages too; 5 s). A daemon that does not finish within its budget (it hangs) is killed; the whole
stop takes at most a minute. A **restart** of switchd (or its crash) leaves the daemons running. After a full stop,
MC-LAG legs rejoin as after a boot (`delay-restore`).

**Every CLI session is told** when a daemon of any member fails and when it is back, e.g.
`*** member 2: cer-lacpd failed (killed by signal SEGV) and is restarted ***` and
`*** member 2: cer-lacpd runs again (restart 3 in the last hour) ***`. The same appears in the log (facility `daemon`,
severity `error` and `notice`). A daemon that cannot be started at all (missing program) is reported every minute.

**Scheduling.** The daemons whose timing the network depends on come first when the CPUs are busy (software
forwarding uses them heavily):

| Program | CPU scheduling | Memory (killed first when out of memory: higher) |
|---|---|---|
| `cer-bfdd` | real-time (`SCHED_FIFO`, priority 50) | −900 |
| `switchd` (stacking BFD) | nice −10 | −900 |
| `cer-lacpd`, `cer-rstpd`, `cer-mclagd` | nice −10 | −900 |
| `cer-ribd`, `cer-ospfd` | nice −5 | −500 |
| `cer-bgpd` | nice −5 | 0 (large tables: given up before the others) |
| `cer-lldpd`, `cer-ntpd`, `cer-dhcpcd`, `switchd-update` | nice 0 | −500 |
| `cer-syslogd` | nice 10, idle I/O | 0 |

Each daemon gets only the privileges it needs (e.g. `cer-lldpd` raw sockets, `cer-ntpd` setting the clock).

**Who changes what in the kernel.** Every kind of kernel object has exactly one owner; no other program changes it.
Daemons that need something created ask switchd, which applies it with the configuration (hitless, 4.14).

| Kernel object | Owner |
|---|---|
| Network devices (bridge, ports' settings, VLANs, bonds and teams, VRFs, irb, VXLAN, stack tunnels, `cme`), addresses (also those a DHCP lease brings: `cer-dhcpcd` only reports the lease), bridge VLANs, tc rules, nftables tables except `switchd_mclag`, routes of the stack's internal table | `switchd` |
| Which ports of a team carry traffic | `cer-lacpd` |
| Spanning-tree state of bridge ports | `cer-rstpd` |
| `switchd_mclag` (split horizon), MAC addresses synchronised for MC-LAG, multicast groups installed on the peer's legs | `cer-mclagd` |
| Routes of every routing instance (protocol ids `switchd`, `ospf`, `bgp`), including the DHCP default route | `cer-ribd` |
| The system clock | `cer-ntpd` |

**Logs.** Every program writes to the system journal (with its name, severity and facility). `cer-syslogd` reads the
journal (including kernel messages) and forwards it to the configured servers (5.1); `show log` shows what it read.
On the firmware image the journal is kept in memory (tmpfs, at most 64 MB, oldest entries dropped first), so logging
never wears the boot medium; remote syslog is where logs are kept.

**Hanging devices.** No program waits without limit for the kernel, a disk or a tool: every such call has a deadline
(5 s for netlink, ethtool and `/sys`; 10 s for files and tools; docs/os-image.md §7). A device that misses one raises
an alarm (log and a notice to every CLI session), its later calls fail at once until it answers again, and the alarm
clears then. Nothing reboots. switchd and the daemons have a systemd watchdog that is fed only while their loops make
progress, so a deadlock restarts the program (switchd restarts without stopping the daemons).

Operational commands:
* `show system processes`: per member every program with its state (`running`, `restarting`, `failed`, `stopped`),
  process id, uptime, restarts in the last hour, the last failure, memory and CPU time, and its scheduling; and an
  **ALARM** list of system calls that do not return (a disk or NIC driver that stopped answering).
* `restart lacp|mclag|rstp|lldp|syslog|ntp|dhcp|routing|bfd|ospf|bgp [member <id>|all-members]`: restarts that daemon
  (super-user). Like a crash, it is hitless where the protocol allows it (LACP and RSTP keep their state; BFD, OSPF and
  BGP sessions are re-established, with graceful restart where configured).

## 2. Configuration formats

The same configuration can be shown and loaded in three equivalent formats. All three round-trip
losslessly, and the test suite verifies this.

### 2.1 Hierarchical (default)

```
system {
    host-name core;
    name-server [ 1.1.1.1 9.9.9.9 ];
}
interfaces {
    1/0/1 {
        description "server A";
        mtu 9000;
        unit 0 {
            family {
                ethernet-switching {
                    interface-mode access;
                    vlan {
                        members storage;
                    }
                }
            }
        }
    }
}
vlans {
    storage {
        vlan-id 20;
        mtu 9000;
    }
}
protocols {
    rstp;
}
```

Rules:
* A statement ends with `;`. A block is written as `{ … }`.
* `interfaces` and `vlans` wrap their entries in a block named after the list. All other lists write the keyword
  in front of each entry (`host 10.0.0.5 { … }`, `member 1 { … }`).
* A leaf-list with one value is written without brackets (`members storage;`). With several values it uses `[ … ]`.
* Empty presence containers and empty list entries are written as `name;` (`rstp;`, `vlans { empty; }`).
* Values containing spaces or any of `" \ [ ] { } ; | #` are double-quoted. `\"` and `\\` escape inside quotes.
* Comments: `# to end of line` and `/* block */`. They are accepted when loading and are not stored.

### 2.2 Set commands (`| display set`)

```
set system host-name core
set system name-server 1.1.1.1
set system name-server 9.9.9.9
set interfaces 1/0/1 description "server A"
set interfaces 1/0/1 mtu 9000
set interfaces 1/0/1 unit 0 family ethernet-switching interface-mode access
set interfaces 1/0/1 unit 0 family ethernet-switching vlan members storage
set vlans storage vlan-id 20
set vlans storage mtu 9000
set protocols rstp
```

* Each line is a complete, absolute `set` command. A leaf-list produces one line per value.
* Inactive statements (3.3) produce an additional `deactivate <path>` line after the `set` lines.
* `show … | display set relative` (inside `edit`) prints paths relative to the current edit level.

### 2.3 JSON (`| display json`, REST API)

```json
{
  "system": { "host-name": "core", "name-server": ["1.1.1.1", "9.9.9.9"] },
  "interfaces": {
    "1/0/1": {
      "description": "server A",
      "mtu": "9000",
      "unit": { "0": { "family": { "ethernet-switching": {
        "interface-mode": "access", "vlan": { "members": ["storage"] } } } } }
    }
  },
  "vlans": { "storage": { "vlan-id": "20", "mtu": "9000" } },
  "protocols": { "rstp": {} }
}
```

* Containers and list entries are objects, and lists are objects keyed by the entry key.
* Leaves are **strings** (also numbers, so values keep their canonical text form), leaf-lists are arrays of strings,
  and flags are `true`.
* An empty object is only meaningful for presence containers and list entries.
* Inactive statements carry `"@inactive": true` in their object (leaves: sibling key `"@inactive:<name>": true`).

---

## 3. Editing, directives and loading

### 3.1 Modes

* **Operational mode** (prompt `user@host>`): `show`, `monitor`, `request`, `clear`, `ping`, `confirm`, `configure`, …
* **Configuration mode** (prompt `user@host#`, with the current edit level shown as `[edit …]`):
  | Command | Effect |
  |---|---|
  | `configure` | Edit the **shared candidate**. Everyone in plain `configure` sees the same candidate, and on entry you get a message naming the other users who are editing. |
  | `configure private` | Edit a private copy of the committed configuration. It is not allowed while the shared candidate has uncommitted changes. Your commit applies only your changes. If someone else committed in between, commit fails and asks you to run `update`, which replays your changes on top of the latest commit. A private candidate without changes follows other commits automatically. |
  | `configure exclusive` | Lock the configuration. Nobody else can commit or change the shared candidate until you leave. Uncommitted changes are discarded when you exit. |

  Leaving plain `configure` keeps uncommitted changes in the shared candidate, and you are warned about them.
  Leaving `private` or `exclusive` discards uncommitted changes after a confirmation prompt.
  The shared candidate is stored on disk and survives a restart of switchd.

* **Multi-user notices.** When someone commits, confirms or rolls back (including the automatic rollback), every
  other CLI session immediately shows a notice such as `*** thorben: commit confirmed (revisions 13–14) ***`. The
  notice redraws the prompt, the edit level and the `[commit pending confirmation …]` line, and keeps the text
  typed so far. Pressing Enter on an empty line also refreshes them (e.g. the minutes left).

* **switchd restarts.** A CLI session survives a restart or crash of switchd:
  * The CLI shows `*** switchd is not available … ***` and keeps trying to reconnect in the background. It does
    not log you out.
  * Users allowed to `start shell` (4.3) are offered a Linux shell meanwhile. Leaving the shell returns to the
    waiting CLI. Other users can wait or log out with Ctrl-D.
  * When switchd is back, the CLI shows `*** switchd is available again ***` and continues in operational mode.
    The shared candidate is kept (see above). Private and exclusive candidates, and the edit level, are lost.
  * A command that was running when the connection broke is reported as interrupted. It is **not** repeated
    automatically, since it may or may not have completed; check with `show system commit`.
  * If the CLI program itself fails, super-users (and root) get a Linux shell instead. When they leave it, the
    CLI starts again.

### 3.2 Configuration-mode commands

| Command | Effect |
|---|---|
| `set <path> [<value>]` | Create the statement or change its value. Missing parents are created. |
| `delete [<path> [<value>]]` | Remove the statement and everything below it. With a leaf-list value, only that value is removed. `delete` on its own asks for confirmation and removes everything below the current level. Deleting something absent prints `warning: statement not found` and is otherwise harmless. |
| `edit <path>` | Move the edit level to a container or list entry (it is created on the first `set` below it). |
| `up [<n>]`, `top`, `exit` | Move up one or n levels, go to the top, or leave the level (at the top: leave configuration mode). |
| `show [<path>]` | Show the candidate below the current level. |
| `copy <path> to <key>` | Duplicate a list entry: `copy interfaces 1/0/1 to 1/0/2`. |
| `rename <path> to <key>` | Rename a list entry: `rename vlans storage to san`. References elsewhere are **not** renamed, so `commit check` reports broken references. |
| `deactivate <path>` / `activate <path>` | Mark a statement inactive/active (3.3). |
| `status` | List users editing the configuration. |
| `update` | (`configure private` only) Rebase your private changes onto the latest committed configuration. |
| `run <operational command>` | Run an operational command without leaving configuration mode. |
| `load …` / `save …` | See 3.4. |
| `commit …`, `rollback …`, `confirm` | See section 4. |
| `set system login user <u> authentication plain-text-password` | Prompts twice for a password (not echoed) and stores only its hash in `encrypted-password`. The plain text is never stored or logged. |

`show` pipes (also in operational mode): `| display set [relative]`, `| display json`, `| compare [rollback <n>]`,
`| match <regex>`, `| except <regex>`, `| find <regex>`, `| last <n>`, `| count`, `| no-more`.
Several pipes can be chained: `show | display set | match vlan`.

### 3.3 Directives

**`inactive:`**: an inactive statement stays in the configuration and is displayed and saved, but it is
**ignored** by validation and by the data plane, exactly as if it were deleted. Use it to switch something
off temporarily without losing its settings.
```
interfaces {
    inactive: 1/0/7 {
        mtu 9000;
    }
}
```
In set format: `set interfaces 1/0/7 mtu 9000` followed by `deactivate interfaces 1/0/7`.
Deactivating a parent deactivates everything below it. Children keep their own inactive markers, so they
return to their previous state when the parent is activated again.

**`replace:`** and **`delete:`**: only meaningful when loading hierarchical text with `load replace` (3.4):
```
vlans {
    replace: storage {          # the whole entry is replaced by this block
        vlan-id 20;
    }
    delete: old-vlan;           # the entry is removed
}
```

### 3.4 Loading and saving

| Command | Effect on the candidate |
|---|---|
| `load merge terminal` \| `load merge <file>` | Merge the text into the candidate. Leaves are overwritten, leaf-lists are extended, and nothing is removed. |
| `load replace terminal` \| `load replace <file>` | Like merge, but statements tagged `replace:` replace the existing statement completely, and statements tagged `delete:` are removed. |
| `load override terminal` \| `load override <file>` | Replace the **entire** candidate with the text. |
| `load set terminal` \| `load set <file>` | Execute `set`/`delete`/`activate`/`deactivate` lines in order, relative to the current edit level. |
| `save <file>` | Write the candidate below the current level in hierarchical format. |

* Hierarchical and set format are detected automatically for `merge`, `replace` and `override`.
  If the first word is `set`, `delete`, `activate` or `deactivate`, set format is assumed.
* `terminal` reads until Ctrl-D. Pasting into the terminal is supported (bracketed paste).
* Loading is **all or nothing**: if any line fails to parse, the candidate is left unchanged, and the error reports the line number.
* File paths are relative to the user's home directory. Loading only changes the candidate; nothing takes effect until `commit`.

---

### 3.5 Operational commands

| Command | Shows / does |
|---|---|
| `show interfaces [terse\|extensive] [<interface>]` | Status, role, VLANs and counters of the ports and bundles. |
| `show chassis hardware` | Every physical port with Linux name, bus address, driver and MAC address (1.6). |
| `show system offload` | Hardware capabilities and acceleration per port (1.7). |
| `show system limits` | What this switch can carry and how much of it is used (3.5.1). |
| `show vlans` | VLANs with their ports (`*` = tagged). |
| `show ethernet-switching table [vlan <v>] [interface <if>]` | Learned and static MAC addresses. `clear ethernet-switching table …` removes learned ones. |
| `show arp [no-resolve]` | The IPv4 neighbour table of all routing instances (default, management and data instances, including `cme`). Columns: MAC address, IP address, interface (switch name where it is a port), instance, state. |
| `show ipv6 neighbors` | The same for IPv6. |
| `show route …` | The routing tables with every route and its source (5.14, `show route`); `show route instance` alone lists the instances with their type, route count and interfaces. |
| `show ospf …`, `show ospf3 …`, `show bgp …`, `show bfd session` | Routing protocol state (5.12–5.14). |
| `show system bottlenecks` | What limits this member's forwarding, with recommendations (3.5.2). |
| `show igmp snooping membership\|vlans`, `show mld snooping …` | Multicast groups and per-VLAN snooping state (5.5). |
| `show vxlan [remote-vtep]` | VNIs, remote VTEPs and their reachability (5.7). |
| `show system ntp` | The NTP servers with the address that answered, stratum, offset, delay and last poll, which server the clock follows (`*`), whether the clock is synchronised, and through which routing instance the queries leave. |
| `show system uptime` | Current time, when the system booted, when switchd started, when and by whom the configuration was last changed, load averages. |
| `show system commit`, `show system rollback …` | See 4.1. |
| `show log`, `show system syslog`, `show version` | Recent log messages, remote syslog state, software version (with every member's two system slots, read from the disks: version, active or backup, `unreadable: …` when a slot's partition cannot be read, and a missing or damaged boot state). |
| `show system processes`, `restart <daemon>` | The switch's programs and their state; restarting one (1.9). |
| `request system reboot\|halt\|power-off [in <minutes>]` | After a confirmation prompt (`[yes,no] (no)`), reboots, halts or powers off this member, now or in n minutes. Every CLI session is notified. `clear system reboot` cancels a scheduled one. With stacking and MC-LAG, the member first drains (as for maintenance mode, 5.2): mastership moves away, stacking paths are routed around it and its MC-LAG legs leave their bundles after their partners stopped sending; then it shuts down. |
| `start shell [local]` | A Linux shell on the master, or with `local` on the member you are connected to (1.8, 4.3); `exit` returns to the CLI. |

#### 3.5.1 `show system limits`

One page with the limits of the switch and the current use of each, so nobody has to read the reference to know
whether a configuration fits. Every range comes from the same definitions as the configuration parser (they cannot
differ from what `commit` accepts); hardware facts are those of the member the command runs on (with a member
target, 3.5, those of that member). Sections and lines (a `-` means no limit applies):

| Section | Lines |
|---|---|
| **Frame sizes** (frame size incl. the Ethernet header, without VLAN tags, 1.3) | configurable `mtu` range and default; the largest configured `mtu` and where it is set (and the host MTU that fits it); the extra bytes the stack tunnels need (58); the largest `mtu` the stack carries (`show virtual-chassis mtu` has the details per stacking port); the hardware maximum of this member's ports (lowest and highest, with the port) |
| **Switching** | VLAN ids (1–4094) and how many are configured; VXLAN VNIs; learned MAC addresses now, the aging range and default, the `mac-limit` range per port |
| **Aggregation** | `ae` numbers (`ae0`–`ae4095`) and how many bundles are configured; the largest bundle (ports) |
| **MC-LAG** | members per MC-LAG bundle (2), peers per member (1); the configured MC-LAG bundles and pairs |
| **Stack** | members (1–16) and how many are configured; voters (at most 7 of the members); this member's stacking ports and whether the stack is a ring |
| **Ports** | physical ports of this member, the fastest port speed, stacking ports, the ports whose NIC encrypts MACsec in hardware (`MACsec offload: n of m ports`; the others encrypt in software) |

Use of a limit is shown as `n of max`. A line whose limit is reached is marked `(full)`; a configured value above a
hardware limit cannot exist (commit refuses it), so the page never shows one.

#### 3.5.2 `show system bottlenecks`

A check of what limits this member's forwarding, with a recommendation per finding (`member <id>`, `all-members`,
3.5). It only reads and changes nothing. Each finding has a severity (`limit`: the hardware or setting caps
throughput or causes drops now; `hint`: worth changing) and names the port, NIC or CPU:
* **PCIe**: per NIC the negotiated link (generation and width) against the card's maximum, and the bandwidth the
  NIC's ports need at their link speed against what the link carries (e.g. a 4×10G card in a PCIe 2.0 x4 slot carries
  16 Gbit/s of the 40 it needs).
* **Queues and CPUs**: the NIC's receive queues against the CPU cores; interrupts of a NIC's queues that all land on
  one CPU; receive packet steering off for a single-queue NIC on a multi-core system; the CPU frequency governor
  (`powersave` on a switch adds latency).
* **NIC settings**: receive/transmit rings below their maximum; offloads the NIC supports but has off (GRO, checksum,
  TSO, VLAN filtering); pause frames.
* **Drops**: per port the receive drops of the NIC (missed, FIFO, no buffer) and of the kernel, and per CPU the
  backlog drops and `time squeeze` counts of the network softirq, as rates since the previous run of the command
  (the first run shows the totals since boot).
* **Memory**: little available memory.
`request system diagnose` is the same command (as every `request`, for super-users, 4.3).

### 3.6 Software updates

cerOS runs as a **firmware image** with two slots (`docs/os-image.md`): the active one and a backup holding the
previous version. An update writes the whole new system (kernel, tools, switchd) into the backup slot and reboots into
it. A member whose new version does not come up returns to the old one by itself.

The stack is updated as one switch: the master fetches the software, checks it, and updates the members **one at a
time**, each drained first, so traffic keeps flowing (reference 5.2, maintenance mode).

**Bundle.** The software is one file per platform, `ceros-<version>-amd64.bundle`: a manifest (version, build time,
platform, SHA-256 and verity root hash of the image), its **Ed25519 signature**, and the image. The signing key must be
one the running version trusts. Development builds are signed with the development key, which only development
builds trust. **No option accepts an unsigned bundle or one with a wrong signature.** A SHA-256 of the whole bundle is
also verified when one is given (`sha256 <hex>`, or a `<bundle>.sha256` file next to it on the server).

**`request system software add <source> [sha256 <hex>] [member <id>] [no-validate] [force]`** (super-user):
* `<source>`: `http://…`, `https://…`, `ftp://…`, `sftp://user@host/path` (asks for the password unless a key of
  the user works), `usb:<file>` (the first USB stick of the master, mounted read-only while it is read), or a local
  file of the master (`/var/tmp/…`). Downloads leave through the management instance (1.8).
* Steps, each reported on the terminal as it happens:
  1. **Fetch and verify** the bundle on the master: signature, platform, SHA-256.
  2. **Check**: every member has the bundle's platform. The new version reads the active configuration and accepts it:
     the master runs the new image's configuration check. With `no-validate`, a failed configuration check is only a
     warning.
  3. **Distribute** the bundle to every member over the stacking protocol. Each member verifies it again.
  4. **Update the members one by one**, the master last. The member enters maintenance mode (drained), writes the new
     image into its backup slot, checks it and reboots into it. It comes back with the stack's configuration and
     leaves maintenance mode once it is current again. The next member starts only then. Before its own turn, the
     master hands mastership to an updated member, which finishes the update.
* `member <id>`: only that member (e.g. a member that joined with an older version).
* A member that is the **only stacking path** to other members (e.g. a switch cabled to it alone) is not updated:
  the update stops before draining it and names the members that would be cut off, because they (and possibly the
  stack's majority) would be lost while it reboots. `force` updates it anyway.
* Members that already run the version are skipped. The command can be repeated: it continues where an update
  stopped.
* **Failure**: a member that is not back and current within 10 minutes stops the update. It is reported, and the
  members not yet updated keep the old version. A member whose new version does not become healthy returns to the
  previous version by itself (below).
* The update runs on the master, not in the CLI session: leaving the CLI does not stop it. `show system software`
  shows its progress.
* **The update daemon.** On every member, the installation itself is done by `switchd-update` (systemd unit
  `switchd-update.service`, the same program in another role), not by switchd. switchd hands it the verified bundle.
  The daemon checks the signature again, writes the image into the backup slot, reads it back and compares it, and
  runs the new image's configuration check. It keeps a copy of the configuration, makes the new slot the one to boot,
  and reboots the member. After the reboot it watches switchd. switchd is healthy when it answers on its CLI socket
  with the new version and has applied the stack's configuration. If it is not healthy within 5 minutes, the daemon
  makes the old slot the one to boot and reboots again. A new system that does not get that far (kernel panic, hang)
  is caught by the boot loader: a slot that has not confirmed its last boot is skipped, and the old one starts.
  Since the daemon is not part of switchd, a failed switchd cannot stop its own rollback. It reports to switchd
  (`show system software`: `writing slot B`, `checking`, `rebooting`, `waiting for switchd`,
  `rolled back: <reason>`). The details, including every failure case, are in `docs/os-image.md` §4.

**`request system software rollback [member <id>]`**: the members (or one) reboot into their backup slot, the
version they ran before, the same way (one by one, drained). Afterwards the slots have changed roles, so a second
rollback returns to the newer version.

**`show system software`**: per member the running version and build time, both slots (version, `active` or
`backup`, `failed` when the slot must not be booted), the result of the last update (`rolled back: <reason>`), and the
state of a running update (`fetching`, `checking`, `distributing`, `updating member 3`, `done`, `failed: <reason>`). The slots are read **from the disks** for every command: the boot state from the ESP (`boot state: missing`,
`damaged` or `unreadable: …` when it cannot be used), and each slot's partition directly, past the page cache
(`slot B: 1.3.2 backup (unreadable: …)` when the partition cannot be read or holds no image). A disk that does not
answer is reported after at most 5 s; an update daemon that does not answer shows `slots: unknown (…)`.

**`request system zeroize [member <id>]`** (super-user, after a `[yes,no] (no)` question): erases the configuration
and everything on the data partition (logs, state) of the member and reboots it with the factory default: no
configuration, every port down, root logs in on the console. The member leaves the virtual chassis (its keys are
gone).

**`request system storage cleanup [member <id>]`**: deletes logs, crash reports and old software bundles from the data
partition. The configuration stays.

**Mixed versions.** During an update, members run different versions for a while:
* Stacking, MC-LAG and the stack tunnels keep working between the previous release and the current one.
* **Configuration**: a version reads every configuration of the previous release line; statements that changed are
  converted when they are read (as they have been so far). A member with an **older** version applies the stack's
  configuration **without the statements it does not know** (they are left out, and logged as not supported by that
  member's version); the members that know them apply them. `commit check` names the statements
  that some members ignore, with their member and version.
* A new version never stops forwarding because of the configuration: if the active configuration fails its check
  (a stricter rule), the data plane keeps its current state, the member reports the errors (`show system
  software`, syslog), and a commit that fixes them applies normally.

## 4. Commit model

### 4.1 Candidate, commit check, commit

Changes are made to a *candidate configuration* and have **no effect** until committed.

| Command | Effect |
|---|---|
| `show \| compare` | Show the difference between the candidate and the active configuration. |
| `commit check` | Validate the candidate without applying it. It prints all errors and warnings and never changes anything. |
| `commit` | Validate, apply to all stack members, and store as a new revision. |
| `commit comment "<text>"` | Commit and store a comment with the revision. |
| `commit confirmed [<minutes>]` | Commit with automatic rollback unless confirmed (4.2). |
| `commit and-quit` | Commit and leave configuration mode. |
| `confirm` | Confirm a pending commit (both modes). |
| `rollback [<n>]` | Replace the candidate with revision n (0 = active configuration, the default; so `rollback` alone discards all uncommitted changes). It still has to be committed. |
| `show system commit` | Revision history: number, time, user, comment and confirmation state. The last 50 revisions are kept. |
| `show system rollback <n>` | The complete configuration of revision n (see `show system commit`), with a `## Revision` header line (left out with `\| display set\|json`, so the output can be loaded again). |
| `show system rollback <n> compare <m>` | The changes from revision n to revision m, e.g. `show system rollback 1 compare 0` shows what the last commit changed. |

Commit steps:
1. **Validation** (the same as `commit check`): schema rules, cross-references, and hardware limits of every member
   that is known (e.g. maximum MTU of a NIC). Any **error** aborts the commit and nothing changes. **Warnings** are
   printed, and the commit continues.
2. **Apply** on every stack member, in parallel. Each member reports success or failure, and the per-member result is printed:
   ```
   member1: configuration check succeeds
   member2: configuration check succeeds
   member1: commit complete
   member2: commit complete
   ```
3. If **any member fails to apply**, *all* members immediately return to the previous revision. The commit is reported
   as failed, with the member's error message, and no revision is stored.
4. A new revision is stored and the change is logged (syslog facility `change-log`, including the diff).

**Applying is hitless.** Only statements that changed are applied: interfaces, VLANs and other objects whose
effective configuration is unchanged are not touched at all, and no link is taken down to reconfigure it. Changes
are ordered so that a changed port never passes through a state that combines old and new permissions: restrictions
(removed VLANs, filters, bond/bridge removal) are applied before new permissions (added VLANs, new PVID, mirroring).
This can cause a sub-millisecond gap on the changed port, but never a leak into another VLAN or port. The same rules
apply to every rollback, including the automatic one. If a change can only be made by resetting a link on the
member's hardware (e.g. an MTU change on some NIC drivers), `commit check` warns and names the interface before you commit.

Members that are unreachable during a commit apply the configuration when they reconnect. The commit output lists them as `pending`.

### 4.2 Commit confirmation and automatic rollback

`system commit confirmation mode` selects the policy:

* **`required`** (the default): *every* commit that changes the configuration must be confirmed.
* **`optional`**: only `commit confirmed` needs confirmation, and a plain `commit` is permanent.

When confirmation is needed:
1. The change is applied immediately, and the commit prints a notice:
   `commit confirmed will be automatically rolled back in 10 minutes unless confirmed`.
   The window is `system commit confirmation timeout` (default 10 minutes), or the minutes given to `commit confirmed <m>`.
2. To confirm, run `confirm` (both modes), or run `commit` in configuration mode with no further changes.
   Every revision up to that point then becomes *confirmed*. `commit check` does **not** confirm.
3. Committing again while a confirmation is pending applies the new change and **restarts** the timer. The rollback
   target stays the **last confirmed revision**, so one expired timer undoes the whole unconfirmed series.
4. If the timer expires, switchd rolls back to the last confirmed revision:
   * The rollback is itself stored as a new revision with the comment `automatic rollback: revisions N–M not confirmed`.
   * It is logged at severity `warning` and announced to every logged-in CLI session.
   * The shared candidate keeps any uncommitted edits.
5. The pending state is stored persistently and replicated across the stack. If a member reboots or switchd
   restarts during the window, the deadline still applies. If it passed while the member was down, the member boots
   straight into the rollback target. If the leader fails, the new leader enforces the deadline.
6. Health checks (planned for later phases): if a post-commit check fails during the window, the rollback happens
   immediately. Examples: the management address is unreachable, stack members are lost, or MC-LAG peers go down after the commit.
7. A commit that **changes the confirmation policy itself** uses the stricter of the old and new policy.
   Switching from `required` to `optional` must therefore still be confirmed.
8. `commit and-quit` behaves the same way. The reminder to run `confirm` is printed, and the prompt shows
   `[commit pending confirmation: 9m left]` until confirmed.

### 4.3 Permissions

| Action | super-user | operator | read-only |
|---|---|---|---|
| `show`, `monitor`, `ping` | ✔ | ✔ | ✔ |
| `clear …` (counters, MAC table, errors) | ✔ | ✔ | – |
| `configure`, `commit`, `confirm`, `rollback` | ✔ | ✔ | – |
| change `system login`, `system services`, `virtual-chassis` | ✔ | – | – |
| `request system reboot/halt`, `request virtual-chassis …` | ✔ | – | – |
| `start shell [local]` (Linux shell as the logged-in user, on the master or with `local` on the connected member, 1.8; `exit` returns to the CLI) | ✔ | – | – |

An operator whose candidate touches a forbidden hierarchy gets an error at commit time naming the forbidden paths.

---

## 5. Statement reference

Each statement lists **behaviour**, **interactions** with related statements, and the **commit check** rules
(E = error, W = warning). Defaults apply whenever a statement is absent.

### 5.1 system

#### `system host-name <hostname>`
Name of the stack as a whole. It appears in syslog messages as the application name prefix, and in the CLI
prompt for members that have no `virtual-chassis member <id> host-name`. Default: `switch`.
* The member's host name (its `virtual-chassis member <id> host-name`, else `system host-name`) is also the operating system's
  host name: it is set at once (no reboot) and written to `/etc/hostname`, and `/etc/hosts` gets the line
  `127.0.1.1 <host>.<domain-name> <host>` (Debian convention). Shells that are already open show the new name
  in their prompt only when started again.
* Without either statement the operating system's host name is left alone. When the statements are removed, the
  host name stays as it is.

#### `system domain-name <hostname>`
DNS search domain, written to the resolver configuration of every member together with `name-server`.

#### `system time-zone <tz>`
IANA time zone (e.g. `Europe/Berlin`). It sets the system time zone on all members, and CLI output shows local time.
An unknown zone is a runtime alarm, and UTC stays in effect. Default: UTC.

#### `system name-server [ <ip> … ]`
DNS resolvers.
* switchd writes `/etc/resolv.conf` on every member (`nameserver` lines, and `search` from `domain-name`). The file it
  replaces is kept and restored when `name-server` is removed again. Without `name-server`, the file is not touched.
* If another program rewrites the file (e.g. a DHCP client), switchd restores it within 30 seconds and logs a
  warning naming the problem. Disable the other program's resolver handling (OS takeover, plan 4.15).
* Only the master resolves names (e.g. syslog server names), through the management instance when it is configured (1.8).
* W: more than 3 servers (only the first 3 are used).

#### `system ntp server <host> [prefer]`
NTP servers. switchd has its own NTP client (SNTP, RFC 5905 packets, UDP port 123); no NTP daemon of the operating system
is used or needed. Without any server, the OS time configuration stays untouched. Correct time matters for logs,
certificates and the stack's TLS.
* The master queries the servers through the management instance when there is one (1.5), like all traffic the
  switch originates, so the servers must be reachable from there (a static route in the instance). The other members
  get their time from the master (1.8).
* Every server is queried at start (after 2 seconds) and then every 64 seconds while the clock is not synchronised and
  every 512 seconds afterwards. The client uses the `prefer` server if it answers, otherwise the server with the
  lowest round-trip delay. Replies are checked (server mode, stratum 1 to 15, not "unsynchronised", not a
  kiss-o'-death, and they must carry the time of the request), so a forged or stray packet cannot set the clock.
* An offset above 128 ms **steps** the clock (and is logged as a warning); a smaller one is **slewed** by the kernel
  (`adjtime`), so time never jumps by a little. All members of a stack run their own client.
* A member without a reachable server keeps its clock and shows `Synchronized: no`; nothing else depends on it.
* W: another time service on the operating system (`systemd-timesyncd`, `chrony`, `ntpd`) is also setting the clock.
  Disable it (OS takeover, plan 4.15).
* `show system ntp` shows the servers and the state.

#### `system syslog host <host> { … }`
Sends log messages to a remote server, through the management instance (5.9) when there is one. The master
sends the messages of all members (1.8). The format is RFC 5424, with the member host name as HOSTNAME.
* `transport udp|tcp|tls`: default `udp`. For TCP and TLS a queue of up to 10 000 messages is held in memory while the
  server is unreachable. When the queue is full, the **oldest** messages are dropped, and the count appears in `show system syslog`.
* `port <1-65535>`: default 514 (udp/tcp) or 6514 (tls).
* `facility <facility>`: send only this facility. Default `any`. The facilities used by switchd are
  `daemon` (switch events), `authorization` (logins), `change-log` (commits including diffs), `interactive-commands`
  (every CLI command with the user), and `kernel` (kernel messages, e.g. link changes and NIC errors).
  On the wire the facilities are the standard numeric ones: `kernel` = kern (0), `daemon` = daemon (3),
  `authorization` = auth (4), `change-log` = local6 (22), `interactive-commands` = local7 (23). `local0`–`local5`
  are not used by switchd (a filter on them sends nothing); `local6`/`local7` select the same messages as
  `change-log`/`interactive-commands`.
* `severity <level>`: send messages of this severity **or more severe**. Default `info`. `any` sends everything.
* A UDP message that cannot be sent is lost (UDP has no delivery guarantee); use `tcp` or `tls` where that matters.
* The local buffer and `show log` contain every message regardless of these filters.
* The messages come from the system journal (1.9): those of switchd and the cer- daemons, kernel messages
  (`kernel`), logins (`authorization`) and the other services of the operating system (`daemon`).
* `ca-certificate <path>`: PEM CA certificate used to verify a TLS server. Default: the OS CA store.
  The server certificate must match `<host>`.

#### `system syslog local-buffer-size <lines>`
Number of recent messages kept in memory per member for `show log`. Default 5000.

#### `system login message <text>`
Banner shown before authentication on SSH and serial logins.

#### `system login user <username> { … }`
Creates a local Linux account on **every** member. Its login shell is the CLI, so logging in via SSH (or on a console with
`system ports login-required`) lands directly in the CLI.
* `class super-user|operator|read-only`: permissions (4.3). Default `read-only`.
* `uid <1000-64000>`: numeric user id. Default: assigned automatically from 2000 upwards and kept stable afterwards.
* `full-name <text>`: stored as the account's GECOS field.
* `authentication encrypted-password <hash>`: a crypt(3) hash (`$6$…` SHA-512 or `$y$…` yescrypt). Use `plain-text-password` in the CLI to create one.
* `authentication ssh-key <key>`: an OpenSSH public key line. Several keys are allowed.
* Removing a user ends their sessions and deletes the account. Its home directory is kept but handed to root
  (mode 0700), so a later account that gets the same uid cannot read it. A new user with the same name gets it back. If the account
  cannot be deleted yet (e.g. a process of the user is still running), switchd retries every 30 seconds. switchd only ever modifies accounts that it created itself.
  Existing OS accounts with the same name are **not** taken over, and that conflict is reported as an E at commit.
* `root` is not a `system login user`; its password and keys are `system root-authentication`.
* W: a user with neither password nor key (they cannot log in).

#### `system root-authentication { encrypted-password <hash>; ssh-key <key>; }`
The password and SSH keys of `root` on **every** member: the image keeps no OS files across a reboot
(`docs/os-image.md` §3), so root's login is part of the configuration like every other account's.
* `encrypted-password <hash>`: a crypt(3) hash, as for `system login user` (`plain-text-password` in the CLI creates one).
* `ssh-key <key>`: an OpenSSH public key line; several are allowed. They are used by the CLI SSH server for root
  according to `system services ssh root-login`.
* Without it, root has no password and no key: root can only log in on a local console (which logs in root without a
  password unless `system ports login-required` is set, 5.1). W when `login-required` is set without
  `root-authentication` (root cannot log in at all then).
* Root lands in the CLI as super-user; `start shell` leads on to a Linux shell.

#### `system services ssh { port <n>; root-login deny|allow|key-only; }`
SSH access to the CLI. switchd runs **its own SSH server instance** for this (unit `switchd-sshd`, configuration in
`/etc/switchd/`). The operating system's SSH server is never modified, so no CLI setting can cut off root or
automation access on the OS port.
* Without `system services ssh`, no CLI SSH server runs. Managed users can still log in through the OS SSH server
  (their login shell is the CLI).
* `port <n>`: default 22. E: the port is already used by another program, e.g. the OS SSH server before the
  takeover (4.15 of the plan masks it). Use another port, such as 2222, until then.
* Only managed users (`system login user`) are admitted, plus `root` according to `root-login`:
  `deny` (default), `key-only` (public key only) or `allow`. `root` also lands in the CLI (as super-user).
* It uses the host's SSH host keys, so the fingerprint is the same as on the OS port.
* Passwords are accepted for users with an `encrypted-password`; keys come from `authentication ssh-key`.
* The pre-login banner is `system login message`.
* The server runs **on the master only (1.8), inside the management instance**: it accepts connections through the
  management addresses, not through data interfaces. When mastership moves, the server stops on the old master and
  starts on the new one; sessions on the old master end. Without a management instance no CLI SSH server runs.
* The server is for people. The stack itself never uses it: sessions reach the master over the stacking protocol (1.8).

#### `system services web-management { port <n>; certificate <file>; key <file>; disable; }`
*Not implemented yet* (W at commit: the statement has no effect yet).
HTTPS web interface and REST API, reachable through the management instance on the master (1.8). Default: port 443 with a self-signed certificate generated
at first start. `certificate` and `key` must be given together (E otherwise). `disable` turns it off.

#### `system management-instance <instance>`
Makes routing instance `<instance>` (any name, 5.9) the management instance (1.5, 1.8). Its interfaces are `cme.0` and
optionally irb units; its addresses and routes exist only on the master (1.8), and the master's services use it: syslog,
NTP, DNS lookups and software downloads go out through it.
* E: the instance is not configured under `routing-instances`.
* E: `cme.0` is configured but not in the management instance, or there is no management instance.
* E: an address with `member` on an irb unit of the management instance (the management addresses belong to the master).
* W: the management instance has no interface (the stack has no management address).
* Without a management instance the switch is managed through its local consoles only (1.4).

#### `system commit confirmation { mode required|optional; timeout <minutes>; }`
See 4.2. Defaults: `required`, 10 minutes.

#### `system ports { no-auto-detect; login-required; console <tty> { speed <baud>; disable; } }`
The local consoles, meaning the serial consoles and a connected display with keyboard (the virtual terminals), always run
the CLI. On these consoles you are **root without a password** by default, because physical access is equivalent
to root anyway (the boot loader, single-user mode, or removing the disk). Only remote SSH sessions authenticate.
* `start shell` gives a root bash. `exit` in that bash returns to the CLI, and `cli` in any shell starts the CLI.
* `exit` in the CLI ends the console session, and the console restarts in the CLI.
* If switchd is not running, the CLI offers a root shell (see 3.1, switchd restarts). A crash of the CLI program
  itself also ends up in a root shell. The console never leaves you without access.
* `login-required`: the consoles ask for user name and password (managed users and root). Whoever logs in lands in the CLI.
* Root on the OS SSH server (port 22) keeps a plain bash, for automation.
* A change to these settings applies to a console at once when nobody is using it. On a console with an open
  session it applies when that session ends, so a commit never cuts off the console you are working on.

**Serial console auto-detection** (the default) starts a login on each of the following:
  * `ttyS*` ports with a real UART behind them,
  * `ttyUSB*` / `ttyACM*` adapters (including hot-plugged ones),
  * the kernel console (`console=` boot parameter).

  The speed is 115200 unless configured.
* `console <tty>`: overrides speed for this device or `disable`s it. Listed devices get a login even if auto-detection misses them.
* `no-auto-detect`: only the listed consoles get a login.
* `speed 9600|19200|38400|57600|115200`: default 115200.

#### `system offload { mode auto|disable; watchdog { interval <s>; threshold <n>; alarm-only; } }`
Hardware acceleration policy (see also `interfaces <if> offload disable`).
* `mode auto` (default): the switch uses hardware offload wherever the NIC and driver support it:
  switchdev bridge/FDB/VLAN offload, tc offload for mirroring/storm control/VLAN MTU filters, checksum/TSO/GRO,
  VXLAN and MACsec offload. Rules are **always** also installed in software (never `skip_sw`), so a
  rule the hardware rejects is still enforced in software instead of being lost.
* `mode disable`: all forwarding and filtering happens in software.
  Stateless NIC offloads (checksum, TSO, GRO) stay enabled because they do not affect forwarding decisions.
* **Watchdog**: every `interval` seconds (default 5) it reads per-port hardware and driver drop/error counters, and
  checks whether rules are really in hardware.
  * If drops or errors exceed `threshold` per interval (default 100) and correlate with an offload feature, that
    feature is switched off **on that port only**. Examples: checksum errors after enabling rx checksum offload, or a tc rule that fell out of hardware.
  * An alarm is raised and a syslog `warning` is sent. The fallback stays in place until `clear system offload-fallback <if>`.
  * Drops caused by pure overload (e.g. `rx_missed` at line rate) raise an alarm with a hint (ring size, queues, CPU) instead.
  * `alarm-only`: never change settings, just raise alarms.
* At startup, switchd also tunes NICs to avoid drops: RX/TX rings at their maximum and RSS across all queues. Pause frames are left
  at the driver default unless `ether-options flow-control` is set.

### 5.2 virtual-chassis

The stack is configured like a Junos Virtual Chassis. Members are numbered 1–16 (Junos uses 0–9); the member
number is the first part of every port name (1.6).

#### Stacking ports (not part of the configuration)

Stacking ports connect members **directly** (1:1 cables, no switch in between). Chain, ring and any mesh are
supported. Messages between members that are not directly connected are relayed hop by hop along the shortest
working path, so a ring survives one broken cable.

* **Designation**: stacking ports (VC ports) are set with the operational command
  `request virtual-chassis vc-port set <interface>` (and `… vc-port delete <interface>`), for a port of any member,
  e.g. `request virtual-chassis vc-port set 1/1/0`; the request goes to the member the port belongs to. A switch that
  has not joined yet is member 1 of its own stack, so on it the ports are `1/<card>/<port>`. The Junos form
  `… vc-port set pic-slot <card> port <port>` names a port of the switch you are working on. The setting is stored on
  that switch, as in Junos, because a switch needs its stacking ports *before* it can receive the stack
  configuration. `show virtual-chassis vc-port` lists them, and `show virtual-chassis` shows the members, their roles
  and the topology.
* **Topology**: any shape of direct cables works (chain, ring, star, full or partial mesh); the members route
  stack messages and the stack tunnels over the shortest working paths (equal paths share the load), and a path
  that fails is replaced within the BFD detection time. A ring (or more cables) survives any single cable failure,
  a chain does not.
* A stacking port is never a data or management port. E: the port is configured under `interfaces` (including
  `management`). Wildcard `interface-range`s skip stacking ports. switchd keeps a stacking port
  administratively up, outside the bridge, without IP addresses and with IPv6 disabled, whatever the configuration says.
* **Every switch is a stack.** A switch that has never joined another stack is member 1 of its own stack (it creates
  its stack key at first start). Two switches of different stacks connected by a stacking cable see each other as
  "other stack" and exchange nothing else until one of them joins the other's stack.
* `show virtual-chassis vc-port`: per stacking port its full name (`2/1/0`), state (`up`, `down`, `absent` when the port
  does not exist), link speed, the neighbour (member id and host name, `other stack`, or `-`), the neighbour's port
  (full name, e.g. `3/2/0`) and how long the link is up.
* `show virtual-chassis`: the stack id, this member, and per member its id, host name, role (`master`, `backup`,
  `linecard`), `mastership-priority` and status (`present`, `not present`, `maintenance`).
* **Protocol**: untagged Ethernet frames with EtherType `0x88b5`, no IP and no VLAN tag. Each stacking link carries a
  reliable stream (sequence numbers, acknowledgements, retransmission, fragmentation to the link MTU). **TLS 1.3 with
  mutual certificate authentication** runs on top, using the stack's own key. Frames from unauthenticated devices are ignored.
  Member certificates never expire and do not depend on the clock (a member with a wrong clock still joins); a member
  that is removed from the stack is rejected because its key is no longer listed, not because a certificate ran out.
* **Joining**: a new switch with designated stacking ports announces itself on them. `request virtual-chassis join token <t>`
  on the new switch, or `request virtual-chassis member add <id> token <t>` on the stack, authorises it. It then receives its
  certificate and the configuration.
* **Mastership can move at any time.** No member is special: the stack key and the configuration are on every
  member, so any member can be master (the leader that coordinates commits), and the member that created the stack
  can be removed like any other.
  * `request chassis routing-engine master switch [member <id>]` (Junos VC command) hands mastership to another
    member (default: the next by `mastership-priority`). A running commit finishes first; forwarding is not affected.
  * `request virtual-chassis member remove <id>` decommissions a member: if it is master, mastership moves first; then
    its traffic is drained (LACP partners are told the links go away, stacking paths are rerouted), it leaves the
    quorum and the member list, and another member takes its vote if needed. Its entry in the stack's configuration
    stays until you delete it. The removed switch becomes **member 1 of a stack of its own** and keeps running with its
    last configuration, rewritten for that: its ports are renamed from `<old id>/<card>/<port>` to `1/<card>/<port>`
    (in `interfaces`, `interface-range`, routing instances, analyzers and the like), addresses and statements that
    belonged to other members are removed, and `virtual-chassis` and `mclag` statements are deleted. The stacking port
    designations stay (a local setting; `request virtual-chassis vc-port delete …` releases them), so it can join again. The stack keys are replaced by new ones (the old keys are gone from the switch) and the
    old stack no longer accepts it. The previous configuration store stays on the switch as `config.pre-leave-<time>`.
  * `mastership-priority 0` means "never master" (useful for a switch that is about to be replaced). The member with
    the highest priority becomes master when it is available, but a working master is only replaced by an explicit
    switch (no flapping when a higher-priority member reboots).
* Stack control needs a majority of members (Raft). Without a majority, the data plane keeps forwarding with the last
  committed configuration, and only commits are blocked.
  * `request virtual-chassis force-master` (super-user, after a `[yes,no] (no)` question) is the explicit override for a
    stack that has lost its majority for good, typically a two-member stack whose other member is dead: this member
    forms the voting group alone and becomes master, so commits work again. switchd restarts once (forwarding
    continues, as with any switchd restart). It is refused while the stack has a master. The other members stay in
    the member list but do not vote while they cannot be reached; when one returns, it takes over this member's
    configuration history (what it committed alone is dropped) and votes again. **Risk**: if the other members are
    still running and only the cables are cut, both sides can commit different configurations, and one side's
    changes are lost when they meet. The override is logged (`change-log`).
* **Maintenance mode** takes one member out of service without losing traffic, e.g. before an update, a reboot or
  re-cabling: `request system maintenance-mode enter [force] [member <id>]` (super-user, after a `[yes,no] (no)`
  question) and `request system maintenance-mode exit [member <id>]`. Entering **drains** the member:
  1. It announces maintenance mode in its stack topology announcements. The other members stop routing stack traffic
     through it wherever another path exists (a ring routes around it; in a chain it still carries transit, and the
     command says so), and it is never chosen as master.
  2. If it is master, mastership moves to the reachable voter with the highest priority (it keeps its vote).
  3. Its MC-LAG legs are held out of their bundles. The peer hears first that the leg goes away (it lets unicast from
     the stack out on its own leg; broadcast and multicast stay filtered while the leg drains, 5.6), 300 ms later this member sends its traffic for the bundle through the peer, and
     LACP tells each partner "not in sync". A port leaves the bundle only once the partner has stopped sending on it
     (at most 2 s), so frames already on the way still arrive.
  4. The command waits (at most 30 s) and then reports `drained`, or what is still carrying traffic. Ports that only
     this member serves (single-homed access ports, bundles without `mclag`) cannot be drained; they are listed as
     `not drained: …`.

  Entering is refused when it would cut traffic that has somewhere else to go only through this member: an MC-LAG
  bundle whose leg on the peer is down (`error: ae1: the peer's leg is down, draining would cut the bundle`), or a
  peer that is itself in maintenance mode. `force` enters anyway. Maintenance mode survives a reboot of the member
  (it comes back drained) until `exit`. Exiting ends the announcement and releases the legs after 2 s (the MAC tables
  are exchanged meanwhile); after a reboot, `delay-restore` still applies. Mastership does not move back by itself.
  Both commands are logged (`change-log`). `show virtual-chassis` shows the member's status as `maintenance`, and
  `show mclag` shows the held legs with the reason `maintenance mode`.
  `request system reboot|halt|power-off` and `request virtual-chassis member remove` drain the member first in the
  same way (without the refusals: the member goes away anyway) and then continue.
* **Configuration mode runs on the master**, as in Junos VC: `configure` on any member opens the configuration
  session on the master (the prompt shows the master's host name). In a stack with more than one member, the line
  above the prompt shows the role of the member the session runs on: `{master:1}`, `{backup:2}`, `{linecard:3}`,
  or `{no-master:2}` without a master. Every command runs on the master (1.8). Without a master (no majority),
  `configure` fails with `error: configuration unavailable: no master (the stack has no majority)`; operational
  commands keep working on the member you are logged in to.
* **The stack is one switch.** Everything that lists or selects interfaces covers **every member** and prints **one
  table** (full interface names carry the member): `show interfaces`, `show chassis hardware`, `show virtual-chassis
  vc-port|mtu`, `show system offload`, `show vlans`, `show ethernet-switching table`, `show arp`, `show ipv6 neighbors`,
  `show lacp interfaces|statistics`, `show lldp neighbors|statistics`, `show dhcp client binding`, `show mclag`,
  `show igmp|mld snooping membership|vlans`, `show vxlan [remote-vtep]`,
  `clear ethernet-switching table`, and the completion of
  interface names. A target at the end only narrows the rows: `member <id>`, or `local` (the member you are logged in
  to). The MAC table lists an address where it was learned (not the copies on the stack tunnels); an MC-LAG bundle's
  ports on both members are one bundle. A member that does not answer is named in a warning above the table.
* **Operational commands about one member's state** accept a target at the end: `member <id>`, `all-members`, or
  `local` (the member you are logged in to; without a target, the master). With more than one target the output has
  a section per member (`member2:` and a line). The command runs on the member as the same user and class.
  * Commands with targets: `show system uptime|ntp|syslog|limits|bottlenecks`, `show version`, `show log`, `show route`,
    `request system reboot|halt|power-off`, `request system maintenance-mode enter|exit`,
    `clear system reboot`, `request chassis card`.
  * `request system reboot all-members` asks once, naming the members, and reboots the other members before this
    one. (Junos reboots all members by default; here the default is the local member.)
  * A member that cannot be reached is reported in its section (`error: member 3 is not reachable`); the others
    still answer.
* `commit` prints the result per member (`member1: commit complete`, `member3: not reachable, applies the
  configuration when it returns`). A member that fails to apply makes every member return to the previous configuration.
* When mastership moves, open configuration sessions end with a notice; the shared candidate is kept.
* Configuration-mode commands and commits are logged by the master (as in Junos VC), with the user; operational
  commands by the member they run on.
* `show virtual-chassis` roles: `master` (Raft leader), `backup` (the voter with the highest priority after the
  master), `linecard` (all others); per member also `voter` or `non-voter`.

#### Data between members (stack tunnels)

Client traffic between members travels over the stacking ports, inside **stack tunnels**. Nothing about them is
configured; they follow from the member list and the stacking cables.

* **Cabling: a ring.** Two members use two cables between them; three or more are cabled `1–2`, `2–3`, …, `n–1`. Every
  single cable can fail without losing a member. Chains and other meshes work too, with less redundancy.
* **Underlay**: every member has an internal address `169.254.64.<member>` in a hidden routing instance (`swstack`)
  that contains only the stacking ports. switchd routes between the members' addresses over the stacking links along
  the shortest paths of the stack topology it already knows (5.2 protocol, `show virtual-chassis`). Equal paths are
  used together (two members: both cables carry traffic). When a link fails (stacking BFD, loss of carrier), the
  routes move to the remaining paths within the BFD detection time. No ARP runs on stacking ports: the neighbour's
  address is bound to the MAC address the stacking protocol learned on that link. The instance is not visible in
  `show route` and can be neither configured nor reached from the data or management planes.
* **Tunnels**: every member has one VXLAN tunnel to every other switch member (UDP 4789 inside `swstack`, one VNI
  per pair of members). Each tunnel is a port of the member's bridge and carries, **tagged**, every VLAN that exists on both ends. A frame
  therefore always arrives on the tunnel of the member it came from:
  * Tunnels never forward to other tunnels (split horizon: flooded traffic is replicated by the member where it
    entered the stack, to every other member). No frame can loop, whatever the cabling, and RSTP never sees tunnels.
  * MAC addresses are learned on tunnels like on ports (`show ethernet-switching table` shows the tunnel as `vc-<member>`),
    except on the tunnel to the MC-LAG peer (5.6).
  * The outer IPv4 header has "don't fragment" set: a frame that is too large is dropped and counted, never
    fragmented. The MTU rule below makes sure that this never happens to a frame a data port accepted.
* **Stack MTU.** A frame between members carries 58 bytes on top of its own size on the stacking link: the tunnel
  (outer Ethernet 14, IPv4 20, UDP 8, VXLAN 8 = 50), the VLAN tag inside the tunnel (4) and one more VLAN tag of the
  frame itself (4: a QinQ customer tag or a host's own tag, which every port also accepts, 1.3).
  * switchd sets every stacking port to the largest MTU its NIC supports (up to a frame size of 16058) when the port is
    designated, and never changes it because of a commit (an MTU change can restart a link). Ports designated by an
    older version are set once when the new version starts.
  * E: the largest `mtu` of any switched interface, bundle or VLAN in the stack plus 58 is larger than the largest frame of
    some member's stacking port. The message names the member and the port, and the largest `mtu` the stack can
    carry. Example: stacking NICs with a maximum frame size of 9216 carry data `mtu 9158`, so hosts up to MTU 9144;
    hosts with MTU 9000 (`mtu 9014`) need 9072 on the stacking ports, which any NIC with jumbo frames supports.
  * A member whose NICs are not known yet (not joined) is checked when it joins: then the stacking ports that cannot
    carry the stack MTU raise an alarm, and `show virtual-chassis mtu` shows them.
  * Frames that are larger than the stack MTU are not fragmented: they are dropped where they enter the tunnel, exactly
    like on a port whose MTU is too small, and counted.
* `show virtual-chassis mtu`: the largest data `mtu` in the stack and where it is configured, the frame size the
  stacking links need for it, the largest data `mtu` (and host MTU) the stacking ports allow, and per stacking port
  its current MTU, its NIC maximum, the frame size **verified** on the cable (probe frames, see
  stack-protocol.md "Path MTU") and whether it suffices. All values are frame sizes (1.3).
  * W (`commit`, `commit check`): a stacking cable of this member carries less than the largest data `mtu` + 58
    needs (verified with probe frames); the message names the port and the `mtu` that would fit.
  * The stacking protocol itself uses frames of at most 1500 bytes, so it works over any cable. A cable that does not
    carry the frames the tunnels need (a bridge, converter or switch with a smaller MTU in between) shows
    `the cable carries only <n>` and a warning is logged; frames larger than that are lost, and only jumbo traffic
    is affected.
* No VLAN is reserved for the stack: management between members runs in the stacking protocol itself (1.8), not in
  a VLAN. (Earlier versions reserved VLAN 4094; it is an ordinary VLAN now.)

#### `virtual-chassis bfd { minimum-interval <ms>; multiplier <n>; }`
Failure detection on every stacking link, BFD-style inside the stacking protocol (IP-less): each side sends a frame
at least every `minimum-interval` (data frames count), and a link is declared down after `minimum-interval ×
multiplier` without any frame from the peer. Defaults: 100 ms × 3 = 300 ms. Both sides use the larger of the two
configured intervals (they announce theirs in the handshake), so a slow member is not declared dead by a fast one.
* BFD runs with real-time scheduling priority, so CPU load does not cause false detections.
* Values below 100 ms can still cause false detections on small ARM boards. A false detection makes stacking paths
  re-route, but never drops data traffic by itself.

#### `virtual-chassis member <1-16> { … }`
Declares a stack member and its per-member settings. Configuration for a member that has not joined yet is kept and
applied when it joins. Without any `virtual-chassis member` entry the switch is standalone member 1, and interfaces of other
members are rejected (E).
* `host-name <hostname>`: sets the Linux host name and the CLI prompt of this member.
  E: the same host name on two members.
* `mastership-priority <0-255>`: default 128. The highest priority healthy member becomes the stack leader (it coordinates
  commits). Of an MC-LAG pair the higher priority member is *primary* (ties: lower member id).
* `role switch|witness`: a `witness` member only takes part in stack quorum, over its own stacking cables. It keeps
  a two-switch stack able to commit when one switch is down. E: interfaces configured on a witness.
  * switchd does not manage a witness's data plane: no bridge, no switch ports; its NICs stay with the OS (a switch
    that becomes a witness releases its ports and routed interfaces; the empty bridge device stays until reboot). Accounts, SSH, host name and syslog are managed as usual.
  * A witness never stays master (its `mastership-priority` counts as 0) and is always among the voters in stacks
    of up to 7 members.

### 5.3 interfaces

#### 5.3.1 `interface-range <name> { member [ <pattern> … ]; member-range <from> to <to>; <interface statements> }`
Applies one block of interface statements to many ports. It takes every statement that is valid below
`interfaces <if>` (described in 5.3.2). Use it for large port counts and for NICs that are added later.
* `member-range 1/0/0 to 1/0/23`: every port between the two ends. Both ends must be on the same member and card.
  Ports that do not exist yet are configured when they appear.
* `member "<m>/<c>/<p>"`: a wildcard. Each of the three parts is a number, `*` (any) or a range `[<a>-<b>]`:
  `1/0/*` (all ports of card 0 on member 1), `*/1/[0-3]`, `*/*/*`. Quote the value: `member "1/0/*"`.
  * Wildcards are evaluated against the ports that exist, **at commit time and whenever a NIC appears**. A newly
    plugged NIC matching a wildcard is configured immediately, without a commit, and this is logged.
  * If the new port cannot take the configuration (e.g. the MTU exceeds its hardware maximum), it stays unconfigured
    and an alarm is raised.
  * Wildcards **never** select stacking ports or management ports (`management`).
* **Precedence**: a port that is also listed under `interfaces` uses its explicit statements. The range fills in only
  what the explicit entry does not set:
  * Leaves: the explicit value wins.
  * Leaf-lists: the explicit list replaces the range list; they are not combined.
  * Mutually exclusive statements: an explicit choice (e.g. `no-flow-control`) suppresses the range's alternative.
* E: a port selected by two ranges. W: a range that selects no port.
* `show interfaces` shows which range configured a port.

#### 5.3.2 `interfaces <interface-name> { … }`
`interfaces <interface-name> { … }` configures a physical port (`<member>/<card>/<port>`) or an aggregated
interface (`ae<N>`). As described in 1.4, only listed interfaces are managed. A listed interface is brought
administratively up unless it has `disable`.

What a port does depends on which statements are present:

| Configured | Role of the port |
|---|---|
| `unit 0 family ethernet-switching` | **Switch port**: member of the bridge, forwards according to its VLAN settings. |
| `ether-options 802.3ad aeN` | **Bundle member**: carries traffic for `aeN`. Switching settings belong on `aeN`. |
| neither | **Plain port**: up, MTU applied, not switched. Used as a mirror destination. W: a plain port that is not a mirror output (it carries nothing and runs no RSTP; usually `unit 0 family ethernet-switching` is missing). |

#### `description <text>`
Free text shown in `show interfaces`.

#### `disable`
Administratively down: the link is taken down, so the neighbour sees link loss. On a bundle member, the port leaves the
bundle. On an `ae`, the whole bundle is down on all members. The configuration below stays in place.

#### `mtu <256-16000>`
Maximum frame size including the Ethernet header (1.3). Default 1514.
* The same value is used for receiving and sending. Frames larger than the MTU are dropped: on receive by the NIC,
  on send by the bridge. Both are counted in `show interfaces extensive` (`mtu-exceeded`).
* Bundle members always use the MTU of their `ae`. W: a different `mtu` on a member port (it is ignored).
* E: MTU above the NIC's hardware maximum (known once the member has reported its inventory). The maximum is
  shown in the same convention (kernel maximum + 14).
* W: the port belongs to a VLAN whose `mtu` is larger than the port MTU. Such frames are dropped at this port.
* The effective limit for a frame is **min(ingress port MTU, VLAN MTU, egress port MTU)**. See also `vlans <v> mtu`.

#### `ether-options { 802.3ad <aeN>; flow-control | no-flow-control; }`
Only on physical ports (E on `ae`).
* `802.3ad <aeN>`: makes the port a member of `aeN`. E: `aeN` is not configured. E: the port also has
  `unit 0 family ethernet-switching`, `storm-control` or `mac-limit` (configure those on the `ae`).
  Still effective on a member port: `description`, `disable`, `ether-options flow-control`, `offload disable`.
* `flow-control` / `no-flow-control` (mutually exclusive): enable or disable IEEE 802.3x pause frames. Pause frames
  let a congested receiver slow the sender down instead of dropping frames. That helps against drops, at the cost of
  head-of-line blocking. Default: leave the driver's default. W: the NIC has no pause-frame support (1.7).

#### `aggregated-ether-options { … }`
Only on `ae` interfaces (E on physical ports). An `ae` without member ports is W, and stays down.
* **Without `lacp`**: a *static* bundle. Every member port with link is used. If the partner is not configured as a
  static bundle as well, this can cause drops or loops, so LACP is recommended.
* `lacp { active | passive; periodic fast|slow; system-priority <n>; }`: IEEE 802.1AX LACP.
  A port only carries traffic after LACP negotiation succeeds.
  * `active` (default) sends LACPDUs, while `passive` only answers. Two passive ends never form a bundle.
  * `periodic fast` (default; Junos defaults to slow) asks the partner to send every second, so a failed partner is
    detected within 3 seconds. `slow` means 30 seconds and 90 seconds.
  * `system-priority` (default 32768). The LACP system id is **one MAC for the whole stack**, derived from the stack
    id (locally administered; stable across restarts, port changes and mastership changes; shown by `show lacp
    interfaces`). The stack is one switch, so every bundle announces the same system, whichever members its ports
    are on: a bundle that gains ports on a second member (5.6) keeps its identity.
  * Actor identity: key `N+1` for `aeN`; port number `member × 1024 + card × 64 + port` (unique in the whole stack, so
    the two members of an MC-LAG never announce the same port); port priority 32768. The number is 16 bits, so a
    port of an LACP bundle must be on card 0-15 with port number 0-63; a commit with any other port in an LACP
    bundle fails (static bundles take every port).
  * A member port carries traffic only while LACP has it *collecting and distributing* (in sync with the partner). A
    port that is not receives nothing but LACPDUs (data frames on it are dropped by the switch, as IEEE 802.1AX
    requires), and nothing is sent on it. Ports whose partner differs from the bundle's partner (a cabling error) stay
    out of the bundle and are shown as `not selected`.
  * A partner that sends only every 30 seconds although `periodic fast` asks for every second (some switches ignore
    the request) lets our side time out after 3 seconds again and again: the port flips between `Current` and
    `Expired`/`Defaulted` and the bundle never forms. switchd notices the long gaps between the partner's LACPDUs,
    logs a warning and shows it in `show lacp interfaces`, with the hint to configure `periodic slow`.
  * With LACP, `minimum-links` counts distributing ports; below it the bundle is down.
  * `show lacp interfaces [<aeN>]`: per member port the actor and partner state (activity, timeout, aggregation,
    synchronization, collecting, distributing, defaulted, expired) and the receive and mux machine states, as in
    Junos. `show lacp statistics interfaces [<aeN>]`: LACPDUs sent and received, and received errors, per port.
* `minimum-links <n>`: the bundle is operationally down while fewer than n member ports are active. On an MC-LAG
  bundle this counts ports on **both** members. Default 1. W: n larger than the number of configured member ports.
* `hash-policy layer2|layer2+3|layer3+4`: how flows are spread across the active members of **this** switch.
  Default `layer3+4`. Non-IP traffic always uses layer 2. Frames of one flow always take the same link, so order is preserved.
* A bundle with member ports on two stack members is an **MC-LAG** (5.6). E: ports on more than two members. E: ports
  on two members without `lacp`.

#### `storm-control { broadcast <pps>; multicast <pps>; }`
Rate-limits flooded traffic **received** on this port, in packets per second. Frames above the rate are dropped
*intentionally* and counted. `broadcast` covers `ff:ff:ff:ff:ff:ff`. `multicast` covers all other group addresses
**except** the IEEE link-local range `01:80:c2:00:00:0x`, so BPDUs and LACPDUs are never rate-limited.
It uses hardware policers where available and is enforced in software otherwise. Without this statement nothing is limited.
(Unknown-unicast storm control is not offered: Linux can only tell whether a unicast frame is unknown after the MAC
lookup, which a reliable ingress policer cannot see.)

#### `mac-limit <n>`
Maximum number of MAC addresses **learned** on this port. Once reached, new source addresses are *not learned*,
but their frames are **still forwarded**: replies to them are flooded instead of switched. An alarm is raised.
The switch never drops traffic because of the limit. Default: unlimited.
* Linux has no per-port learning limit (only a bridge-wide one), so switchd enforces it: it counts the learned
  entries of the port and switches learning off on the port when the limit is reached, and on again when entries
  have aged out. Because this reaction is not instantaneous, the count can briefly exceed the limit by a few entries.

#### `offload disable`
Forces software forwarding for this port: no switchdev or tc hardware offload. See `system offload`.

#### `native-vlan-id <vlan>`
The **untagged VLAN of a trunk port** (one VLAN name or id).
* Untagged and priority-tagged (VID 0) frames received on the port belong to this VLAN.
* Frames of this VLAN leave the port **untagged**.
* Tagged frames with this VLAN's id are also accepted on ingress.
* The native VLAN is automatically a member of the trunk. Listing it in `vlan members` as well is allowed and changes nothing.
* E: used without `unit 0 family ethernet-switching`. E: used with `interface-mode access`.
* E: the VLAN is not defined.
* Without `native-vlan-id`, a trunk **drops** all untagged frames.

#### `unit 0 family ethernet-switching { interface-mode access|trunk; vlan members [ <vlan> … ]; }`
Makes the port a switch port. As in Junos (ELS), `family ethernet-switching` exists only on unit 0; other units
are routed subinterfaces (below).
* `interface-mode access` (**default**): the port belongs to exactly **one** VLAN, untagged.
  * Received untagged or priority-tagged frames → the member VLAN. **Received tagged frames are dropped**
    (also when the tag equals the access VLAN, which is stricter than Linux' default).
  * Frames are sent untagged.
  * E: more than one VLAN (including a range or `all` that expands to more than one).
  * W: no VLAN (the port drops everything; there is no implicit default VLAN).
* `interface-mode trunk`: the port carries all VLANs in `vlan members`, **tagged**, plus the optional `native-vlan-id` untagged.
  * Received tagged frames of a member VLAN are accepted. Other tags are dropped.
  * Untagged frames are accepted only with `native-vlan-id`.
  * W: a trunk with no VLANs.
* `vlan members`: VLAN names, ids, ranges (`100-199`) or `all`.
  * Every VLAN id must be defined under `vlans` (E, listing the undefined ids).
  * `all` means every VLAN defined under `vlans` at commit time, so later VLANs are added automatically.
* Frames with an 802.1ad S-tag (`0x88a8`) are not interpreted and are treated as untagged payload.
* Switch ports take part in RSTP when `protocols rstp` is configured (5.5).

#### `unit <n> family inet|inet6 { address <address/prefix>; }`, `vlan-tagging`, `unit <n> vlan-id <id>`
**Routed interfaces** (layer 3) on a physical port or `ae` interface, instead of switching:
* **Routed port**: `unit 0 family inet address 10.1.1.1/30` on a port that is not a switch port. The port is not in
  the bridge. Untagged frames are routed; tagged frames are dropped.
* **Routed subinterfaces**: `vlan-tagging` on the port, then `unit <n> { vlan-id <id>; family inet { address …; } }`
  for n = 1–16385. Each unit takes the frames with its tag. Untagged frames are dropped (unless unit 0 is also routed).
* `family inet` and `family inet6` each take several `address <address/prefix>` entries. `family inet6` also gets a
  link-local address. `family inet dhcp` takes the IPv4 address from DHCP instead:
  * switchd runs the DHCP client itself (RFC 2131: discover, request, renewal at T1 to the server, rebinding at T2
    by broadcast, release when the statement is removed). The client identifier is the interface's MAC. On an irb
    unit every member runs its own client with its own MAC and gets its own address (no anycast address).
  * The leased address is added to the unit; switchd's removal of unconfigured addresses leaves it alone.
  * The router option becomes the default route of the unit's routing instance, unless that instance has another
    `0.0.0.0/0` (a static route or one from a routing protocol wins). The route goes away with the lease.
  * The DNS servers and domain from the lease are only shown; the switch's own resolver uses `system name-server`.
  * When the lease expires without renewal, the address and route are removed and the client starts over.
  * `show dhcp client binding [<interface>]`: per unit and member the state (`selecting`, `requesting`, `bound`,
    `renewing`, `rebinding`), address, server, router, DNS servers, lease time and time until renewal and expiry.
  * E: `dhcp` together with an IPv4 `address` on the same unit.
* E: `family ethernet-switching` together with `family inet|inet6` on the same unit, or `family ethernet-switching`
  on a unit other than 0, or on a port with `vlan-tagging`.
* E: a unit other than 0 without `vlan-tagging`, or without `vlan-id`. E: the same `vlan-id` on two units of a port.
* E: a routed interface on a bundle member port (route on the `ae` instead).
* E: the same address (or overlapping subnets) on two interfaces of the default instance. E: a network or broadcast
  address as interface address (except /31, /32, /127, /128).
* A unit's `mtu` is the port's `mtu` (the frame size); the IP MTU is 14 bytes less (18 on a subinterface).
* In the kernel a routed subinterface is a VLAN device named `sw-<card>-<port>.<unit>` (Linux names cannot contain `/`).

#### 5.3.3 `interfaces irb unit <n> { description <text>; disable; family inet|inet6 { address <address/prefix>; } }`
**VLAN IP interfaces** (IRB, integrated routing and bridging): an IP interface inside a VLAN, so the switch has an
address in that VLAN and **routes between VLANs** (and routed ports) in the default routing instance.
* `irb.<n>` is attached to a VLAN with `vlans <v> l3-interface irb.<n>`. n is any number 0–16385; using the VLAN id
  keeps it readable (`irb.10` for VLAN 10). W: an irb unit that no VLAN references (it has no effect).
* Addresses follow the same rules as routed interfaces (above), with one addition: an irb address can belong to a
  single member, `address 10.5.176.95/16 { member 1; }` (set form `… address 10.5.176.95/16 member 1`), e.g. for a
  routing protocol that needs one address per member. Addresses without `member` exist on every member (anycast
  gateway). E: `member` in the management instance (1.8). E: `member` on an address of a port or `ae` (those belong to one member already).
* In a stack, the irb interface exists on **every member that has the VLAN**, with the same addresses and the same MAC
  address (derived from the stack), so every member routes locally (anycast gateway). With MC-LAG, both peers
  answer for the gateway address.
  The MAC is derived from the stack id (locally administered). Duplicate address detection is off on irb units,
  because every member holds the same IPv6 addresses.
* An irb unit can be an in-band management interface: put it into the management instance (5.9). Its addresses
  then exist only on the master (1.8).
* In the kernel, `irb.<n>` is a VLAN device on the bridge, and the bridge itself joins the VLAN (bridge self VLAN).

**Routing** happens only between switchd's own L3 interfaces (irb, routed ports and subinterfaces): IPv4 forwarding is
enabled per interface on them only; `cme` does not forward. For IPv6, Linux can only enable forwarding for the whole
system, so switchd sets `accept_ra 0` everywhere (routes come from the configuration).
* ICMP redirects are not sent. Reverse-path filtering is loose (`rp_filter 2`) on routed interfaces.

#### 5.3.4 `interfaces <port> management`, `interfaces cme unit 0 { description <text>; family inet|inet6 { address <address/prefix>; } }`
**Chassis management interface** (1.8), the counterpart of Junos' `vme`.
* `interfaces <port> management` makes a physical port of any member a management port. The port carries only `cme`:
  it is never bridged, takes no `unit`, `ether-options` or VLANs (E), and wildcard ranges skip it. `description`,
  `disable` and `mtu` are allowed. switchd keeps it up with IPv6 disabled; only the master puts `cme` on it.
* `interfaces cme unit 0` holds the stack's management addresses (IPv4 and IPv6, several allowed). Only unit 0 (E
  otherwise). `cme.0` must be in the management instance (5.1 `system management-instance`, E otherwise).
* W: `cme` has addresses but no member has a management port.
* The MAC address of `cme` is derived from the stack id (locally administered, different from the irb MAC).
* `show interfaces cme` names the master and the port; `show interfaces` marks management ports as `management`
  with `active` on the master's port.

Example:
```
set interfaces 1/2/0 management
set interfaces 2/2/0 management
set interfaces cme unit 0 family inet address 10.5.20.76/16
set routing-instances oob interface cme.0
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 10.5.0.1
set system management-instance oob
```

### 5.4 vlans

`vlans <name> { … }` defines a VLAN (a broadcast domain). The name is what you reference in `vlan members`.

#### `vlan-id <1-4094>`
802.1Q id. E: missing. E: the same id in two VLANs.
1–4093 are available. A VLAN exists on a member's bridge only if a port of that member or VXLAN needs it; the stack
tunnel between two members carries the VLANs that both of them have.

#### `description <text>`
Free text.

#### `mtu <256-16000>`
Maximum frame size (1.3) **within this VLAN**, independent of port MTUs. Use it for example to allow jumbo frames
only in a storage VLAN (`mtu 9014` for 9000-byte hosts) while the trunks carry `mtu 9216`.
* Frames larger than this are dropped when they are **received** (on any port or tunnel), and counted per VLAN in `show vlans extensive`.
* Implemented as an nftables bridge filter on receipt (forwarded or delivered to the switch): tagged frames by their
  VLAN id, untagged ones by the VLAN of their ingress port. It costs a little CPU per frame in software.
  Without `mtu` no VLAN filter is installed, and only port MTUs apply.
* W: a member port with a smaller MTU (frames that fit the VLAN are dropped at that port).
* If the VLAN is extended over VXLAN, the tunnel MTU follows the largest VXLAN VLAN MTU (5.7).

#### `l3-interface irb.<n>`
Attaches the VLAN IP interface `irb.<n>` (5.3.3) to this VLAN. E: the irb unit is not configured. E: the same irb unit
on two VLANs.

#### `vxlan vni <vni>`
Extends the VLAN over VXLAN to the remote VTEPs that list the VNI (5.7). E: VNI used twice. E: no
`switch-options vxlan source-address`. The stack tunnels carry the VLAN between members as for any VLAN.

### 5.5 protocols

#### `protocols rstp { … }`
Enables Rapid Spanning Tree (IEEE 802.1w) on **all** switch ports of all members: every interface with
`unit 0 family ethernet-switching`, and every `ae` (a bundle is one RSTP port; its member ports never run RSTP themselves).
`interface` entries only tune individual ports.

`show spanning-tree interface <if>` explains why a port is not an RSTP port (not configured, a bundle member: RSTP
runs on its `ae`, or not a switch port). These are never part of RSTP:
* **stack tunnels**: loop-free by design (5.2),
* **VXLAN tunnels**: the VXLAN mesh is loop-free by design (5.7),
* **plain ports**.

**The stack is one RSTP bridge** (as a Junos virtual chassis): every member uses the same bridge id, and neighbours
see one switch whatever member their cable ends on. A cable between two ports of the stack is a loop of that one
bridge, and one of its ends becomes a backup port (discarding).
* **Bridge id**: `bridge-priority` and a MAC derived from the stack id (locally administered, stable: it does not
  change when members join, leave or fail). `show spanning-tree bridge` shows it.
* **One decision point**: the RSTP state machines of all ports of all members run on one member, the **RSTP owner**:
  the member with the lowest id among the members it reaches (members in maintenance mode only if no other is
  reachable). BPDUs received on any member are relayed to the owner over the stacking links (milliseconds), and the
  owner sends each member its port states and the BPDUs to transmit. The owner copies its complete state (roles,
  port states, received information, timers) to every member on each change and every second. When the owner
  changes (it fails, reboots, or a member with a lower id joins), the next one continues from that copy: no port
  changes state, no BPDU is missed by the neighbours (they allow 3 × `hello-time`), and nothing is reconverged.
  Until a member hears from an owner, its ports keep the states they have.
* A stack that splits into parts that cannot reach each other runs one owner per part, all with the same bridge id.
  If the parts are still connected through other switches, each part receives BPDUs with its own bridge id from the
  other part, and those ports become backup ports (discarding): the split cannot form a loop.
* **MC-LAG**: an MC-LAG bundle is **one** RSTP port with one role and state, applied to its legs on both members.
  BPDUs from the partner arrive on either leg; BPDUs to the partner leave on one leg that is up (the leg of the
  member with the lowest id). The port's path cost follows the speed of all active legs of both members. One leg
  failing, or one member of the pair failing, is no topology change for RSTP.
* **Stack tunnels** are the bridge's internal fabric: always forwarding, never sending or receiving BPDUs. When RSTP
  runs, BPDUs are consumed on every switch port and never forwarded (not even between members).
* Topology changes flush the learned addresses of the affected ports on every member.
* switchd restarts and upgrades are hitless: port states stay in the kernel and the member takes its state from the
  owner (or, as owner, from its last copy).

Without `protocols rstp` (or with `disable`), the switch does not run STP and **forwards BPDUs transparently**
(it floods them like other multicast). A loop through this switch is then still detected by the neighbours' STP.

* `bridge-priority <0-61440, step 4096>`: default 32768. Lower wins the root election.
* `hello-time <1-10>` (default 2), `max-age <6-40>` (default 20), `forward-delay <4-30>` (default 15), in seconds.
  E: violating `2 × (forward-delay − 1) ≥ max-age ≥ 2 × (hello-time + 1)`.
* `disable`: RSTP off, with the configuration kept.
* `interface <interface-name> { … }`:
  * `cost <n>`: path cost. Default: automatic from link speed (20 000 000 / Mbit/s; 10G = 2000, 1G = 20000).
    For an `ae` it is based on the speed of its active members, and it changes when members fail.
  * `priority <0-240, step 16>`: default 128.
  * `edge`: the port faces an end host and starts forwarding immediately. If it receives a BPDU, it loses edge status
    and runs RSTP normally (or is shut down by `bpdu-block`).
  * `no-root-port`: root guard. If a superior BPDU arrives, the port is blocked (root-inconsistent) until those BPDUs
    stop, so a foreign switch can never become root through this port.
  * `mode point-to-point|shared`: link type. Default: automatic (full duplex = point-to-point, which is needed for rapid transitions).
  * `disable`: the port does not run RSTP. It is **always forwarding**, and BPDUs received on it are dropped.
    Use it only when you are sure the port cannot form a loop.
  * E: the interface is not configured or is a bundle member. W: it is not a switch port.

Operational commands:
* `show spanning-tree bridge`: this bridge's id, the root bridge id, root path cost and root port, the timers in use,
  the RSTP owner member, time since the last topology change and the number of topology changes.
* `show spanning-tree interface [<if>] [detail]`: per RSTP port (full names, e.g. `2/0/3`, `ae1`): role (`root`,
  `designated`, `alternate`, `backup`, `disabled`), state (`forwarding`, `learning`, `discarding`), cost, priority
  and port id, designated bridge and port, edge (`edge` configured, `oper-edge`), link type, protocol (`rstp`, or `stp`
  when the neighbour speaks 802.1D only), and flags (`root-inconsistent`, `bpdu-blocked`). `detail` adds BPDU counters.
* `show spanning-tree statistics`: BPDUs sent and received per port, topology changes.
* `clear spanning-tree protocol-migration [interface <if>]`: send RSTP BPDUs again on ports that fell back to 802.1D
  (the neighbour was replaced).
* `clear spanning-tree statistics`.

#### `protocols lldp { disable; interface <interface>|all { disable; }; advertisement-interval <5-32768>; hold-multiplier <2-10>; }`
Link Layer Discovery Protocol (IEEE 802.1AB). The stack presents itself as **one system**: every member sends with the
same chassis ID and system name, so a neighbour sees one switch with many ports, whichever member a cable is on.
* `set protocols lldp` runs LLDP on every configured port (switch ports, routed ports, bundle member ports and
  management ports; never on stacking ports). `interface <interface>` (a port, or an `ae` for all its member ports)
  restricts it to the listed ones once one is listed; `interface all` is the default. `interface <if> disable` excludes one.
  `disable` stops LLDP everywhere.
* Timing: an LLDPDU every `advertisement-interval` seconds (default 30), and at once when a port comes up or what it
  announces changes; time to live = interval × `hold-multiplier` (default 4, i.e. 120 s). A port going down or
  losing LLDP sends a shutdown LLDPDU (TTL 0).
* What each port announces:
  | TLV | Value |
  |---|---|
  | Chassis ID | MAC address (subtype 4), derived from the stack id, the same on every member |
  | Port ID | interface name (subtype 5), e.g. `2/0/3` |
  | Time to live | as above |
  | Port description | the interface's `description`, else its name |
  | System name | `system host-name` (the chassis name), else the member's host name |
  | System description | `cerOS <version>` |
  | System capabilities | bridge (and router when routed interfaces exist); enabled as configured |
  | Management address | the addresses of `cme` (1.8), when there are any |
  | Port VLAN ID (802.1) | the untagged VLAN of a switch port (on a bundle member port: of its `ae`) |
  | Link aggregation (802.3) | for bundle member ports: aggregation capable; whether LACP has the port in the bundle now (collecting and distributing; a static bundle: the port is up); the aggregated port id `N+1` for `aeN`, the same on every member (an MC-LAG looks like one bundle) |
  | Maximum frame size (802.3) | the port's `mtu` |
* Received LLDPDUs are kept per port until their time to live runs out (a shutdown LLDPDU removes them at once). At most
  8 neighbours per port; LLDP frames are never forwarded by the switch.
* `show lldp neighbors [interface <interface>]`: per port the neighbour's chassis ID, port ID, port description,
  system name and remaining time to live (one table for the whole stack, 3.5); with `interface`, every TLV of that
  port's neighbours, including capabilities and management addresses.
* `show lldp local-information`: what the stack announces (chassis ID, system name and description, capabilities,
  management addresses) and the ports LLDP runs on.
* `show lldp statistics`: per port LLDPDUs sent and received, discarded and aged-out neighbours.

#### `protocols igmp-snooping { … }`, `protocols mld-snooping { … }`
IGMP snooping (IPv4) and MLD snooping (IPv6): the switch watches the group memberships hosts announce and sends a
group's traffic only to the ports with receivers and to the multicast-router ports, instead of to every port of the
VLAN. **Both are on by default** in every VLAN, also without the statements.
```
protocols igmp-snooping {
    disable;                         # off everywhere
    vlan <vlan>|all {                # per VLAN (all: the default for every VLAN)
        disable;
        querier;                     # send general queries in this VLAN
        version 2|3;                 # IGMP version of the queries (default 2; MLD: version 1|2, default 1)
    }
    interface <interface> {          # a switch port or ae
        immediate-leave;             # a leave removes the port at once (one receiver per port)
        multicast-router-interface;  # always send all group traffic of its VLANs here
    }
}
```
* `interface <interface> immediate-leave`: a leave message removes the port from the group at once, without the
  usual last-member queries. Only for ports with a single receiver behind them.
* `interface <interface> multicast-router-interface`: the port always receives all group traffic of its VLANs (a
  multicast router or another switch's uplink that snooping cannot detect).
* **Never dropped for lack of a querier.** Without a querier in a VLAN (no IGMP/MLD queries seen, and no `querier`
  here), group traffic is flooded in the VLAN as without snooping. Link-local groups (224.0.0.0/24, ff02::/16) are
  always flooded. Ports where queries (or multicast routing protocols) arrive become multicast-router ports by
  themselves.
* `querier`: this switch sends general queries in the VLAN when no other querier with a lower address is active. IGMP
  queries come from the VLAN's irb address (5.3.3), else from 0.0.0.0, which most hosts accept. MLD queries need an
  IPv6 link-local address: with an MLD querier the bridge keeps its link-local address (only for that purpose).
* **The stack**: every stack tunnel is a multicast-router port, so group traffic reaches every member that has the
  VLAN, and each member sends it only to its own ports with receivers. Membership reports cross the stack the same
  way, so every member knows the groups (`show igmp snooping membership` lists each where it was learned).
* **MC-LAG**: groups learned on an MC-LAG bundle are installed on the peer's leg too (with the leg states, over the
  stacking plane), so a leg failure loses no group until the next query; they are removed when they expire on the
  member that learned them.
* **VXLAN** ports (5.7) are multicast-router ports.
* **IGMP and MLD together**: the switch (the Linux bridge) snoops both in one: in a VLAN, both are on or both off
  (E otherwise), and `querier` in either sends IGMP and MLD queries in that VLAN.
* E: `interface` not configured or a bundle member port. W: not a switch port. The schema allows `version` 2–3
  (IGMP) and 1–2 (MLD).
* `show igmp snooping membership [vlan <vlan>]` / `show mld snooping membership …`: per VLAN and group the
  interfaces with receivers, the source filter (IGMPv3/MLDv2) and the time until expiry; one table for the stack.
* `show igmp snooping vlans` / `show mld snooping vlans`: per VLAN whether snooping runs, the querier (this switch,
  another address, or none: flooding) and the multicast-router ports.

#### `protocols layer2-control bpdu-block { interface [ <if> … ]; disable-timeout <s>; }`
BPDU protection. It works with or without RSTP. A listed port that receives any BPDU (STP/RSTP/MSTP, or Cisco PVST+
`01:00:0c:cc:cc:cd`) is **shut down immediately**, an alarm is raised, and a syslog `error` is sent.
* The port stays down until `clear error bpdu interface <if>`, or until `disable-timeout` seconds have passed (if set).
  The shutdown survives restarts of the daemons and of the switch software. The member the port belongs to watches
  it (cer-rstpd, also without RSTP); `clear error bpdu` runs there by itself.
* On an `ae`, the whole bundle is shut down.
* E: the interface is not configured or is a bundle member.
* W: the port runs RSTP as a non-edge port (any neighbouring switch would shut it down).

### 5.6 mclag

An **MC-LAG** is an `ae` bundle whose member ports are on **two** stack members. Nothing else declares it: the
ports say which two members carry the bundle (`set interfaces 1/0/1 ether-options 802.3ad ae1` and
`set interfaces 2/0/1 ether-options 802.3ad ae1` make `ae1` an MC-LAG of members 1 and 2). Towards the device on the
other end, both members are one LACP partner, as the stack is one switch (like a LAG across Junos Virtual Chassis
members). The two members of an MC-LAG are its **pair**; each is the other's **peer**.

Both members are members of the same stack, and everything between them runs over the stacking ring (5.2): the
stack tunnel between the two members takes the role that a dedicated peer-link has in other MC-LAG
implementations, and the stacking protocol carries MAC synchronisation, leg states and consistency checks. No extra
cable is needed. If the two are not cabled directly, both run through other members.

Rules (commit check):
* E: an `ae` with ports on more than two members.
* E: an `ae` with ports on two members without `aggregated-ether-options lacp` (a static bundle across members
  cannot tell the partner which links to use).
* E: a member that is paired with two different members (e.g. `ae1` on members 1 and 2, `ae2` on members 1 and 3).
  All MC-LAG bundles of a member have the same peer.
* A bundle whose ports are all on one member is an ordinary bundle, also while its second member's ports are being
  added (migration): it becomes an MC-LAG when the commit adds them. Its LACP identity does not change (below), so the
  partner does not re-negotiate.
* `mclag domain …` and `aggregated-ether-options mclag` of earlier versions are no longer needed. A stored
  configuration is converted when it is read: both are removed, and `mclag domain <n> delay-restore` becomes
  `mclag delay-restore`. (`system-mac`, `system-priority` and `anycast-vtep` of a domain are dropped: the stack has
  one LACP system id, see below; VXLAN comes with Phase 9.)

#### `mclag delay-restore <0-3600>`
After a member boots, its MC-LAG legs stay out of their bundles for this long (default 300 s). During that time the
MAC table is synchronised and RSTP converges before traffic is attracted. A member that only lost its connection to
the peer is not delayed (see failure handling). It applies to every MC-LAG of the stack.

**Behaviour:**

* **The peer's tunnel** (the stack tunnel between the two members) carries the VLANs like every tunnel, with two rules:
  * It does not learn addresses (a dual-homed device's frames cross it when they are flooded, which would make the
    peer point that device at the tunnel). MAC synchronisation fills it instead.
  * **Split horizon**: traffic that arrives over the peer's tunnel is never sent out of an MC-LAG bundle whose other
    leg (on the peer) is up: flooded traffic was already delivered by the peer on its own leg, and with MAC
    synchronisation known unicast for a dual-homed device only crosses to the peer while the sending member's leg is
    down. When a member's leg of a bundle fails, the peer lifts this filter for that bundle, and the traffic reaches the
    device via the peer. Leg changes reach the peer within 50 ms.
  * A leg never forwards while its peer lets tunnel traffic out on its own leg, or the partner's flooded frames
    (BPDUs among them, which the stack floods when RSTP is off) would come back to the partner on the same bundle,
    and the partner's spanning tree would block or shut down its ports. So a leg that comes up (after
    `delay-restore`, a link or LACP flap, a hold) is announced to the peer first; the peer installs the filter and
    answers, then LACP lets the first port carry traffic (without an answer within 300 ms, e.g. from an earlier
    software version, the leg forwards anyway). A leg going down is reported after it stopped forwarding.
  * While a leg drains for maintenance mode (below) the peer lets the member's unicast for the bundle out on its leg,
    but still drops broadcast and multicast from the tunnel towards it: the draining leg receives until the partner
    stops sending on it. (Unknown unicast the partner sends to the draining leg in that window, at most about 2 s,
    can reach the partner once more through the peer.)
* **Traffic from other members** (stacks with more than two members): flooded traffic from a third member reaches
  both members of the pair on their own tunnels. For every MC-LAG bundle only one of them delivers broadcast and
  multicast: the primary while its leg is up, otherwise the secondary. Known unicast is delivered by the member it
  was sent to. Unknown unicast from a third member can reach a dual-homed device twice until its address is learned.
  When a member's leg fails, the other members forget the addresses behind that bundle that point to this member,
  so their traffic floods and reaches the device via the remaining leg at once.
* **MAC synchronisation** (stacking plane):
  * A MAC learned on an MC-LAG bundle is installed on the peer on the same bundle (on the peer's tunnel while the
    peer's own leg of that bundle is down).
  * MACs learned on single-homed ports are installed on the peer pointing to the tunnel of the member that learned them.
  * An address a member learns itself replaces a synchronised one. A synchronised address is removed when it ages out
    on the member that learned it; if it is still in use on the peer, the peer learns it again at once. Addresses that
    point to the peer's tunnel are removed when the peer becomes unreachable.
  * The members exchange changes as they happen and their whole tables every 30 s and when they meet again.
* **Failure handling** (*primary* = higher `virtual-chassis member mastership-priority`, ties: lower id):
  | Situation | Behaviour |
  |---|---|
  | One stacking cable fails | Nothing visible: the stack tunnels move to the other way around the ring within the stacking BFD time. |
  | A member's leg of an MC-LAG bundle fails | The bundle continues on the other member. The peer lifts split horizon for that bundle, and MACs point to the peer. |
  | The peer is not reachable, **two-member stack** (peer dead, or both cables cut) | Both keep forwarding at all costs: every member keeps its legs up and keeps learning. Addresses that pointed to the peer are removed (their traffic floods locally). A device behind a bundle keeps working on both legs; a device connected only to the other member is unreachable while the stack is split. |
  | The peer is not reachable, **three or more members** | A member that reaches less than half of the stack's switch members (itself included) is in the minority part: it takes its MC-LAG legs out of the bundles (LACP out-of-sync), so the partners use the majority part. Otherwise, including an exact half, it keeps forwarding as above. |
  | The peer returns | The whole MAC tables are exchanged at once. Legs that stayed up stay up; legs that were held for the minority rule rejoin once the tables are exchanged. |
  | A member boots | `delay-restore` applies, then its legs rejoin. |
* **LACP identity** of an MC-LAG bundle: both members announce the stack's LACP system id (5.3.2
  `aggregated-ether-options lacp`) and the same key (`N+1` for `aeN`); port numbers are unique in the stack, so the
  partner sees one system with one bundle.
* **Leg state**: each member tells its peer over the stacking plane, on every change and every second, which of its
  MC-LAG legs are up (have ports that LACP has collecting and distributing) and how many of its ports LACP has ready
  (for `minimum-links`, which counts the ports of both members). Split horizon for a bundle on a member
  applies while the **peer's** leg of that bundle is up.
* A leg that is *held* stays out of the bundle: LACP tells the partner "not in sync" on its ports, so the partner moves
  its traffic to the other member without loss. Holds: the minority rule above, a lasting configuration difference
  (below), and `delay-restore` after the member booted. A restart of switchd alone (the legs still up) does not hold
  anything.
* `show mclag`: every pair of the stack (one table, 5.2 "The stack is one switch"; `member <id>` narrows it to the
  pairs of that member): per pair the primary and the secondary, whether they reach each other over the stack, and
  per MC-LAG bundle each member's leg state, split horizon and a hold with its reason and remaining time.
* **Consistency checks** at runtime: VLAN membership (and native VLAN), MTU, LACP mode and rate of each MC-LAG
  bundle as each member applies them are compared between the members (sent with the leg states). Both members
  apply the same configuration, so a difference while a commit propagates is ignored; one that lasts 10 seconds
  (for example because a member runs another software version) holds the bundle's leg on the secondary, with the
  reason in `show mclag`. The primary's leg stays up. `show mclag consistency` shows what each member applies per
  bundle and since when they differ.

### 5.7 switch-options

#### `switch-options mac-table-aging-time <10-1000000>`
Seconds after which an unused dynamically learned MAC is removed. Default 300.
On MC-LAG bundles, MACs age out only when both members age them out (5.6).

#### `switch-options vxlan { source-address <ipv4>; udp-port <n>; remote-vtep <ip> { vni [ <vni> … ]; } }`
Extends VLANs (`vlans <v> vxlan vni <vni>`) over VXLAN to **VTEPs outside the stack** (servers, hypervisors, other
vendors' switches). Between the members of the stack nothing is configured: the stack tunnels (5.2) carry every VLAN
already.

**The stack is one VTEP.** It has one source address, `source-address`, on every member, as the stack is one switch
(like a Junos Virtual Chassis). Remote VTEPs see one VTEP whichever member a frame comes from or goes to.
* `source-address <ipv4>`: the stack's VTEP address. switchd puts it on an internal loopback device (`swvtep`) of every
  switch member, in the default routing instance; the network must route it to the stack (e.g. a static route to the
  routed interface of one or more members; with several, the network's ECMP picks one). E: missing while a VLAN has a
  `vni`. E: an IPv6 address (IPv6 underlays come later). E: also the address of an interface.
* `udp-port <n>`: default 4789.
* `remote-vtep <ip> { vni [ <vni> … ]; }`: a remote VTEP and the VNIs it takes part in. Broadcast, unknown unicast and
  multicast (BUM) of a VNI is sent to every remote VTEP that lists it (head-end replication). E: a listed VNI that is
  not mapped to a VLAN. W: a remote VTEP without VNIs. E: the stack's own `source-address`.
* The remote VTEPs are reached through the routed interfaces of the default instance (irb, routed ports) and its
  static routes, on each member by its own routing table. A member without a route to a remote VTEP cannot send to it
  (`show vxlan remote-vtep` shows this per member).

**Behaviour:**
* Every member has one VXLAN port per VNI (`swvx<vni>`) in the bridge, untagged in the VNI's VLAN, sending from
  `source-address`.
* **Sending**: each member sends the traffic of its own ports to the remote VTEPs itself. Frames that reached it over a
  stack tunnel are never sent into VXLAN (the member where they entered the stack sent them), so a remote VTEP gets
  every frame once.
* **Receiving**: a frame from a remote VTEP arrives at whichever member the network delivers it to. That member
  bridges it into the VLAN: to its own ports and, over the stack tunnels, to the other members. Frames from a VXLAN
  port are never sent into another VXLAN port (split horizon), so remote VTEPs never forward for each other.
* **MAC addresses**: remote MACs are learned from received VXLAN traffic (flood and learn, the way remote VTEPs without
  a control plane learn ours). The member that learns one tells every member over the stacking protocol, and each
  installs it on its own VXLAN port with the VTEP it belongs to, so its hosts reach the remote host directly. The
  address ages out when the learning member no longer sees it. `show ethernet-switching table` shows them as
  `vtep <ip>`.
* **RSTP**: VXLAN ports do not run RSTP and never receive or send BPDUs (the mesh to the remote VTEPs is loop-free by
  split horizon).
* **Multicast**: IGMP/MLD snooping (5.5) treats the VXLAN ports as multicast-router ports: group traffic of the VLAN
  is always sent to the remote VTEPs.
* **MTU**: a VXLAN port carries frames up to the VLAN's `mtu` (default 1514). The routed path to the remote VTEPs needs
  50 bytes more (IPv4 + UDP + VXLAN + the inner Ethernet header): W when a routed interface of the default instance
  (the one towards the remote VTEPs is not known at commit time: every routed interface is checked) has a smaller
  `mtu`. Frames that do not fit are dropped where they enter the VXLAN port and counted.
* **Protection**: UDP to `source-address` and `udp-port` is accepted from the configured remote VTEPs only (1.5).
* **MC-LAG** needs nothing extra: a device behind an MC-LAG bundle is behind the one VTEP of the stack.
* `show vxlan`: per VNI the VLAN, the VXLAN port, the remote VTEPs and the remote MACs learned.
* `show vxlan remote-vtep`: per remote VTEP and member the route used to reach it (next hop and interface) or
  `no route`, and the frames sent and received.
* **The stack tunnels' port**: the stack tunnels (5.2) use UDP 4789 inside the hidden stack instance. A port cannot be
  used both there and in the default instance, so while VXLAN to remote VTEPs uses 4789 (`udp-port`, the default),
  the stack tunnels use 4790. Switching VXLAN on or off re-creates the stack tunnels with the other port: traffic
  between members pauses for a moment (well below a second) during that commit. E: a member whose version does not
  know VXLAN (it would keep the old port; update it first).
* Statements of earlier versions that no longer exist (`virtual-chassis member <id> vtep-address|underlay`,
  `switch-options vxlan mode|encryption`, `mclag … anycast-vtep`) are removed when a stored configuration is read,
  and logged. Encryption of the VXLAN underlay returns with Phase 10.

### 5.8 routing-options

Routing options of the default routing instance. Every routing instance (5.9) has the same statements under
`routing-instances <name> routing-options`.

#### `routing-options static route <prefix> { next-hop [ <ip> … ]; discard; preference <n>; }`
Static routes.
* `next-hop`: one or more gateway addresses. Several next hops share the traffic (ECMP). A next hop must be inside
  a subnet of an L3 interface of the instance, else the route is inactive (W at commit; it becomes active
  once such an interface exists and is up).
* `discard`: drop matching traffic silently (blackhole). E: `discard` together with `next-hop`. E: neither.
* `preference <0-255>`: overrides the default preference of static routes (5), e.g. `preference 200` for a floating
  static route that only takes over when the same prefix is not learned by OSPF or BGP.
* IPv4 and IPv6 prefixes can be mixed; a next hop must have the family of its prefix (E).

#### `routing-options router-id <ipv4>`
The router id of OSPF, OSPFv3 and BGP in this instance. Default: the lowest IPv4 address of a `member`-less irb
unit or routed interface of the instance that is up, chosen once at start and kept while that address exists (a
router id never changes by itself while sessions are up). W: a protocol is configured and no router id can be chosen
(no IPv4 address in the instance); the protocol does not start until one exists. Changing it restarts the protocols
of the instance (`commit check` says so).

#### `routing-options autonomous-system <asn>`
The AS number of BGP in this instance (1–4294967295; 4-byte AS numbers in plain notation). E: `protocols bgp` without
an autonomous system (here or `local-as` in every group).

#### Routes, preferences and the routing table (RIB)
Every routing instance has one routing table per family (`inet.0` and `inet6.0`; in an instance `<name>.inet.0`
and `<name>.inet6.0`). It holds every route a source offers, also the ones that are not used. For each prefix the
route with the **lowest preference** is active and installed into the kernel; among routes of equal preference the
protocol's own rule decides (OSPF: path type and cost; BGP: best-path selection). Active routes with several
next hops of equal cost are installed as ECMP routes.

| Source | Preference | `show route` name |
|---|---|---|
| Interface subnets | 0 | `Direct` |
| The switch's own addresses | 0 | `Local` |
| Static routes | 5 (or `preference`) | `Static` |
| OSPF and OSPFv3 internal (intra-area and inter-area) | 10 | `OSPF`, `OSPF3` |
| OSPF and OSPFv3 external | 150 | `OSPF`, `OSPF3` |
| BGP (external and internal) | 170 | `BGP` |
| A DHCP lease's default route (`family inet dhcp`) | 200 | `DHCP` |

* cer-ribd (1.9) installs the routes, with the switch's own protocol ids (static and DHCP: `switchd`, OSPF: `ospf`,
  BGP: `bgp`), and only ever changes or removes routes with these ids. After a restart it removes nothing of a
  routing protocol until that protocol has reported all its routes again (or its graceful restart time, 180 s,
  has passed). Only routes that changed are replaced; a commit that does not change a
  route never touches it.
* A route is never removed and added again to change it: next hops are replaced in place, so forwarding never has
  a gap.

#### Routing in a virtual chassis
As in a Junos Virtual Chassis, the routing protocols run **on the master** (the routing engine):
* The master runs OSPF, OSPFv3 and BGP for every instance and computes the routing tables. The result is replicated
  over the stacking protocol to every member, and each member installs it in its own kernel, so every member forwards
  by itself (the master is not in the forwarding path).
* Protocol packets on an interface that lives on another member (a routed port or subinterface of member 2) are
  passed between that member and the master over the stacking protocol: OSPF and BFD packets are received on the
  member and handed to the master, and sent by the member on the master's behalf; a BGP session to an address of
  that interface is relayed as a TCP stream.
* **irb interfaces and MC-LAG bundles.** An irb exists on every member with the same address and MAC, and a
  neighbour behind an MC-LAG (or any port of another member) reaches whichever member its frame arrives on.
  Multicast protocol packets (OSPF hellos, flooded updates) reach the master anyway (the VLAN floods them through the
  stack). Unicast protocol packets to the irb's address (OSPF to the neighbour's address, BFD, BGP) are consumed by the
  member they arrive on, so every member that is not the master passes them to the master **as frames**: they enter
  the stack tunnel to the master in their VLAN and the master's irb receives them as if they had arrived there (the
  TCP session of BGP ends on the master, so TCP MD5 and TTL checks work unchanged). The same holds for a routed MC-LAG
  bundle (`ae1.0` with legs on two members). Every routing protocol works over MC-LAG bundles: one neighbour, one
  adjacency or session, whatever leg its packets take, and a leg failing is no event for the protocol.
  * What is passed: OSPF (IP protocol 89), BFD (UDP 3784 and 4784) and BGP (TCP to or from port 179), IPv4 and IPv6,
    in frames to the irb MAC on a VLAN that has an irb and that the port carries (a frame with another VLAN's tag is
    not passed, so no client reaches another VLAN's irb). Everything else to the irb (routed traffic, ARP, ping) is
    handled by the member it arrives on.
  * The frame is not changed: source MAC (the neighbour's), destination MAC, IP packet and VLAN stay as they were;
    an untagged frame of an access port or native VLAN carries its VLAN's tag in the tunnel, which the master's bridge
    removes again. Mirroring still sees these frames on their port; storm control never limits them.
  * A new master is used at once by every member (within a second of the election).
* **BFD** sessions run on the member that owns the interface, so failure detection does not depend on the stacking
  links; state changes go to the master. A session over an irb or an MC-LAG bundle (no single owner) runs on the
  master like the protocol it serves; its packets reach the master as above (milliseconds over the stacking links,
  well within the detection time).
* **ECMP**: equal-cost paths (OSPF, static routes with several next hops, BGP with `multipath`) are all installed, up
  to 16 next hops per route; flows are spread by a hash over addresses and ports (layer 3+4, the kernel's
  `fib_multipath_hash_policy` 1 for IPv4 and IPv6), so one flow always takes the same path. Unlike Junos, no
  `load-balance per-packet` export policy is needed.
* **Mastership change**: the members keep the installed protocol routes, marked stale, while the new master brings
  the sessions up again (at most the `graceful-restart` restart time, default 120 s). With graceful restart (on by
  default for OSPF, OSPFv3 and BGP) the neighbours keep forwarding to the switch meanwhile. Routes that are not
  learned again by then are removed.
* `show route`, `show ospf …`, `show bgp …` and `show bfd session` run on the master and show the whole stack.

### 5.9 routing-instances

#### `routing-instances <name> { description <text>; instance-type virtual-router; interface <unit-name>; routing-options { … }; protocols { ospf|ospf3|bgp { … } } }`
A separate routing table (a Linux VRF), as in Junos. Interfaces in an instance route only among themselves; nothing is
routed between instances or to and from the default instance.
* `interface <unit-name>`: a routed unit (`irb.<n>`, `1/0/5.0`, `1/0/6.100`, `ae1.0`) that belongs to this instance.
  E: the unit has no `family inet|inet6`. E: the unit is in two instances. Units in no instance are in the default one.
* `routing-options …`: as in 5.8 (static routes, router id, autonomous system), for this instance.
* `protocols ospf|ospf3|bgp`: the routing protocols of this instance (5.13, 5.14), independent of those of the
  default instance and of other instances (own router id, own neighbours, own routing table). E: routing protocols in
  the management instance.
* Addresses and subnets may overlap between instances, but not within one (E).
* `instance-type virtual-router` (default and the only type for now; `vrf` with route distinguishers comes with BGP).
* **The management instance** is the one named by `system management-instance <instance>` (5.1, 1.8); any name is
  allowed. Its interfaces are `cme.0` and optionally in-band irb units, and it exists only on the master.
  Typical configurations:

  ```
  # management ports (see 5.3.4)
  set interfaces 1/2/0 management
  set interfaces 2/2/0 management
  set interfaces cme unit 0 family inet address 10.5.20.76/16
  set routing-instances oob interface cme.0
  set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 10.5.0.1
  set system management-instance oob

  # or in-band, on a management VLAN
  set vlans mgmt vlan-id 99
  set vlans mgmt l3-interface irb.99
  set interfaces irb unit 99 family inet address 10.5.176.95/16
  set routing-instances oob interface irb.99
  set system management-instance oob
  ```
* Older configurations (`mgmt_ceros`, `system management-instance` without a name, per-member management
  addresses) are **not** converted: they fail the commit check and must be rewritten in this form.
* In the kernel an instance is a VRF device named like the instance, with its own routing table.
* `show route instance <name>` lists the routes (5.14, `show route`); `show interfaces` marks management interfaces.

### 5.10 forwarding-options

#### `forwarding-options analyzer <name> { input { … }; output { interface <if>; } }`
Port mirroring. Copies of the selected traffic are sent out of the output interface. Mirroring never affects
the original traffic. If the output port is congested, only mirrored copies are dropped.
* `input ingress interface [ <if> … ]`: frames **received** on these interfaces, as received (including their VLAN tag).
* `input egress interface [ <if> … ]`: frames **sent** on these interfaces, as sent.
* `input ingress vlan [ <vlan> … ]`: frames received in these VLANs, on any port of the output port's member.
* `output interface <if>`: destination. A dedicated plain port (without `family ethernet-switching`) is recommended.
  Mirrored frames are sent unmodified. An `ae` output spreads copies by its hash policy.
* Mirroring happens on the output port's member. Mirroring an `ae` mirrors its member ports on that member. For an
  MC-LAG bundle, that is only the local leg; traffic on the other member's leg is not seen.
* Several analyzers may share an output port, and one interface may be an input of several analyzers.
* Implemented with tc (`matchall`/`flower` + `mirred`) on the clsact hooks, before storm control (copies are taken as
  received); offloaded to hardware where supported. Per VLAN: tagged frames by VLAN id, untagged ones (access or
  native VLAN) by "no VLAN tag".
* Commit check:
  * E: no output.
  * E: no input.
  * E: the output is also an input.
  * E: an input has no ports on the output's member (mirroring across the stack is not supported).
  * E: the output is a bundle spanning two members.
  * E: an input or output is not configured under `interfaces`, or is a bundle member (use the `ae`).
  * W: the output port is also a switch port.


### 5.11 policy-options

Routing policies select routes and change their attributes. They are used by `import` and `export` of BGP and by
`export` of OSPF/OSPFv3 (redistribution). The syntax and evaluation follow Junos.

#### `policy-options prefix-list <name> prefix [ <prefix> … ]`
A list of prefixes (IPv4 and IPv6 may be mixed). Used in `from prefix-list` (exact matches) and
`from prefix-list-filter <name> match exact|orlonger|longer`. (Junos writes the prefixes directly inside the list;
here they are a `prefix` leaf-list.)

#### `policy-options community <name> members [ <community> … ]`
A named BGP community set. A member is `<asn>:<value>` (standard), `large:<a>:<b>:<c>` (large community, RFC
8092), a well-known name (`no-export`, `no-advertise`, `no-export-subconfed`), or a regular expression on the
`<asn>:<value>` form (`^65000:1..$`). A route matches the community when it carries **all** listed members.

#### `policy-options as-path <name> path "<regex>"`
A regular expression over the AS path, Junos style: the elements are AS numbers, `.` matches one AS, and the
operators `* + ? {m,n} | ( ) ^ $ [ ]` work on whole AS numbers (`"^65000 .*"`: learned from AS 65000;
`"^$"`: originated in this AS).

#### `policy-options policy-statement <name> { term <term> { from { … } then { … } } then { … } }`
A policy is a list of terms evaluated in order. A route matches a term when it matches **every** condition in the
term's `from` (an empty `from` matches everything); then the term's `then` actions run. A **terminating action**
(`accept`, `reject`) ends the evaluation of all policies; `next term` continues with the next term (default when
a term has no terminating action), `next policy` with the next policy of the chain. A `then` directly under the
policy (without a term) applies to routes no term terminated.

`from` conditions:
* `protocol direct|local|static|ospf|ospf3|bgp|aggregate` (several: any of them)
* `route-filter <prefix> exact`, `… orlonger`, `… longer`, `… upto /<n>`, `… prefix-length-range /<a>-/<b>`
  (several route filters: any of them matches)
* `prefix-list <name>`, `prefix-list-filter <name> match exact|orlonger|longer`
* `community <name>`, `as-path <name>`
* `neighbor <ip>` (BGP: the peer the route came from or goes to)
* `area <area-id>` (OSPF: the area of the route)
* `family inet|inet6`
* `tag <n>` (OSPF external tag)

`then` actions:
* `accept`, `reject`, `next term`, `next policy`
* `metric <n>` (BGP MED, OSPF external metric), `metric-add <n>`
* `local-preference <n>`, `preference <n>` (the route's preference in this switch's RIB)
* `community add|delete|set [ <name> … ]`
* `as-path-prepend "<asn> …"`
* `next-hop self|<ip>|discard`
* `external-type 1|2` (OSPF external type), `tag <n>`

A chain of policies (`export [ a b ]`) is evaluated in order; a route that no policy accepts or rejects gets the
protocol's **default policy**:
* BGP import: accept. BGP export: accept the active BGP routes, reject everything else (so OSPF, static and direct
  routes are announced only through an explicit policy).
* OSPF export: reject (nothing is redistributed unless a policy accepts it; OSPF's own routes are flooded by OSPF
  itself and are not affected by export).

Commit check:
* E: a policy, prefix list, community or AS path that is referenced but not defined.
* E: an invalid regular expression or community.
* W: a defined policy that nothing uses.
* W: a term after a term without `from` whose `then` terminates (the later term is never reached).

### 5.12 BFD

#### `bfd-liveness-detection { minimum-interval <ms>; multiplier <n>; authentication { algorithm keyed-sha-1|keyed-md5; key <secret>; key-id <n>; } }`
Bidirectional Forwarding Detection (RFC 5880, 5881 single-hop, 5883 multihop) for a routing protocol neighbour: a
neighbour that stops answering is declared down after `minimum-interval × multiplier` (default 300 ms × 3) instead
of the protocol's hold time (OSPF 40 s, BGP 90 s by default). The statement goes under an OSPF/OSPFv3 interface
(every neighbour on it) or a BGP group or neighbour.
* Asynchronous mode; single-hop sessions use UDP 3784 with TTL 255, multihop sessions (BGP `multihop`, iBGP between
  loopback-like irb addresses) UDP 4784.
* `minimum-interval` 50–60000 ms (both directions); both sides use the slower of the two intervals.
* `authentication`: optional keyed SHA-1 or MD5 (RFC 5880 §6.7); the neighbour must use the same key.
* A BFD session going down takes the OSPF adjacency or BGP session down at once; it comes back through the protocol's
  normal start (BGP waits for BFD to be up before it connects again).
* BFD runs with real-time scheduling priority on the member that owns the interface (5.8, virtual chassis).
* Values below 100 ms can cause false detections on small ARM boards (W at commit for `minimum-interval` < 100 on a
  member with fewer than 4 CPU cores).
* `show bfd session [extensive]`: per session the neighbour, interface, state, the negotiated interval and
  multiplier, the detection time, the client (OSPF, BGP), uptime and transitions; `extensive` adds counters and the
  local and remote discriminators.

### 5.13 protocols ospf, protocols ospf3

OSPF version 2 for IPv4 (RFC 2328) and OSPFv3 for IPv6 (RFC 5340), in the default instance (`protocols ospf`) and in
routing instances (`routing-instances <name> protocols ospf`). OSPFv3 has the same statements as OSPF unless noted.

#### `protocols ospf { area <area-id> { interface <unit> { … } } … }`
* `area <area-id>`: an area, written as a number (`0`) or dotted (`0.0.0.0`); both mean the same area. Area 0 is the
  backbone. With interfaces in more than one area the switch is an **area border router**: it must have an
  interface in area 0 (E otherwise) and announces each area's networks into the others as summaries (type 3 LSAs;
  OSPFv3: inter-area prefix LSAs). Stub and NSSA areas are not supported (planned).
* `interface <unit>`: a routed unit of the instance (`irb.10`, `1/0/5.0`, `ae1.0`, `1/0/6.100`). E: the unit has
  no address of the family (IPv4 for OSPF, IPv6 for OSPFv3; OSPFv3 also uses the link-local address). E: the unit is
  in another instance, or in two areas. E: an irb address with `member` is the only address (OSPF runs once for the
  whole stack, on the master's irb). OSPF on a management interface (`cme.0`) is an error.
  * `passive`: the interface's subnets are announced, but no hellos are sent and no neighbours are formed.
  * `metric <1-65535>`: the cost. Default: `reference-bandwidth` / interface speed, at least 1 (an irb counts as
    the speed of the fastest port in its VLAN on the master; an `ae` as the sum of its active ports). An interface
    whose speed is unknown (a virtual NIC, a unit without ports) costs 1.
  * `interface-type p2p`: point-to-point (no DR election; recommended for routed links between two routers).
    Default: broadcast (DR/BDR election).
  * `priority <0-255>`: DR election priority (default 128; 0: never DR or BDR).
  * `hello-interval <s>` (default 10), `dead-interval <s>` (default 4 × hello), `retransmit-interval <s>`
    (default 5), `transit-delay <s>` (default 1). E: the dead interval is not larger than the hello interval.
    Hello and dead intervals must match the neighbours'.
  * `authentication { simple-password <key>; }` or `authentication { md5 <key-id> key <key>; }` (OSPF only; several
    `md5` keys for rollover: the newest is used to send, all are accepted). OSPF only: OSPFv3 has no
    `authentication` statement (neither RFC 4552 IPsec nor the RFC 7166 trailer is supported yet).
  * `bfd-liveness-detection { … }` (5.12).
* `export [ <policy> … ]`: redistribution of routes into OSPF as external routes (type 5 LSAs; OSPFv3: AS-external
  LSAs), e.g. static or BGP routes. Default: nothing (5.11). Exporting makes the switch an AS boundary router.
  `then external-type 1|2` (default 2), `metric` (default 0 for type 2) and `tag`.
* `reference-bandwidth <bandwidth>`: for the default metric, e.g. `100g` (default `100g`; Junos default is 100m,
  which gives every port faster than 100 Mbit/s cost 1).
* `overload`: the switch announces itself with maximum metric (RFC 6987), so traffic passes through it only when
  there is no other way (e.g. before maintenance). `overload timeout <s>` keeps it for that long after every start.
* `graceful-restart { disable; restart-duration <s>; }`: graceful restart (RFC 3623; OSPFv3 RFC 5187) as restarting
  router (mastership change, switchd restart, software update) and as helper for neighbours. Default on, 120 s.
* `disable`: OSPF is configured but not running.
* Routes: intra-area before inter-area before external type 1 before external type 2; equal cost paths become ECMP
  (up to 16 next hops).
* The protection of the switch's addresses (1.5) lets OSPF packets in on the OSPF interfaces only.
* Changing an interface's cost, passive or timers affects only that interface; adding an interface does not reset
  other adjacencies. `commit check` names the adjacencies a change resets.

Operational commands:
* `show ospf neighbor [<address>] [detail]`: neighbour id, address, interface, state, priority, dead time; `detail`
  adds the DR/BDR, the area, uptime and options.
* `show ospf interface [<unit>] [detail]`: interface, area, state (DR, BDR, DRother, PtToPt, Passive, Down),
  DR/BDR, neighbour count, cost, timers.
* `show ospf database [router|network|summary|asbr-summary|external] [lsa-id <id>] [advertising-router <id>]
  [detail|extensive]`: the link-state database per area.
* `show ospf route`: the routes OSPF computed (with path type, cost and next hops), also those not active in the RIB.
* `show ospf overview`: router id, ABR/ASBR role, areas, SPF runs and the last SPF time, graceful-restart state.
* `show ospf statistics`: packets sent and received by type, errors.
* `clear ospf neighbor [<address>]`: restarts adjacencies.
* The same under `show ospf3 …` and `clear ospf3 neighbor`. All with `instance <name>`.

### 5.14 protocols bgp

BGP-4 (RFC 4271) with 4-byte AS numbers, IPv4 and IPv6 unicast, route refresh, graceful restart and route
reflection, in the default instance and in routing instances. switchd embeds the GoBGP implementation.

#### `protocols bgp { group <name> { type internal|external; peer-as <asn>; neighbor <ip> { … } … } }`
Neighbours are configured in groups; a neighbour inherits everything from its group and can override it.
* `type internal|external`: iBGP (the neighbour is in the switch's own AS) or eBGP. E: `type internal` with a
  `peer-as` other than the own AS; E: `type external` with `peer-as` equal to it. E: a group without `type`.
* `peer-as <asn>`: the neighbour's AS (group or neighbour; E: an external neighbour without one).
* `neighbor <ip>`: a neighbour address (IPv4 or IPv6). E: the same neighbour in two groups.
* `local-address <ip>`: the source address of the session (an address of the instance). Default: the address of the
  interface the neighbour is reached through. Needed for iBGP between irb addresses with `member` or loopback-like
  addresses.
* `local-as <asn>`: a different local AS for this group/neighbour (AS migration).
* `description <text>`.
* `authentication-key <secret>`: TCP MD5 signatures (RFC 2385). Changing it resets the session.
* `hold-time <0|3-65535>` (default 90; 0 disables keepalives), `passive` (never connect, only accept),
  `multihop { ttl <n>; }` (eBGP to a neighbour that is not directly connected; default TTL 1 for eBGP, 255 for iBGP).
* `family inet unicast`, `family inet6 unicast`: the address families (default: the family of the neighbour address).
  An IPv6 neighbour can carry IPv4 routes and the other way round (next hops are then IPv4-mapped).
* `import [ <policy> … ]`, `export [ <policy> … ]` (5.11): group or neighbour (the neighbour's replaces the group's).
* `multipath`: up to 16 equal BGP paths are installed as ECMP (same AS path length, origin, MED, local preference).
  `multipath multiple-as` also across different neighbouring ASes.
* `cluster <ipv4>` (internal groups): this switch is a route reflector for the group's neighbours (RFC 4456), which are
  its clients.
* `remove-private` (external): private AS numbers are removed from the AS path towards the neighbour.
* `bfd-liveness-detection { … }` (5.12).
* `graceful-restart { disable; restart-time <s>; stale-routes-time <s>; }`: RFC 4724, default on (restart time 120 s,
  stale routes kept 300 s).
* `disable` (group or neighbour): configured, but no session.
* Next hops: eBGP sets the next hop to the switch's address; iBGP keeps it unless the export policy says
  `next-hop self`.

**Hitless reconfiguration**: adding or removing a neighbour never affects the others. A changed policy is applied
with route refresh (soft reconfiguration) without resetting the session. Changes that need a new session
(`peer-as`, `local-address`, `local-as`, `authentication-key`, `type`, `family`, `multihop`, `hold-time`, `passive`)
reset only that neighbour, and `commit check` names it (`warning: bgp neighbor 10.1.1.2: this change resets the
session`).

Operational commands:
* `show bgp summary`: the router id and AS, per neighbour its AS, state (or prefixes received/accepted/active per
  family when established), up/down time, messages and flaps.
* `show bgp neighbor [<ip>]`: everything about a neighbour: state, timers, capabilities negotiated, families,
  policies, last error, counters.
* `show bgp group [<name>]`: the groups and their neighbours.
* `show route receive-protocol bgp <ip> [extensive]`, `show route advertising-protocol bgp <ip> [extensive]`
  (below).
* `clear bgp neighbor [<ip>] [soft|soft-inbound]`: reset (or with `soft`: send route refresh / re-evaluate).
* All with `instance <name>`.

#### `show route` (full)
`show route` shows the routing tables (RIB) the way Junos does. It replaces the earlier short listing.

```
inet.0: 7 destinations, 9 routes (7 active, 0 holddown, 1 hidden)
+ = Active Route, - = Last Active, * = Both

0.0.0.0/0          *[OSPF/150] 01:02:03, metric 0, tag 0
                    >  to 10.1.1.2 via 1/0/1.0
                    [BGP/170] 00:10:00, localpref 100
                      AS path: 65001 I, validation-state: unverified
                    >  to 10.2.2.2 via irb.20
10.1.1.0/30        *[Direct/0] 1d 02:03:04
                    >  via 1/0/1.0
10.1.1.1/32        *[Local/0] 1d 02:03:04
                       Local via 1/0/1.0
```

* Tables: `inet.0`, `inet6.0`, and `<instance>.inet.0`/`<instance>.inet6.0` for routing instances; by default all
  tables of the default instance, `instance <name>` / `table <name>` select others, `instance all` every table.
* Per destination every route with its protocol and preference (`[OSPF/10]`), `*` for the active route, its age,
  metric/tag/local preference, and its next hops (`>` marks the next hop in use; ECMP shows several `>`).
* Filters (combinable): `show route <prefix>` (longest match for an address, the prefix and everything inside it
  for a prefix; `exact` only that prefix; `longer` only more specific ones), `protocol <direct|local|static|ospf|
  ospf3|bgp>`, `next-hop <ip>`, `active-path`, `hidden` (routes rejected by import policy or with an unresolvable
  next hop).
* `terse`: one line per route (destination, protocol, preference, metrics, next hop, AS path).
* `detail` / `extensive`: everything the RIB knows: for OSPF the area, path type (intra, inter, ext1, ext2), cost and
  advertising router; for BGP the neighbour, AS path, origin, local preference, MED, communities, originator and
  cluster list, the reason a route is not active (`Inactive reason: Route Preference`, `AS path`, `Not Best in its
  group - Router ID`), and the policy result.
* `summary`: per table the number of destinations and routes per protocol, active and hidden.
* `receive-protocol bgp <neighbor>`: the routes received from the neighbour before import policy (with `hidden`:
  the ones import rejected). `advertising-protocol bgp <neighbor>`: the routes sent to it after export policy.
* Routes installed in the kernel are checked against the RIB: a difference is marked (`# not in the kernel` / an
  extra line `Kernel: <route>`) so a broken install is visible.
* Member targets (`member <id>`, `all-members`) show a member's kernel routes (each member installs the master's
  routing table, 5.8).

---

## 6. Frame handling summary

Per-port behaviour for frames of VLAN *V*, where *N* is the native VLAN:

| Received frame | access port (VLAN A) | trunk with native N | trunk without native |
|---|---|---|---|
| untagged / priority-tagged | → VLAN A | → VLAN N | dropped |
| tagged V, V ∈ members | dropped | → VLAN V | → VLAN V |
| tagged V, V ∉ members | dropped | dropped | dropped |
| larger than port MTU | dropped (mtu-exceeded) | dropped | dropped |
| larger than VLAN MTU | dropped (vlan mtu) | dropped | dropped |

| Sent frame of VLAN V | access port (VLAN A) | trunk |
|---|---|---|
| V = A | untagged | – |
| V = N | – | untagged |
| V ∈ members, V ≠ N | – | tagged |

Additional rules, applied in this order:
1. BPDU on a `bpdu-block` port → the port is shut down.
2. LACPDUs are consumed by the switch's own LACP. They are never forwarded.
3. BPDUs → processed by RSTP if enabled, otherwise flooded transparently.
4. Storm control on the ingress port.
5. VLAN classification (tables above), then the VLAN MTU filter.
6. MAC learning (subject to `mac-limit`), then forwarding by MAC table. Unknown destinations, broadcast and
   multicast are flooded to all ports in the VLAN, including VXLAN tunnels. Split horizon applies to stack
   tunnels (5.2, 5.6) and VXLAN tunnels.
7. Mirroring copies are taken at ingress (step 1, before any filter) and at egress (as sent).

---

## 7. Complete examples

### 7.1 Standalone switch with access and trunk ports

```
system {
    host-name lab-sw;
    management-instance oob;
    login {
        user admin {
            class super-user;
            authentication {
                ssh-key "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample admin@laptop";
            }
        }
    }
    syslog {
        host 192.168.1.5 {
            transport tcp;
            severity notice;
        }
    }
}
interfaces {
    1/3/0 {
        description "management port";
        management;
    }
    cme {
        unit 0 {
            family {
                inet {
                    address 192.168.1.20/24;
                }
            }
        }
    }
    1/1/0 {
        description "server A";
        unit 0 {
            family {
                ethernet-switching {
                    vlan {
                        members users;
                    }
                }
            }
        }
    }
    1/1/1 {
        description "uplink";
        mtu 9216;
        native-vlan-id users;
        unit 0 {
            family {
                ethernet-switching {
                    interface-mode trunk;
                    vlan {
                        members [ users storage ];
                    }
                }
            }
        }
    }
}
vlans {
    storage {
        vlan-id 20;
        mtu 9014;
    }
    users {
        vlan-id 10;
    }
}
routing-instances {
    oob {
        interface cme.0;
        routing-options {
            static {
                route 0.0.0.0/0 {
                    next-hop 192.168.1.1;
                }
            }
        }
    }
}
protocols {
    rstp {
        interface 1/1/0 {
            edge;
        }
    }
    layer2-control {
        bpdu-block {
            interface 1/1/0;
        }
    }
}
```

### 7.2 MC-LAG pair: dual-homed server, in-band management VLAN, port ranges, mirroring, VXLAN (set format)

```
# Stacking ports were designated locally beforehand, e.g. on both switches:
#   request virtual-chassis vc-port set 1/2/1
set system management-instance oob
set virtual-chassis member 1 host-name sw-a
set virtual-chassis member 2 host-name sw-b
# routed uplinks towards the remote VTEPs (the network routes 10.255.0.1 to them)
set interfaces 1/5/0 mtu 9216
set interfaces 1/5/0 unit 0 family inet address 10.99.0.1/31
set interfaces 2/5/0 mtu 9216
set interfaces 2/5/0 unit 0 family inet address 10.99.0.3/31
set routing-options static route 10.200.0.0/16 next-hop [ 10.99.0.0 10.99.0.2 ]
set switch-options vxlan source-address 10.255.0.1
set switch-options vxlan remote-vtep 10.200.1.10 vni 10010
set interfaces 1/1/0 ether-options 802.3ad ae1
set interfaces 2/1/0 ether-options 802.3ad ae1
set interfaces ae1 description "server A (dual-homed)"
set interfaces ae1 mtu 9216
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 native-vlan-id users
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members [ storage mgmt ]
set interface-range edge-ports member "*/4/*"
set interface-range edge-ports mtu 9014
set interface-range edge-ports unit 0 family ethernet-switching vlan members users
set interfaces 1/3/0 description "mirror to analyzer laptop"
set interfaces 1/3/1 description "management access (1G)"
set interfaces 1/3/1 unit 0 family ethernet-switching vlan members mgmt
set vlans mgmt vlan-id 99
set vlans mgmt l3-interface irb.99
set interfaces irb unit 99 family inet address 192.168.1.11/24
set routing-instances oob interface irb.99
set routing-instances oob routing-options static route 0.0.0.0/0 next-hop 192.168.1.1
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9014
set protocols rstp
set forwarding-options analyzer debug input ingress interface ae1
set forwarding-options analyzer debug input egress interface ae1
set forwarding-options analyzer debug output interface 1/3/0
```

---

## 8. Implementation status

| Area | Status |
|---|---|
| Schema, formats (hierarchical, set, JSON), diff, directives, `load`, `copy`, `rename` | implemented, tested and fuzzed |
| Commit check, commit / confirmation / rollback, stack-wide replication | implemented and tested, lab tested |
| CLI (modes, pipes, completion, member targets, sessions surviving switchd restarts) | implemented, unit and lab tested |
| Hitless apply, switch ports, VLANs, bundles (static and LACP), MTU, storm control, mac-limit, flow control, `interface-range` | implemented, unit and lab tested |
| `vlans <v> mtu` (nftables bridge filter) | implemented; lab tested on access ports |
| L3: irb, routed ports and subinterfaces, routing instances, static routes, `family inet dhcp`, protection of data addresses | implemented, unit and lab tested |
| Chassis management (1.8): management instance, `cme`, services on the master, CLI forwarding | implemented, lab tested |
| `system login`, `system services ssh`, `system ports`, host names, resolver, NTP, syslog | implemented and lab tested; kernel messages not yet forwarded to syslog; `system login message` not yet on serial consoles |
| Stacking (transport, TLS, relay, BFD, stack tunnels, join/remove, force-master, maintenance mode, software updates) | implemented, lab tested |
| LACP, MC-LAG (5.6), RSTP (one bridge for the stack), LLDP, port mirroring | implemented, lab tested |
| `system services web-management` | not implemented (W at commit) |
| VXLAN to remote VTEPs (5.7) | implemented |
| IGMP/MLD snooping (5.5) | implemented |
| `show system bottlenecks` (3.5.2) | implemented |
| Routing: RIB and preferences, `policy-options`, BFD, OSPF/OSPFv3, BGP, full `show route` (5.8, 5.11–5.14) | specified, being implemented |
| Firmware image (docs/os-image.md), signed bundles, A/B slots, update daemon with automatic rollback (3.6), `system root-authentication` | implemented; unit tested and tested in QEMU; not yet on the lab switches |
| `request system zeroize`, `request system storage cleanup` | not implemented yet |
| OS takeover (1.4: masking the operating system's network services, ending DHCP clients), own systemd unit | implemented |

---

## 9. Statement index (generated)

All statements with their types, ranges and defaults, generated from the schema.
"excl." marks mutually exclusive statements.

<!-- BEGIN GENERATED STATEMENT INDEX (go test ./docs -update) -->
| Statement | Kind | Value | Default | Description |
|---|---|---|---|---|
| `system` | container |  |  | System parameters |
| `system host-name` | leaf | &lt;hostname&gt; |  | Name of the stack/system |
| `system management-instance` | leaf | &lt;instance-name&gt; |  | Routing instance for management (cme, services on the master) |
| `system domain-name` | leaf | &lt;hostname&gt; |  | DNS domain name |
| `system time-zone` | leaf | &lt;time-zone&gt; |  | Time zone (e.g. Europe/Berlin) |
| `system name-server` | leaf-list | &lt;ip-address&gt; |  | DNS servers |
| `system ntp` | container |  |  | Network time protocol |
| `system ntp server <host>` | list | &lt;host&gt; |  | NTP server |
| `system ntp server <host> prefer` | flag |  |  | Prefer this server |
| `system syslog` | container |  |  | System logging |
| `system syslog host <host>` | list | &lt;host&gt; |  | Remote syslog server |
| `system syslog host <host> port` | leaf | &lt;port&gt; 1..65535 | 514 (udp/tcp), 6514 (tls) | Destination port |
| `system syslog host <host> transport` | leaf | udp \\| tcp \\| tls | udp | Transport protocol |
| `system syslog host <host> facility` | leaf | any \\| kernel \\| daemon \\| authorization \\| change-log \\| interactive-commands \\| local0 \\| local1 \\| local2 \\| local3 \\| local4 \\| local5 \\| local6 \\| local7 | any | Facility filter |
| `system syslog host <host> severity` | leaf | emergency \\| alert \\| critical \\| error \\| warning \\| notice \\| info \\| debug \\| any | info | Minimum severity to send |
| `system syslog host <host> ca-certificate` | leaf | &lt;path&gt; |  | PEM file used to verify the TLS server |
| `system syslog local-buffer-size` | leaf | &lt;lines&gt; 100..100000 | 5000 | Lines kept for 'show log' |
| `system root-authentication` | container |  |  | Password and SSH keys of root |
| `system root-authentication encrypted-password` | leaf | &lt;hash&gt; |  | Crypt(3) password hash ($6$/$y$) |
| `system root-authentication ssh-key` | leaf-list | &lt;public-key&gt; |  | SSH public key ("ssh-ed25519 AAAA... comment") |
| `system login` | container |  |  | Local user accounts |
| `system login message` | leaf | &lt;text&gt; |  | Login banner |
| `system login user <username>` | list | &lt;username&gt; |  | User account |
| `system login user <username> uid` | leaf | &lt;uid&gt; 1000..64000 |  | Numeric user id |
| `system login user <username> class` | leaf | super-user \\| operator \\| read-only | read-only | Permission class |
| `system login user <username> full-name` | leaf | &lt;text&gt; |  | Full name |
| `system login user <username> authentication` | container |  |  | Authentication methods |
| `system login user <username> authentication encrypted-password` | leaf | &lt;hash&gt; |  | Crypt(3) password hash ($6$/$y$) |
| `system login user <username> authentication ssh-key` | leaf-list | &lt;public-key&gt; |  | SSH public key ("ssh-ed25519 AAAA... comment") |
| `system services` | container |  |  | System services |
| `system services ssh` | presence |  |  | SSH access to the CLI (present: switchd manages the SSH server configuration) |
| `system services ssh port` | leaf | &lt;port&gt; 1..65535 | 22 | Listening port |
| `system services ssh root-login` | leaf | deny \\| allow \\| key-only | deny | Root login policy |
| `system services web-management` | container |  |  | Web interface and REST API |
| `system services web-management port` | leaf | &lt;port&gt; 1..65535 | 443 | HTTPS port |
| `system services web-management certificate` | leaf | &lt;path&gt; |  | PEM certificate file (self-signed if unset) |
| `system services web-management key` | leaf | &lt;path&gt; |  | PEM private key file |
| `system services web-management disable` | flag |  |  | Disable the web interface |
| `system commit` | container |  |  | Commit behaviour |
| `system commit confirmation` | container |  |  | Automatic rollback of unconfirmed commits |
| `system commit confirmation mode` | leaf | required \\| optional | required | Whether every commit must be confirmed |
| `system commit confirmation timeout` | leaf | &lt;minutes&gt; 1..60 | 10 | Minutes until an unconfirmed commit is rolled back |
| `system ports` | container |  |  | Console ports |
| `system ports no-auto-detect` | flag |  |  | Do not start a CLI login on detected serial ports |
| `system ports login-required` | flag |  |  | Ask for user name and password on the local consoles (default: root without password) |
| `system ports console <tty>` | list | &lt;tty&gt; |  | Serial console port |
| `system ports console <tty> speed` | leaf | 9600 \\| 19200 \\| 38400 \\| 57600 \\| 115200 | 115200 | Baud rate |
| `system ports console <tty> disable` | flag |  |  | Do not start a login on this port |
| `system offload` | container |  |  | Hardware acceleration |
| `system offload mode` | leaf | auto \\| disable | auto | Offload policy |
| `system offload watchdog` | container |  |  | Runtime offload health monitoring |
| `system offload watchdog interval` | leaf | &lt;seconds&gt; 1..300 | 5 | Seconds between counter checks |
| `system offload watchdog threshold` | leaf | &lt;count&gt; 1..1000000 | 100 | Drops/errors per interval that trigger a software fallback |
| `system offload watchdog alarm-only` | flag |  |  | Only raise alarms, never change offload settings |
| `virtual-chassis` | container |  |  | Stack members and stacking (like a Junos Virtual Chassis) |
| `virtual-chassis bfd` | container |  |  | BFD on stacking ports (IP-less) |
| `virtual-chassis bfd minimum-interval` | leaf | &lt;ms&gt; 50..10000 | 100 | Transmit/receive interval in milliseconds |
| `virtual-chassis bfd multiplier` | leaf | &lt;count&gt; 2..255 | 3 | Missed packets before the session goes down |
| `virtual-chassis member <member-id>` | list | &lt;member-id&gt; 1..16 |  | Stack member |
| `virtual-chassis member <member-id> host-name` | leaf | &lt;hostname&gt; |  | Host name of this member |
| `virtual-chassis member <member-id> mastership-priority` | leaf | &lt;priority&gt; 0..255 | 128 | Priority for leader election (higher wins) |
| `virtual-chassis member <member-id> role` | leaf | switch \\| witness | switch | Member role |
| `interface-range <name>` | list | &lt;name&gt; |  | Apply one configuration to many ports |
| `interface-range <name> member` | leaf-list | &lt;pattern&gt; |  | Ports by pattern, e.g. 1/0/* or */1/[0-3] |
| `interface-range <name> member-range <interface-name>` | list | &lt;interface-name&gt; |  | Contiguous ports on one card, e.g. 1/0/0 to 1/0/23 |
| `interface-range <name> member-range <interface-name> to` | leaf | &lt;interface-name&gt; |  | Last port of the range |
| `interface-range <name> description` | leaf | &lt;text&gt; |  | Interface description |
| `interface-range <name> disable` | flag |  |  | Administratively disable the interface |
| `interface-range <name> mtu` | leaf | &lt;mtu&gt; 256..16000 | 1514 | Maximum frame size incl. Ethernet header, excl. FCS and VLAN tags |
| `interface-range <name> ether-options` | container |  |  | Physical port options |
| `interface-range <name> ether-options 802.3ad` | leaf | &lt;ae-interface&gt; |  | Make this port a member of an aggregated interface |
| `interface-range <name> ether-options flow-control` | flag (excl. flow) |  |  | Enable pause frames (reduces drops under load) |
| `interface-range <name> ether-options no-flow-control` | flag (excl. flow) |  |  | Disable pause frames |
| `interface-range <name> aggregated-ether-options` | container |  |  | Aggregated interface options |
| `interface-range <name> aggregated-ether-options lacp` | presence |  |  | Link aggregation control protocol |
| `interface-range <name> aggregated-ether-options lacp active` | flag (excl. lacp-mode) |  |  | Actively send LACPDUs |
| `interface-range <name> aggregated-ether-options lacp passive` | flag (excl. lacp-mode) |  |  | Only respond to LACPDUs |
| `interface-range <name> aggregated-ether-options lacp periodic` | leaf | fast \\| slow | fast | LACPDU interval |
| `interface-range <name> aggregated-ether-options lacp system-priority` | leaf | &lt;priority&gt; 1..65535 | 32768 | LACP system priority |
| `interface-range <name> aggregated-ether-options minimum-links` | leaf | &lt;links&gt; 1..64 | 1 | Minimum active links for the bundle to be up |
| `interface-range <name> aggregated-ether-options hash-policy` | leaf | layer2 \\| layer2+3 \\| layer3+4 | layer3+4 | Load-balancing hash |
| `interface-range <name> storm-control` | container |  |  | Rate limit flooded traffic |
| `interface-range <name> storm-control broadcast` | leaf | &lt;pps&gt; 1..100000000 |  | Broadcast packets per second |
| `interface-range <name> storm-control multicast` | leaf | &lt;pps&gt; 1..100000000 |  | Multicast packets per second |
| `interface-range <name> mac-limit` | leaf | &lt;count&gt; 1..131072 |  | Maximum learned MAC addresses |
| `interface-range <name> offload` | container |  |  | Per-interface hardware acceleration |
| `interface-range <name> offload disable` | flag |  |  | Never offload this interface |
| `interface-range <name> native-vlan-id` | leaf | &lt;vlan&gt; |  | Untagged VLAN on a trunk port |
| `interface-range <name> vlan-tagging` | flag |  |  | Routed subinterfaces: each unit takes the frames with its vlan-id |
| `interface-range <name> unit <unit>` | list | &lt;unit&gt; 0..16385 |  | Logical unit |
| `interface-range <name> unit <unit> description` | leaf | &lt;text&gt; |  | Unit description |
| `interface-range <name> unit <unit> disable` | flag |  |  | Administratively disable the unit |
| `interface-range <name> unit <unit> vlan-id` | leaf | &lt;vlan-id&gt; 1..4094 |  | 802.1Q tag of a routed subinterface (needs vlan-tagging) |
| `interface-range <name> unit <unit> family` | container |  |  | Protocol family |
| `interface-range <name> unit <unit> family ethernet-switching` | presence |  |  | Layer 2 switching (unit 0 only) |
| `interface-range <name> unit <unit> family ethernet-switching interface-mode` | leaf | access \\| trunk |  | Port mode |
| `interface-range <name> unit <unit> family ethernet-switching vlan` | container |  |  | VLAN membership |
| `interface-range <name> unit <unit> family ethernet-switching vlan members` | leaf-list | &lt;vlan&gt; |  | VLAN names or ids (ranges like 10-20 allowed) |
| `interface-range <name> unit <unit> family inet` | presence |  |  | IPv4 (routed interface) |
| `interface-range <name> unit <unit> family inet address <address/prefix>` | list | &lt;address/prefix&gt; |  | Interface address |
| `interface-range <name> unit <unit> family inet address <address/prefix> member` | leaf | &lt;member-id&gt; 1..16 |  | Only on this member (irb units) |
| `interface-range <name> unit <unit> family inet dhcp` | flag |  |  | Obtain the IPv4 address via DHCP |
| `interface-range <name> unit <unit> family inet6` | presence |  |  | IPv6 (routed interface) |
| `interface-range <name> unit <unit> family inet6 address <address/prefix>` | list | &lt;address/prefix&gt; |  | Interface address |
| `interface-range <name> unit <unit> family inet6 address <address/prefix> member` | leaf | &lt;member-id&gt; 1..16 |  | Only on this member (irb units) |
| `interfaces <interface-name>` | list | &lt;interface-name&gt; |  | Interface configuration |
| `interfaces <interface-name> description` | leaf | &lt;text&gt; |  | Interface description |
| `interfaces <interface-name> disable` | flag |  |  | Administratively disable the interface |
| `interfaces <interface-name> mtu` | leaf | &lt;mtu&gt; 256..16000 | 1514 | Maximum frame size incl. Ethernet header, excl. FCS and VLAN tags |
| `interfaces <interface-name> ether-options` | container |  |  | Physical port options |
| `interfaces <interface-name> ether-options 802.3ad` | leaf | &lt;ae-interface&gt; |  | Make this port a member of an aggregated interface |
| `interfaces <interface-name> ether-options flow-control` | flag (excl. flow) |  |  | Enable pause frames (reduces drops under load) |
| `interfaces <interface-name> ether-options no-flow-control` | flag (excl. flow) |  |  | Disable pause frames |
| `interfaces <interface-name> aggregated-ether-options` | container |  |  | Aggregated interface options |
| `interfaces <interface-name> aggregated-ether-options lacp` | presence |  |  | Link aggregation control protocol |
| `interfaces <interface-name> aggregated-ether-options lacp active` | flag (excl. lacp-mode) |  |  | Actively send LACPDUs |
| `interfaces <interface-name> aggregated-ether-options lacp passive` | flag (excl. lacp-mode) |  |  | Only respond to LACPDUs |
| `interfaces <interface-name> aggregated-ether-options lacp periodic` | leaf | fast \\| slow | fast | LACPDU interval |
| `interfaces <interface-name> aggregated-ether-options lacp system-priority` | leaf | &lt;priority&gt; 1..65535 | 32768 | LACP system priority |
| `interfaces <interface-name> aggregated-ether-options minimum-links` | leaf | &lt;links&gt; 1..64 | 1 | Minimum active links for the bundle to be up |
| `interfaces <interface-name> aggregated-ether-options hash-policy` | leaf | layer2 \\| layer2+3 \\| layer3+4 | layer3+4 | Load-balancing hash |
| `interfaces <interface-name> storm-control` | container |  |  | Rate limit flooded traffic |
| `interfaces <interface-name> storm-control broadcast` | leaf | &lt;pps&gt; 1..100000000 |  | Broadcast packets per second |
| `interfaces <interface-name> storm-control multicast` | leaf | &lt;pps&gt; 1..100000000 |  | Multicast packets per second |
| `interfaces <interface-name> mac-limit` | leaf | &lt;count&gt; 1..131072 |  | Maximum learned MAC addresses |
| `interfaces <interface-name> offload` | container |  |  | Per-interface hardware acceleration |
| `interfaces <interface-name> offload disable` | flag |  |  | Never offload this interface |
| `interfaces <interface-name> native-vlan-id` | leaf | &lt;vlan&gt; |  | Untagged VLAN on a trunk port |
| `interfaces <interface-name> vlan-tagging` | flag |  |  | Routed subinterfaces: each unit takes the frames with its vlan-id |
| `interfaces <interface-name> unit <unit>` | list | &lt;unit&gt; 0..16385 |  | Logical unit |
| `interfaces <interface-name> unit <unit> description` | leaf | &lt;text&gt; |  | Unit description |
| `interfaces <interface-name> unit <unit> disable` | flag |  |  | Administratively disable the unit |
| `interfaces <interface-name> unit <unit> vlan-id` | leaf | &lt;vlan-id&gt; 1..4094 |  | 802.1Q tag of a routed subinterface (needs vlan-tagging) |
| `interfaces <interface-name> unit <unit> family` | container |  |  | Protocol family |
| `interfaces <interface-name> unit <unit> family ethernet-switching` | presence |  |  | Layer 2 switching (unit 0 only) |
| `interfaces <interface-name> unit <unit> family ethernet-switching interface-mode` | leaf | access \\| trunk |  | Port mode |
| `interfaces <interface-name> unit <unit> family ethernet-switching vlan` | container |  |  | VLAN membership |
| `interfaces <interface-name> unit <unit> family ethernet-switching vlan members` | leaf-list | &lt;vlan&gt; |  | VLAN names or ids (ranges like 10-20 allowed) |
| `interfaces <interface-name> unit <unit> family inet` | presence |  |  | IPv4 (routed interface) |
| `interfaces <interface-name> unit <unit> family inet address <address/prefix>` | list | &lt;address/prefix&gt; |  | Interface address |
| `interfaces <interface-name> unit <unit> family inet address <address/prefix> member` | leaf | &lt;member-id&gt; 1..16 |  | Only on this member (irb units) |
| `interfaces <interface-name> unit <unit> family inet dhcp` | flag |  |  | Obtain the IPv4 address via DHCP |
| `interfaces <interface-name> unit <unit> family inet6` | presence |  |  | IPv6 (routed interface) |
| `interfaces <interface-name> unit <unit> family inet6 address <address/prefix>` | list | &lt;address/prefix&gt; |  | Interface address |
| `interfaces <interface-name> unit <unit> family inet6 address <address/prefix> member` | leaf | &lt;member-id&gt; 1..16 |  | Only on this member (irb units) |
| `interfaces <interface-name> management` | flag |  |  | Management port: carries only cme (the stack's management address, on the master) |
| `vlans <name>` | list | &lt;name&gt; |  | VLAN configuration |
| `vlans <name> vlan-id` | leaf | &lt;vlan-id&gt; 1..4094 |  | 802.1Q VLAN id |
| `vlans <name> description` | leaf | &lt;text&gt; |  | VLAN description |
| `vlans <name> l3-interface` | leaf | &lt;irb-unit&gt; |  | VLAN IP interface (routing between VLANs) |
| `vlans <name> mtu` | leaf | &lt;mtu&gt; 256..16000 |  | Maximum frame size within this VLAN (same meaning as interface mtu) |
| `vlans <name> vxlan` | container |  |  | Extend this VLAN over VXLAN |
| `vlans <name> vxlan vni` | leaf | &lt;vni&gt; 1..16777214 |  | VXLAN network identifier |
| `protocols` | container |  |  | Protocol configuration |
| `protocols lldp` | presence |  |  | Link layer discovery protocol (802.1AB); the stack is one system |
| `protocols lldp disable` | flag |  |  | Stop LLDP on every port |
| `protocols lldp interface <interface-name>` | list | &lt;interface-name&gt; |  | Ports LLDP runs on (default: all) |
| `protocols lldp interface <interface-name> disable` | flag |  |  | No LLDP on this port |
| `protocols lldp advertisement-interval` | leaf | &lt;seconds&gt; 5..32768 | 30 | Seconds between LLDPDUs |
| `protocols lldp hold-multiplier` | leaf | &lt;multiplier&gt; 2..10 | 4 | Time to live in advertisement intervals |
| `protocols rstp` | presence |  |  | Rapid spanning tree (802.1w) |
| `protocols rstp bridge-priority` | leaf | &lt;priority&gt; 0..61440 in steps of 4096 | 32768 | Bridge priority |
| `protocols rstp hello-time` | leaf | &lt;seconds&gt; 1..10 | 2 | Hello interval in seconds |
| `protocols rstp max-age` | leaf | &lt;seconds&gt; 6..40 | 20 | Maximum BPDU age in seconds |
| `protocols rstp forward-delay` | leaf | &lt;seconds&gt; 4..30 | 15 | Forward delay in seconds |
| `protocols rstp interface <interface-name>` | list | &lt;interface-name&gt; |  | RSTP port settings |
| `protocols rstp interface <interface-name> cost` | leaf | &lt;cost&gt; 1..200000000 |  | Port path cost |
| `protocols rstp interface <interface-name> priority` | leaf | &lt;priority&gt; 0..240 in steps of 16 | 128 | Port priority |
| `protocols rstp interface <interface-name> edge` | flag |  |  | Port connects to an end host (fast transition) |
| `protocols rstp interface <interface-name> no-root-port` | flag |  |  | Root guard: never become root port |
| `protocols rstp interface <interface-name> mode` | leaf | point-to-point \\| shared |  | Link type |
| `protocols rstp interface <interface-name> disable` | flag |  |  | Do not run RSTP on this port |
| `protocols rstp disable` | flag |  |  | Disable RSTP |
| `protocols igmp-snooping` | presence |  |  | IGMP snooping (on by default in every VLAN) |
| `protocols igmp-snooping disable` | flag |  |  | Snooping off in every VLAN |
| `protocols igmp-snooping vlan <vlan>` | list | &lt;vlan&gt; |  | Per-VLAN settings (all: every VLAN) |
| `protocols igmp-snooping vlan <vlan> disable` | flag |  |  | Snooping off in this VLAN |
| `protocols igmp-snooping vlan <vlan> querier` | flag |  |  | Send general queries in this VLAN |
| `protocols igmp-snooping vlan <vlan> version` | leaf | &lt;version&gt; 2..3 | 2 | Version of the queries |
| `protocols igmp-snooping interface <interface-name>` | list | &lt;interface-name&gt; |  | Per-port settings |
| `protocols igmp-snooping interface <interface-name> immediate-leave` | flag |  |  | A leave removes the port at once |
| `protocols igmp-snooping interface <interface-name> multicast-router-interface` | flag |  |  | Always send all group traffic here |
| `protocols mld-snooping` | presence |  |  | MLD snooping (on by default in every VLAN) |
| `protocols mld-snooping disable` | flag |  |  | Snooping off in every VLAN |
| `protocols mld-snooping vlan <vlan>` | list | &lt;vlan&gt; |  | Per-VLAN settings (all: every VLAN) |
| `protocols mld-snooping vlan <vlan> disable` | flag |  |  | Snooping off in this VLAN |
| `protocols mld-snooping vlan <vlan> querier` | flag |  |  | Send general queries in this VLAN |
| `protocols mld-snooping vlan <vlan> version` | leaf | &lt;version&gt; 1..2 | 1 | Version of the queries |
| `protocols mld-snooping interface <interface-name>` | list | &lt;interface-name&gt; |  | Per-port settings |
| `protocols mld-snooping interface <interface-name> immediate-leave` | flag |  |  | A leave removes the port at once |
| `protocols mld-snooping interface <interface-name> multicast-router-interface` | flag |  |  | Always send all group traffic here |
| `protocols ospf` | presence |  |  | OSPF version 2 (IPv4) |
| `protocols ospf area <area-id>` | list | &lt;area-id&gt; |  | OSPF area |
| `protocols ospf area <area-id> interface <unit-name>` | list | &lt;unit-name&gt; |  | Routed unit in this area |
| `protocols ospf area <area-id> interface <unit-name> passive` | flag |  |  | Announce the subnets, form no neighbours |
| `protocols ospf area <area-id> interface <unit-name> metric` | leaf | &lt;metric&gt; 1..65535 |  | Cost (default: reference-bandwidth / speed) |
| `protocols ospf area <area-id> interface <unit-name> interface-type` | leaf | p2p |  | Network type |
| `protocols ospf area <area-id> interface <unit-name> priority` | leaf | &lt;priority&gt; 0..255 | 128 | DR election priority (0: never DR) |
| `protocols ospf area <area-id> interface <unit-name> hello-interval` | leaf | &lt;seconds&gt; 1..255 | 10 | Seconds between hellos |
| `protocols ospf area <area-id> interface <unit-name> dead-interval` | leaf | &lt;seconds&gt; 2..65535 |  | Seconds without hellos until a neighbour is down (default 4 x hello) |
| `protocols ospf area <area-id> interface <unit-name> retransmit-interval` | leaf | &lt;seconds&gt; 1..65535 | 5 | Seconds until an unacknowledged LSA is sent again |
| `protocols ospf area <area-id> interface <unit-name> transit-delay` | leaf | &lt;seconds&gt; 1..65535 | 1 | Seconds added to LSA ages when flooding |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `protocols ospf area <area-id> interface <unit-name> authentication` | container |  |  | OSPF authentication |
| `protocols ospf area <area-id> interface <unit-name> authentication simple-password` | leaf (excl. auth) | &lt;key&gt; |  | Plain-text password (8 characters at most) |
| `protocols ospf area <area-id> interface <unit-name> authentication md5 <key-id>` | list (excl. auth) | &lt;key-id&gt; 0..255 |  | MD5 key (several: rollover) |
| `protocols ospf area <area-id> interface <unit-name> authentication md5 <key-id> key` | leaf | &lt;key&gt; |  | Shared secret |
| `protocols ospf export` | leaf-list | &lt;name&gt; |  | Policies redistributing routes into OSPF |
| `protocols ospf reference-bandwidth` | leaf | &lt;bandwidth&gt; | 100g | Bandwidth with cost 1 |
| `protocols ospf overload` | presence |  |  | Announce maximum metric (no transit traffic) |
| `protocols ospf overload timeout` | leaf | &lt;seconds&gt; 60..1800 |  | Seconds after every start (default: always) |
| `protocols ospf graceful-restart` | container |  |  | Graceful restart (RFC 3623) |
| `protocols ospf graceful-restart disable` | flag |  |  | No graceful restart |
| `protocols ospf graceful-restart restart-duration` | leaf | &lt;seconds&gt; 1..3600 | 120 | Seconds a restart may take |
| `protocols ospf disable` | flag |  |  | Configured but not running |
| `protocols ospf3` | presence |  |  | OSPFv3 (IPv6) |
| `protocols ospf3 area <area-id>` | list | &lt;area-id&gt; |  | OSPF area |
| `protocols ospf3 area <area-id> interface <unit-name>` | list | &lt;unit-name&gt; |  | Routed unit in this area |
| `protocols ospf3 area <area-id> interface <unit-name> passive` | flag |  |  | Announce the subnets, form no neighbours |
| `protocols ospf3 area <area-id> interface <unit-name> metric` | leaf | &lt;metric&gt; 1..65535 |  | Cost (default: reference-bandwidth / speed) |
| `protocols ospf3 area <area-id> interface <unit-name> interface-type` | leaf | p2p |  | Network type |
| `protocols ospf3 area <area-id> interface <unit-name> priority` | leaf | &lt;priority&gt; 0..255 | 128 | DR election priority (0: never DR) |
| `protocols ospf3 area <area-id> interface <unit-name> hello-interval` | leaf | &lt;seconds&gt; 1..255 | 10 | Seconds between hellos |
| `protocols ospf3 area <area-id> interface <unit-name> dead-interval` | leaf | &lt;seconds&gt; 2..65535 |  | Seconds without hellos until a neighbour is down (default 4 x hello) |
| `protocols ospf3 area <area-id> interface <unit-name> retransmit-interval` | leaf | &lt;seconds&gt; 1..65535 | 5 | Seconds until an unacknowledged LSA is sent again |
| `protocols ospf3 area <area-id> interface <unit-name> transit-delay` | leaf | &lt;seconds&gt; 1..65535 | 1 | Seconds added to LSA ages when flooding |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `protocols ospf3 export` | leaf-list | &lt;name&gt; |  | Policies redistributing routes into OSPF |
| `protocols ospf3 reference-bandwidth` | leaf | &lt;bandwidth&gt; | 100g | Bandwidth with cost 1 |
| `protocols ospf3 overload` | presence |  |  | Announce maximum metric (no transit traffic) |
| `protocols ospf3 overload timeout` | leaf | &lt;seconds&gt; 60..1800 |  | Seconds after every start (default: always) |
| `protocols ospf3 graceful-restart` | container |  |  | Graceful restart (RFC 3623) |
| `protocols ospf3 graceful-restart disable` | flag |  |  | No graceful restart |
| `protocols ospf3 graceful-restart restart-duration` | leaf | &lt;seconds&gt; 1..3600 | 120 | Seconds a restart may take |
| `protocols ospf3 disable` | flag |  |  | Configured but not running |
| `protocols bgp` | presence |  |  | BGP-4 (IPv4 and IPv6 unicast) |
| `protocols bgp group <name>` | list | &lt;name&gt; |  | Neighbour group |
| `protocols bgp group <name> type` | leaf | internal \\| external |  | Internal or external BGP |
| `protocols bgp group <name> neighbor <ip-address>` | list | &lt;ip-address&gt; |  | Neighbour address |
| `protocols bgp group <name> neighbor <ip-address> description` | leaf | &lt;text&gt; |  | Description |
| `protocols bgp group <name> neighbor <ip-address> peer-as` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of the neighbour |
| `protocols bgp group <name> neighbor <ip-address> local-address` | leaf | &lt;ip-address&gt; |  | Source address of the session |
| `protocols bgp group <name> neighbor <ip-address> local-as` | leaf | &lt;asn&gt; 1..4294967295 |  | Local AS towards this neighbour (AS migration) |
| `protocols bgp group <name> neighbor <ip-address> authentication-key` | leaf | &lt;secret&gt; |  | TCP MD5 signature secret |
| `protocols bgp group <name> neighbor <ip-address> hold-time` | leaf | &lt;seconds&gt; 0..65535 |  | Hold time in seconds (0: no keepalives) |
| `protocols bgp group <name> neighbor <ip-address> passive` | flag |  |  | Only accept connections, never connect |
| `protocols bgp group <name> neighbor <ip-address> multihop` | presence |  |  | The neighbour is not directly connected |
| `protocols bgp group <name> neighbor <ip-address> multihop ttl` | leaf | &lt;ttl&gt; 1..255 |  | TTL of the session's packets |
| `protocols bgp group <name> neighbor <ip-address> family` | container |  |  | Address families |
| `protocols bgp group <name> neighbor <ip-address> family inet` | container |  |  | IPv4 |
| `protocols bgp group <name> neighbor <ip-address> family inet unicast` | flag |  |  | IPv4 unicast routes |
| `protocols bgp group <name> neighbor <ip-address> family inet6` | container |  |  | IPv6 |
| `protocols bgp group <name> neighbor <ip-address> family inet6 unicast` | flag |  |  | IPv6 unicast routes |
| `protocols bgp group <name> neighbor <ip-address> import` | leaf-list | &lt;name&gt; |  | Import policies |
| `protocols bgp group <name> neighbor <ip-address> export` | leaf-list | &lt;name&gt; |  | Export policies |
| `protocols bgp group <name> neighbor <ip-address> remove-private` | flag |  |  | Remove private AS numbers towards external neighbours |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `protocols bgp group <name> neighbor <ip-address> graceful-restart` | container |  |  | Graceful restart (RFC 4724) |
| `protocols bgp group <name> neighbor <ip-address> graceful-restart disable` | flag |  |  | No graceful restart |
| `protocols bgp group <name> neighbor <ip-address> graceful-restart restart-time` | leaf | &lt;seconds&gt; 1..4095 | 120 | Seconds the neighbour keeps our routes |
| `protocols bgp group <name> neighbor <ip-address> graceful-restart stale-routes-time` | leaf | &lt;seconds&gt; 1..3600 | 300 | Seconds we keep a restarting neighbour's routes |
| `protocols bgp group <name> neighbor <ip-address> disable` | flag |  |  | Configured, but no session |
| `protocols bgp group <name> multipath` | presence |  |  | Install equal BGP paths as ECMP |
| `protocols bgp group <name> multipath multiple-as` | flag |  |  | Also across neighbouring ASes |
| `protocols bgp group <name> cluster` | leaf | &lt;ipv4-address&gt; |  | Route reflector cluster id (the neighbours are clients) |
| `protocols bgp group <name> description` | leaf | &lt;text&gt; |  | Description |
| `protocols bgp group <name> peer-as` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of the neighbour |
| `protocols bgp group <name> local-address` | leaf | &lt;ip-address&gt; |  | Source address of the session |
| `protocols bgp group <name> local-as` | leaf | &lt;asn&gt; 1..4294967295 |  | Local AS towards this neighbour (AS migration) |
| `protocols bgp group <name> authentication-key` | leaf | &lt;secret&gt; |  | TCP MD5 signature secret |
| `protocols bgp group <name> hold-time` | leaf | &lt;seconds&gt; 0..65535 |  | Hold time in seconds (0: no keepalives) |
| `protocols bgp group <name> passive` | flag |  |  | Only accept connections, never connect |
| `protocols bgp group <name> multihop` | presence |  |  | The neighbour is not directly connected |
| `protocols bgp group <name> multihop ttl` | leaf | &lt;ttl&gt; 1..255 |  | TTL of the session's packets |
| `protocols bgp group <name> family` | container |  |  | Address families |
| `protocols bgp group <name> family inet` | container |  |  | IPv4 |
| `protocols bgp group <name> family inet unicast` | flag |  |  | IPv4 unicast routes |
| `protocols bgp group <name> family inet6` | container |  |  | IPv6 |
| `protocols bgp group <name> family inet6 unicast` | flag |  |  | IPv6 unicast routes |
| `protocols bgp group <name> import` | leaf-list | &lt;name&gt; |  | Import policies |
| `protocols bgp group <name> export` | leaf-list | &lt;name&gt; |  | Export policies |
| `protocols bgp group <name> remove-private` | flag |  |  | Remove private AS numbers towards external neighbours |
| `protocols bgp group <name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `protocols bgp group <name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `protocols bgp group <name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `protocols bgp group <name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `protocols bgp group <name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `protocols bgp group <name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `protocols bgp group <name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `protocols bgp group <name> graceful-restart` | container |  |  | Graceful restart (RFC 4724) |
| `protocols bgp group <name> graceful-restart disable` | flag |  |  | No graceful restart |
| `protocols bgp group <name> graceful-restart restart-time` | leaf | &lt;seconds&gt; 1..4095 | 120 | Seconds the neighbour keeps our routes |
| `protocols bgp group <name> graceful-restart stale-routes-time` | leaf | &lt;seconds&gt; 1..3600 | 300 | Seconds we keep a restarting neighbour's routes |
| `protocols bgp group <name> disable` | flag |  |  | Configured, but no session |
| `protocols bgp disable` | flag |  |  | Configured but not running |
| `protocols layer2-control` | container |  |  | Layer 2 protocol protection |
| `protocols layer2-control bpdu-block` | container |  |  | Shut down ports that receive BPDUs |
| `protocols layer2-control bpdu-block interface` | leaf-list | &lt;interface-name&gt; |  | Protected interfaces |
| `protocols layer2-control bpdu-block disable-timeout` | leaf | &lt;seconds&gt; 10..86400 |  | Seconds until a blocked port is re-enabled (never if unset) |
| `mclag` | container |  |  | Multi-chassis link aggregation (bundles with ports on two members) |
| `mclag delay-restore` | leaf | &lt;seconds&gt; 0..3600 | 300 | Seconds to wait after a boot before MC-LAG legs join their bundles |
| `switch-options` | container |  |  | Global switching options |
| `switch-options mac-table-aging-time` | leaf | &lt;seconds&gt; 10..1000000 | 300 | MAC table aging time in seconds |
| `switch-options vxlan` | container |  |  | VXLAN to VTEPs outside the stack (the stack is one VTEP) |
| `switch-options vxlan source-address` | leaf | &lt;ipv4-address&gt; |  | The stack's VTEP address (on every member) |
| `switch-options vxlan udp-port` | leaf | &lt;port&gt; 1..65535 | 4789 | VXLAN UDP destination port |
| `switch-options vxlan remote-vtep <ip-address>` | list | &lt;ip-address&gt; |  | Remote VTEP |
| `switch-options vxlan remote-vtep <ip-address> vni` | leaf-list | &lt;vni&gt; 1..16777214 |  | VNIs to extend to this VTEP |
| `routing-options` | container |  |  | Routing of the default instance |
| `routing-options static` | container |  |  | Static routes |
| `routing-options static route <prefix>` | list | &lt;prefix&gt; |  | Destination network |
| `routing-options static route <prefix> next-hop` | leaf-list | &lt;ip-address&gt; |  | Gateway addresses (several: ECMP) |
| `routing-options static route <prefix> discard` | flag |  |  | Drop matching traffic silently |
| `routing-options static route <prefix> preference` | leaf | &lt;preference&gt; 0..255 |  | Preference instead of 5 (lower wins) |
| `routing-options router-id` | leaf | &lt;ipv4-address&gt; |  | Router id of OSPF and BGP |
| `routing-options autonomous-system` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of BGP |
| `routing-instances <instance-name>` | list | &lt;instance-name&gt; |  | Separate routing tables (VRFs); system management-instance names the management instance |
| `routing-instances <instance-name> description` | leaf | &lt;text&gt; |  | Instance description |
| `routing-instances <instance-name> instance-type` | leaf | virtual-router | virtual-router | Instance type |
| `routing-instances <instance-name> interface` | leaf-list | &lt;unit-name&gt; |  | Routed units in this instance (irb.10, 1/0/5.0) |
| `routing-instances <instance-name> routing-options` | container |  |  | Routing of this instance |
| `routing-instances <instance-name> routing-options static` | container |  |  | Static routes |
| `routing-instances <instance-name> routing-options static route <prefix>` | list | &lt;prefix&gt; |  | Destination network |
| `routing-instances <instance-name> routing-options static route <prefix> next-hop` | leaf-list | &lt;ip-address&gt; |  | Gateway addresses (several: ECMP) |
| `routing-instances <instance-name> routing-options static route <prefix> discard` | flag |  |  | Drop matching traffic silently |
| `routing-instances <instance-name> routing-options static route <prefix> preference` | leaf | &lt;preference&gt; 0..255 |  | Preference instead of 5 (lower wins) |
| `routing-instances <instance-name> routing-options router-id` | leaf | &lt;ipv4-address&gt; |  | Router id of OSPF and BGP |
| `routing-instances <instance-name> routing-options autonomous-system` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of BGP |
| `routing-instances <instance-name> protocols` | container |  |  | Routing protocols of this instance |
| `routing-instances <instance-name> protocols ospf` | presence |  |  | OSPF version 2 (IPv4) |
| `routing-instances <instance-name> protocols ospf area <area-id>` | list | &lt;area-id&gt; |  | OSPF area |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name>` | list | &lt;unit-name&gt; |  | Routed unit in this area |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> passive` | flag |  |  | Announce the subnets, form no neighbours |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> metric` | leaf | &lt;metric&gt; 1..65535 |  | Cost (default: reference-bandwidth / speed) |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> interface-type` | leaf | p2p |  | Network type |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> priority` | leaf | &lt;priority&gt; 0..255 | 128 | DR election priority (0: never DR) |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> hello-interval` | leaf | &lt;seconds&gt; 1..255 | 10 | Seconds between hellos |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> dead-interval` | leaf | &lt;seconds&gt; 2..65535 |  | Seconds without hellos until a neighbour is down (default 4 x hello) |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> retransmit-interval` | leaf | &lt;seconds&gt; 1..65535 | 5 | Seconds until an unacknowledged LSA is sent again |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> transit-delay` | leaf | &lt;seconds&gt; 1..65535 | 1 | Seconds added to LSA ages when flooding |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> authentication` | container |  |  | OSPF authentication |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> authentication simple-password` | leaf (excl. auth) | &lt;key&gt; |  | Plain-text password (8 characters at most) |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> authentication md5 <key-id>` | list (excl. auth) | &lt;key-id&gt; 0..255 |  | MD5 key (several: rollover) |
| `routing-instances <instance-name> protocols ospf area <area-id> interface <unit-name> authentication md5 <key-id> key` | leaf | &lt;key&gt; |  | Shared secret |
| `routing-instances <instance-name> protocols ospf export` | leaf-list | &lt;name&gt; |  | Policies redistributing routes into OSPF |
| `routing-instances <instance-name> protocols ospf reference-bandwidth` | leaf | &lt;bandwidth&gt; | 100g | Bandwidth with cost 1 |
| `routing-instances <instance-name> protocols ospf overload` | presence |  |  | Announce maximum metric (no transit traffic) |
| `routing-instances <instance-name> protocols ospf overload timeout` | leaf | &lt;seconds&gt; 60..1800 |  | Seconds after every start (default: always) |
| `routing-instances <instance-name> protocols ospf graceful-restart` | container |  |  | Graceful restart (RFC 3623) |
| `routing-instances <instance-name> protocols ospf graceful-restart disable` | flag |  |  | No graceful restart |
| `routing-instances <instance-name> protocols ospf graceful-restart restart-duration` | leaf | &lt;seconds&gt; 1..3600 | 120 | Seconds a restart may take |
| `routing-instances <instance-name> protocols ospf disable` | flag |  |  | Configured but not running |
| `routing-instances <instance-name> protocols ospf3` | presence |  |  | OSPFv3 (IPv6) |
| `routing-instances <instance-name> protocols ospf3 area <area-id>` | list | &lt;area-id&gt; |  | OSPF area |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name>` | list | &lt;unit-name&gt; |  | Routed unit in this area |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> passive` | flag |  |  | Announce the subnets, form no neighbours |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> metric` | leaf | &lt;metric&gt; 1..65535 |  | Cost (default: reference-bandwidth / speed) |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> interface-type` | leaf | p2p |  | Network type |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> priority` | leaf | &lt;priority&gt; 0..255 | 128 | DR election priority (0: never DR) |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> hello-interval` | leaf | &lt;seconds&gt; 1..255 | 10 | Seconds between hellos |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> dead-interval` | leaf | &lt;seconds&gt; 2..65535 |  | Seconds without hellos until a neighbour is down (default 4 x hello) |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> retransmit-interval` | leaf | &lt;seconds&gt; 1..65535 | 5 | Seconds until an unacknowledged LSA is sent again |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> transit-delay` | leaf | &lt;seconds&gt; 1..65535 | 1 | Seconds added to LSA ages when flooding |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `routing-instances <instance-name> protocols ospf3 area <area-id> interface <unit-name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `routing-instances <instance-name> protocols ospf3 export` | leaf-list | &lt;name&gt; |  | Policies redistributing routes into OSPF |
| `routing-instances <instance-name> protocols ospf3 reference-bandwidth` | leaf | &lt;bandwidth&gt; | 100g | Bandwidth with cost 1 |
| `routing-instances <instance-name> protocols ospf3 overload` | presence |  |  | Announce maximum metric (no transit traffic) |
| `routing-instances <instance-name> protocols ospf3 overload timeout` | leaf | &lt;seconds&gt; 60..1800 |  | Seconds after every start (default: always) |
| `routing-instances <instance-name> protocols ospf3 graceful-restart` | container |  |  | Graceful restart (RFC 3623) |
| `routing-instances <instance-name> protocols ospf3 graceful-restart disable` | flag |  |  | No graceful restart |
| `routing-instances <instance-name> protocols ospf3 graceful-restart restart-duration` | leaf | &lt;seconds&gt; 1..3600 | 120 | Seconds a restart may take |
| `routing-instances <instance-name> protocols ospf3 disable` | flag |  |  | Configured but not running |
| `routing-instances <instance-name> protocols bgp` | presence |  |  | BGP-4 (IPv4 and IPv6 unicast) |
| `routing-instances <instance-name> protocols bgp group <name>` | list | &lt;name&gt; |  | Neighbour group |
| `routing-instances <instance-name> protocols bgp group <name> type` | leaf | internal \\| external |  | Internal or external BGP |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address>` | list | &lt;ip-address&gt; |  | Neighbour address |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> description` | leaf | &lt;text&gt; |  | Description |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> peer-as` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of the neighbour |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> local-address` | leaf | &lt;ip-address&gt; |  | Source address of the session |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> local-as` | leaf | &lt;asn&gt; 1..4294967295 |  | Local AS towards this neighbour (AS migration) |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> authentication-key` | leaf | &lt;secret&gt; |  | TCP MD5 signature secret |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> hold-time` | leaf | &lt;seconds&gt; 0..65535 |  | Hold time in seconds (0: no keepalives) |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> passive` | flag |  |  | Only accept connections, never connect |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> multihop` | presence |  |  | The neighbour is not directly connected |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> multihop ttl` | leaf | &lt;ttl&gt; 1..255 |  | TTL of the session's packets |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> family` | container |  |  | Address families |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> family inet` | container |  |  | IPv4 |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> family inet unicast` | flag |  |  | IPv4 unicast routes |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> family inet6` | container |  |  | IPv6 |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> family inet6 unicast` | flag |  |  | IPv6 unicast routes |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> import` | leaf-list | &lt;name&gt; |  | Import policies |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> export` | leaf-list | &lt;name&gt; |  | Export policies |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> remove-private` | flag |  |  | Remove private AS numbers towards external neighbours |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> graceful-restart` | container |  |  | Graceful restart (RFC 4724) |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> graceful-restart disable` | flag |  |  | No graceful restart |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> graceful-restart restart-time` | leaf | &lt;seconds&gt; 1..4095 | 120 | Seconds the neighbour keeps our routes |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> graceful-restart stale-routes-time` | leaf | &lt;seconds&gt; 1..3600 | 300 | Seconds we keep a restarting neighbour's routes |
| `routing-instances <instance-name> protocols bgp group <name> neighbor <ip-address> disable` | flag |  |  | Configured, but no session |
| `routing-instances <instance-name> protocols bgp group <name> multipath` | presence |  |  | Install equal BGP paths as ECMP |
| `routing-instances <instance-name> protocols bgp group <name> multipath multiple-as` | flag |  |  | Also across neighbouring ASes |
| `routing-instances <instance-name> protocols bgp group <name> cluster` | leaf | &lt;ipv4-address&gt; |  | Route reflector cluster id (the neighbours are clients) |
| `routing-instances <instance-name> protocols bgp group <name> description` | leaf | &lt;text&gt; |  | Description |
| `routing-instances <instance-name> protocols bgp group <name> peer-as` | leaf | &lt;asn&gt; 1..4294967295 |  | AS number of the neighbour |
| `routing-instances <instance-name> protocols bgp group <name> local-address` | leaf | &lt;ip-address&gt; |  | Source address of the session |
| `routing-instances <instance-name> protocols bgp group <name> local-as` | leaf | &lt;asn&gt; 1..4294967295 |  | Local AS towards this neighbour (AS migration) |
| `routing-instances <instance-name> protocols bgp group <name> authentication-key` | leaf | &lt;secret&gt; |  | TCP MD5 signature secret |
| `routing-instances <instance-name> protocols bgp group <name> hold-time` | leaf | &lt;seconds&gt; 0..65535 |  | Hold time in seconds (0: no keepalives) |
| `routing-instances <instance-name> protocols bgp group <name> passive` | flag |  |  | Only accept connections, never connect |
| `routing-instances <instance-name> protocols bgp group <name> multihop` | presence |  |  | The neighbour is not directly connected |
| `routing-instances <instance-name> protocols bgp group <name> multihop ttl` | leaf | &lt;ttl&gt; 1..255 |  | TTL of the session's packets |
| `routing-instances <instance-name> protocols bgp group <name> family` | container |  |  | Address families |
| `routing-instances <instance-name> protocols bgp group <name> family inet` | container |  |  | IPv4 |
| `routing-instances <instance-name> protocols bgp group <name> family inet unicast` | flag |  |  | IPv4 unicast routes |
| `routing-instances <instance-name> protocols bgp group <name> family inet6` | container |  |  | IPv6 |
| `routing-instances <instance-name> protocols bgp group <name> family inet6 unicast` | flag |  |  | IPv6 unicast routes |
| `routing-instances <instance-name> protocols bgp group <name> import` | leaf-list | &lt;name&gt; |  | Import policies |
| `routing-instances <instance-name> protocols bgp group <name> export` | leaf-list | &lt;name&gt; |  | Export policies |
| `routing-instances <instance-name> protocols bgp group <name> remove-private` | flag |  |  | Remove private AS numbers towards external neighbours |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection` | container |  |  | BFD failure detection for the neighbours |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection minimum-interval` | leaf | &lt;ms&gt; 50..60000 | 300 | Transmit and receive interval in milliseconds |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection multiplier` | leaf | &lt;n&gt; 1..255 | 3 | Missed packets until the neighbour is down |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection authentication` | container |  |  | BFD authentication |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection authentication algorithm` | leaf | keyed-sha-1 \\| keyed-md5 |  | Authentication algorithm |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection authentication key` | leaf | &lt;secret&gt; |  | Shared secret |
| `routing-instances <instance-name> protocols bgp group <name> bfd-liveness-detection authentication key-id` | leaf | &lt;key-id&gt; 0..255 | 1 | Key id |
| `routing-instances <instance-name> protocols bgp group <name> graceful-restart` | container |  |  | Graceful restart (RFC 4724) |
| `routing-instances <instance-name> protocols bgp group <name> graceful-restart disable` | flag |  |  | No graceful restart |
| `routing-instances <instance-name> protocols bgp group <name> graceful-restart restart-time` | leaf | &lt;seconds&gt; 1..4095 | 120 | Seconds the neighbour keeps our routes |
| `routing-instances <instance-name> protocols bgp group <name> graceful-restart stale-routes-time` | leaf | &lt;seconds&gt; 1..3600 | 300 | Seconds we keep a restarting neighbour's routes |
| `routing-instances <instance-name> protocols bgp group <name> disable` | flag |  |  | Configured, but no session |
| `routing-instances <instance-name> protocols bgp disable` | flag |  |  | Configured but not running |
| `forwarding-options` | container |  |  | Forwarding options |
| `forwarding-options analyzer <name>` | list | &lt;name&gt; |  | Port mirroring session |
| `forwarding-options analyzer <name> input` | container |  |  | Traffic to mirror |
| `forwarding-options analyzer <name> input ingress` | container |  |  | Traffic received |
| `forwarding-options analyzer <name> input ingress interface` | leaf-list | &lt;interface-name&gt; |  | Source interfaces |
| `forwarding-options analyzer <name> input ingress vlan` | leaf-list | &lt;vlan&gt; |  | Source VLANs |
| `forwarding-options analyzer <name> input egress` | container |  |  | Traffic transmitted |
| `forwarding-options analyzer <name> input egress interface` | leaf-list | &lt;interface-name&gt; |  | Source interfaces |
| `forwarding-options analyzer <name> output` | container |  |  | Mirror destination |
| `forwarding-options analyzer <name> output interface` | leaf | &lt;interface-name&gt; |  | Destination interface |
| `policy-options` | container |  |  | Routing policies |
| `policy-options prefix-list <name>` | list | &lt;name&gt; |  | List of prefixes |
| `policy-options prefix-list <name> prefix` | leaf-list | &lt;prefix&gt; |  | Prefixes |
| `policy-options community <name>` | list | &lt;name&gt; |  | Named community set |
| `policy-options community <name> members` | leaf-list | &lt;community&gt; |  | Community members |
| `policy-options as-path <name>` | list | &lt;name&gt; |  | Named AS path expression |
| `policy-options as-path <name> path` | leaf | &lt;regex&gt; |  | Regular expression over AS numbers |
| `policy-options policy-statement <name>` | list | &lt;name&gt; |  | Routing policy |
| `policy-options policy-statement <name> term <name>` | list | &lt;name&gt; |  | Term (evaluated in order) |
| `policy-options policy-statement <name> term <name> from` | container |  |  | Match conditions (all must match) |
| `policy-options policy-statement <name> term <name> from protocol` | leaf-list | direct \\| local \\| static \\| ospf \\| ospf3 \\| bgp \\| aggregate |  | Route source |
| `policy-options policy-statement <name> term <name> from route-filter <prefix>` | list | &lt;prefix&gt; |  | Prefix match |
| `policy-options policy-statement <name> term <name> from route-filter <prefix> exact` | flag (excl. match) |  |  | Only this prefix |
| `policy-options policy-statement <name> term <name> from route-filter <prefix> orlonger` | flag (excl. match) |  |  | This prefix and more specific ones |
| `policy-options policy-statement <name> term <name> from route-filter <prefix> longer` | flag (excl. match) |  |  | Only more specific prefixes |
| `policy-options policy-statement <name> term <name> from route-filter <prefix> upto` | leaf (excl. match) | &lt;/n&gt; |  | Up to this prefix length (/n) |
| `policy-options policy-statement <name> term <name> from route-filter <prefix> prefix-length-range` | leaf (excl. match) | &lt;/a-/b&gt; |  | Prefix lengths (/a-/b) |
| `policy-options policy-statement <name> term <name> from prefix-list` | leaf-list | &lt;name&gt; |  | Exact prefixes of a prefix list |
| `policy-options policy-statement <name> term <name> from prefix-list-filter <name>` | list | &lt;name&gt; |  | Prefix list with a match type |
| `policy-options policy-statement <name> term <name> from prefix-list-filter <name> match` | leaf | exact \\| orlonger \\| longer |  | Match type |
| `policy-options policy-statement <name> term <name> from community` | leaf-list | &lt;name&gt; |  | Communities (all members) |
| `policy-options policy-statement <name> term <name> from as-path` | leaf-list | &lt;name&gt; |  | AS path expressions |
| `policy-options policy-statement <name> term <name> from neighbor` | leaf-list | &lt;ip-address&gt; |  | BGP neighbour |
| `policy-options policy-statement <name> term <name> from area` | leaf-list | &lt;area-id&gt; |  | OSPF area |
| `policy-options policy-statement <name> term <name> from family` | leaf | inet \\| inet6 |  | Address family |
| `policy-options policy-statement <name> term <name> from tag` | leaf | &lt;metric&gt; 0..4294967295 |  | OSPF external route tag |
| `policy-options policy-statement <name> term <name> then` | container |  |  | Actions |
| `policy-options policy-statement <name> term <name> then accept` | flag (excl. flow) |  |  | Accept (ends the evaluation) |
| `policy-options policy-statement <name> term <name> then reject` | flag (excl. flow) |  |  | Reject (ends the evaluation) |
| `policy-options policy-statement <name> term <name> then next` | leaf (excl. flow) | term \\| policy |  | Continue with the next term or policy |
| `policy-options policy-statement <name> term <name> then metric` | leaf | &lt;metric&gt; 0..4294967295 |  | BGP MED / OSPF external metric |
| `policy-options policy-statement <name> term <name> then metric-add` | leaf | &lt;metric&gt; 0..4294967295 |  | Add to the metric |
| `policy-options policy-statement <name> term <name> then local-preference` | leaf | &lt;metric&gt; 0..4294967295 |  | BGP local preference |
| `policy-options policy-statement <name> term <name> then preference` | leaf | &lt;preference&gt; 0..255 |  | Preference in this switch's RIB |
| `policy-options policy-statement <name> term <name> then community` | container |  |  | Community changes |
| `policy-options policy-statement <name> term <name> then community add` | leaf-list | &lt;name&gt; |  | Add the members of these communities |
| `policy-options policy-statement <name> term <name> then community delete` | leaf-list | &lt;name&gt; |  | Remove the members of these communities |
| `policy-options policy-statement <name> term <name> then community set` | leaf-list | &lt;name&gt; |  | Replace the communities |
| `policy-options policy-statement <name> term <name> then as-path-prepend` | leaf | &lt;as-path&gt; |  | AS numbers to prepend ("65000 65000") |
| `policy-options policy-statement <name> term <name> then next-hop` | leaf | self\|discard\|&lt;ip&gt; |  | Next hop |
| `policy-options policy-statement <name> term <name> then external-type` | leaf | 1 \\| 2 |  | OSPF external type |
| `policy-options policy-statement <name> term <name> then tag` | leaf | &lt;metric&gt; 0..4294967295 |  | OSPF external route tag |
| `policy-options policy-statement <name> then` | container |  |  | Actions for routes no term terminated |
| `policy-options policy-statement <name> then accept` | flag (excl. flow) |  |  | Accept |
| `policy-options policy-statement <name> then reject` | flag (excl. flow) |  |  | Reject |
<!-- END GENERATED STATEMENT INDEX -->
