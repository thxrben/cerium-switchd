# mclag Configuration Reference

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
   - [5.2 stack](#52-stack)
   - [5.3 interfaces](#53-interfaces)
   - [5.4 vlans](#54-vlans)
   - [5.5 protocols](#55-protocols)
   - [5.6 mclag](#56-mclag)
   - [5.7 switch-options](#57-switch-options)
   - [5.8 routing-options](#58-routing-options)
   - [5.9 forwarding-options](#59-forwarding-options)
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

* Every switch is a **stack member** with an id from 1 to 16. A switch without any `stack` configuration is member 1.
* Interface names carry the member id, so the whole stack is configured in one place.
* **switchd only touches interfaces that appear under `interfaces`, or that an `interface-range` selects**, plus
  management and underlay ports that are configured explicitly.
  * A NIC that is not selected stays exactly as the OS left it: it is not brought up, not bridged, and its addresses are
    not changed. New NICs never start switching traffic unless a wildcard `interface-range` selects them on purpose (5.3.1).
  * An interface that is configured but not physically present (not plugged in, or on a member that has not joined yet)
    keeps its configuration. The configuration is applied as soon as the interface appears. `commit check` warns about this.
  * An interface that is **removed** from the configuration (or no longer selected by a range) is **released**: it is
    taken administratively down first, then removed from the bridge or bundle. Its MTU and description are left as
    they are. It does not return to the state before switchd managed it, because a released port that stayed up
    could leak traffic into whatever network it is cabled to.
  * switchd creates and owns the bridge `swbr0` and one bond device per `ae` interface (named like the `ae`). It never
    modifies other bridges, bonds or VLAN devices.

### 1.5 The three planes

Every port belongs to exactly one plane. Traffic never crosses from one plane to another inside the switch.

| Plane | Ports | Carries | IP |
|---|---|---|---|
| **Data plane** | Switch ports (`unit 0 family ethernet-switching`), bundles, MC-LAG peer-links, VXLAN tunnels | Client traffic only | none on any port |
| **Stacking plane** | Dedicated **stacking ports** (e.g. 1G), direct 1:1 cables between members | Stack configuration, member state, MC-LAG synchronisation, RSTP coordination, BFD | none: IP-less protocol |
| **Management plane** | One IP interface per member, on a VLAN (IRB-like) or a dedicated port, in VRF `mgmt` | SSH, web/API, syslog, NTP, DNS, MC-LAG BFD heartbeat | yes, in VRF `mgmt` only |

Rules that follow from this:

* **Stacking protocol frames on data ports are client traffic.** The stacking protocol uses untagged frames with
  EtherType `0x88b5`. switchd receives these frames *only* on designated stacking ports (and peer-link ports, for
  micro-BFD; see 5.6). On access, trunk or VXLAN ports, a frame with this EtherType is **never interpreted and never
  influences stacking or MC-LAG**. It is switched like any other frame and is not dropped.
* The protocols a switch port legitimately terminates are the data plane's own link-local protocols. LACP is consumed on
  bundle members. BPDUs are consumed when RSTP runs, and trigger `bpdu-block`.
* **No IP on data or stacking ports.** switchd disables IPv6 (link-local addresses, router solicitations, neighbour
  discovery) on all of them and never assigns addresses. IP exists only on management interfaces (VRF `mgmt`) and the
  VXLAN underlay interface (default VRF).
* The management VRF has no route into the data plane's VLANs. The switch never routes between VLANs, or between
  management and data traffic.
* An in-band management VLAN on the data trunks is supported (`stack member <id> management vlan`). Management then
  shares the fate of the data plane. Commit confirmation and the serial console are the safety nets. A separate port,
  set as an access port in the management VLAN, avoids that.

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
* `show chassis hardware` lists every port with its Linux name, PCI address, driver and MAC address.
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
  VLAN filter offload, checksum/TSO/GRO, and whether switchd's rules on the port are in hardware.
* The PCIe link of each NIC (negotiated vs. possible speed and width) is part of the system diagnostics (PLAN.md Phase 13).

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
| `show vlans` | VLANs with their ports (`*` = tagged). |
| `show ethernet-switching table [vlan <v>] [interface <if>]` | Learned and static MAC addresses. `clear ethernet-switching table …` removes learned ones. |
| `show arp [no-resolve]` | The IPv4 neighbour table of all routing instances (default and `mgmt`), including entries of interfaces the operating system manages (e.g. its own management NIC). Columns: MAC address, IP address, interface (switch name where it is a port), instance, state. |
| `show ipv6 neighbors` | The same for IPv6. |
| `show system uptime` | Current time, when the system booted, when switchd started, when and by whom the configuration was last changed, load averages. |
| `show system commit`, `show system rollback …` | See 4.1. |
| `show log`, `show system syslog`, `show version` | Recent log messages, remote syslog state, software version. |
| `request system reboot\|halt\|power-off [in <minutes>]` | After a confirmation prompt (`[yes,no] (no)`), reboots, halts or powers off this member, now or in n minutes. Every CLI session is notified. `clear system reboot` cancels a scheduled one. With stacking and MC-LAG, the member first moves its traffic to the peers (LACP out of sync, stacking links drained); until then it simply shuts down. |
| `start shell` | A Linux shell (4.3); `exit` returns to the CLI. |

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
| change `system login`, `system services`, `stack` | ✔ | – | – |
| `request system reboot/halt`, `request stack …` | ✔ | – | – |
| `start shell` (Linux shell as the logged-in user; `exit` returns to the CLI) | ✔ | – | – |

An operator whose candidate touches a forbidden hierarchy gets an error at commit time naming the forbidden paths.

---

## 5. Statement reference

Each statement lists **behaviour**, **interactions** with related statements, and the **commit check** rules
(E = error, W = warning). Defaults apply whenever a statement is absent.

### 5.1 system

#### `system host-name <hostname>`
Name of the stack as a whole. It appears in syslog messages as the application name prefix, and in the CLI
prompt for members that have no `stack member <id> host-name`. Default: `switch`.
* The member's host name (its `stack member <id> host-name`, else `system host-name`) is also the operating system's
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
* switchd's own lookups (e.g. syslog server names) go through the management VRF when it is configured.
* W: more than 3 servers (only the first 3 are used).

#### `system ntp server <host> [prefer]`
NTP servers, queried from the management VRF. `prefer` marks the preferred source. Without any server, the OS time
configuration stays untouched. Correct time matters for logs, certificates and the stack's TLS.

#### `system syslog host <host> { … }`
Sends log messages to a remote server, from the management VRF. The format is RFC 5424, with the member host name as HOSTNAME.
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
* `root` is not managed. Its password is maintained by the OS (used with `login-required` consoles and SSH).
* W: a user with neither password nor key (they cannot log in).

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
* It accepts connections from the management VRF (and, before a management interface is configured, from any
  interface of the host).

#### `system services web-management { port <n>; certificate <file>; key <file>; disable; }`
HTTPS web interface and REST API in the management VRF. Default: port 443 with a self-signed certificate generated
at first start. `certificate` and `key` must be given together (E otherwise). `disable` turns it off.

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

### 5.2 stack

#### Stacking ports (not part of the configuration)

Stacking ports connect members **directly** (1:1 cables, no switch in between). Chain, ring and any mesh are
supported. Messages between members that are not directly connected are relayed hop by hop along the shortest
working path, so a ring survives one broken cable.

* **Designation**: stacking ports are set per switch with the operational command `request stack port add <card>/<port>`
  (and `… delete`), e.g. `request stack port add 0/2`. The member part of the name is left out because a switch that
  has not joined yet does not know its member id. The setting is stored locally, like Junos VC ports, because a switch
  needs its stacking ports *before* it can receive the stack configuration. `show stack ports` and
  `show stack topology` display them.
* A stacking port is never a data or management port. E: the port is configured under `interfaces`, or as a
  management/underlay interface. Wildcard `interface-range`s skip stacking ports.
* **Protocol**: untagged Ethernet frames with EtherType `0x88b5`, no IP and no VLAN tag. Each stacking link carries a
  reliable stream (sequence numbers, acknowledgements, retransmission, fragmentation to the link MTU). **TLS 1.3 with
  mutual certificate authentication** runs on top, using the stack's own CA. Frames from unauthenticated devices are ignored.
* **Joining**: a new switch with designated stacking ports announces itself on them. `request stack join token <t>`
  on the new switch, or `request stack member add <id> token <t>` on the stack, authorises it. It then receives its
  certificate and the configuration.
* Stack control needs a majority of members (Raft). Without a majority, the data plane keeps forwarding with the last
  committed configuration, and only commits are blocked.

#### `stack bfd { minimum-interval <ms>; multiplier <n>; }`
BFD (RFC 5880 state machine, carried IP-less inside the stacking protocol) on every stacking link. A link is declared
down after `minimum-interval × multiplier` without packets. Defaults: 100 ms × 3 = 300 ms.
* BFD runs with real-time scheduling priority, so CPU load does not cause false detections.
* Values below 100 ms can still cause false detections on small ARM boards. A false detection makes stacking paths
  re-route, but never drops data traffic by itself.

#### `stack member <1-16> { … }`
Declares a stack member and its per-member settings. Configuration for a member that has not joined yet is kept and
applied when it joins. Without any `stack member` entry the switch is standalone member 1, and interfaces of other
members are rejected (E).
* `host-name <hostname>`: sets the Linux host name and the CLI prompt of this member.
  E: the same host name on two members.
* `priority <0-255>`: default 128. The highest priority healthy member becomes the stack leader (it coordinates
  commits). In an MC-LAG domain the higher priority member is *primary* (ties: lower member id).
* `role switch|witness`: a `witness` member only takes part in stack quorum, over its own stacking cables. It keeps
  a two-switch stack able to commit when one switch is down. E: interfaces configured on a witness.
* `vtep-address <ip>`: source address of this member's VXLAN tunnels (5.7). If it differs from the underlay address,
  it is added to a loopback interface and must be routable in the underlay.

#### `stack member <id> management { vlan <vlan> | interface <interface-name>; address [ … ]; dhcp; gateway [ … ]; }`
The member's management IP interface, in VRF `mgmt` (1.5). SSH, the web interface, syslog, NTP, DNS and the MC-LAG
BFD heartbeat use it.
* `vlan <vlan>` (IRB-like): an IP interface on this VLAN of the member's bridge. The VLAN is switched normally as
  well. Hosts in it reach the switch and each other. W: no switch port of this member carries the VLAN
  (the address would be unreachable). E: the VLAN is not defined.
* `interface <interface-name>`: a dedicated port of this member that is **not** switched. E: the port is a switch
  port, a bundle member or a stacking port, or it belongs to another member.
* `vlan` and `interface` are mutually exclusive.
* `address [ <address/prefix> … ]`: static IPv4 and/or IPv6 addresses.
* `dhcp`: IPv4 address via DHCP. E: together with a static IPv4 address (a static IPv6 address is fine).
* `gateway [ <ip> … ]`: default route(s) of the management VRF, at most one per address family (E).
  W: a gateway of a family without an address of that family.
* E: `address`, `dhcp` or `gateway` without `vlan`/`interface`.
* **Without a `management` block, switchd does not touch the host's existing network configuration.** This is the safe
  default for the first installation. Once you configure it, switchd takes over, and commit confirmation protects you
  against locking yourself out.

#### `stack member <id> underlay { vlan <vlan> | interface <interface-name>; address [ … ]; gateway [ … ]; }`
The IP interface that carries this member's VXLAN tunnels, in the default VRF (not `mgmt`). It has the same structure
as `management`, and the same rules apply for `vlan`/`interface`, addresses and gateways.
* `gateway` is used only for routes to remote VTEPs that are not directly connected. No default route is installed.
* E: management and underlay on the same VLAN (they live in different VRFs).
* E: the underlay VLAN is itself extended over VXLAN (tunnel traffic would loop into the tunnel).
* W: underlay shares the management interface.
* W: underlay MTU (the dedicated port's `mtu`, or the VLAN's `mtu`) is smaller than the largest VXLAN VLAN MTU plus
  encapsulation overhead (5.7).

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
  * Wildcards **never** select stacking ports, the member's management/underlay port, or a port that has IP
    addresses configured by the operating system (e.g. the installer's management NIC before the takeover).
    Naming such a port explicitly under `interfaces` is allowed but gives a W, because it can cut management access.
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
| neither | **Plain port**: up, MTU applied, not switched. Used as a mirror destination or underlay NIC. |

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
  * `system-priority` (default 32768). The LACP system id is the member's bridge MAC. On MC-LAG bundles the domain's
    shared `system-mac`/`system-priority` replace both (W if `system-priority` is set on an MC-LAG bundle).
* `minimum-links <n>`: the bundle is operationally down while fewer than n member ports are active. On an MC-LAG
  bundle this counts ports on **both** members. Default 1. W: n larger than the number of configured member ports.
* `hash-policy layer2|layer2+3|layer3+4`: how flows are spread across the active members of **this** switch.
  Default `layer3+4`. Non-IP traffic always uses layer 2. Frames of one flow always take the same link, so order is preserved.
* `mclag`: this bundle is an **MC-LAG**, with member ports on the two members of an MC-LAG domain, which present
  themselves to the partner as one LACP system (5.6). A bundle with ports on only one of the two members is allowed
  (a single-homed device, or during a migration).
  * E: `lacp` missing.
  * E: there is no domain containing the bundle's members.
  * E: the bundle is the peer-link.

  Rules for all bundles:
  * E: an `ae` with ports on two members that is neither `mclag` nor a domain's `peer-link`.
  * E: ports on more than two members.

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
* `family inet` and `family inet6` each take several `address` entries. `family inet6` also gets a link-local address.
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
* Addresses follow the same rules as routed interfaces (above).
* In a stack, the irb interface exists on **every member that has the VLAN**, with the same addresses and the same MAC
  address (derived from the stack), so every member routes locally (anycast gateway). With MC-LAG, both peers
  answer for the gateway address.
* The management address is **not** an irb interface: it stays per member in `stack member <id> management` (5.2),
  in VRF `mgmt`, with no routing to or from the data VLANs.
* In the kernel, `irb.<n>` is a VLAN device on the bridge, and the bridge itself joins the VLAN (bridge self VLAN).

**Routing** happens only between switchd's own L3 interfaces (irb, routed ports and subinterfaces): IPv4 forwarding is
enabled per interface on them only. Interfaces the operating system manages (e.g. its installer management NIC)
do not forward. For IPv6, Linux can only enable forwarding for the whole system, so switchd sets `accept_ra 2` on the
OS-managed interfaces that use router advertisements first (they keep their SLAAC addresses and default routes).
* W: routed interfaces exist while the operating system's management NIC is in the default instance (not moved to
  `stack member <id> management`): data VLANs can then reach the management network through the OS routes.
* ICMP redirects are not sent. Reverse-path filtering is loose (`rp_filter 2`) on routed interfaces.

### 5.4 vlans

`vlans <name> { … }` defines a VLAN (a broadcast domain). The name is what you reference in `vlan members`.

#### `vlan-id <1-4094>`
802.1Q id. E: missing. E: the same id in two VLANs. No VLAN id is reserved: all of 1–4094 are available.
A VLAN exists on a member's bridge only if a port of that member, the peer-link or VXLAN needs it.

#### `description <text>`
Free text.

#### `mtu <256-16000>`
Maximum frame size (1.3) **within this VLAN**, independent of port MTUs. Use it for example to allow jumbo frames
only in a storage VLAN (`mtu 9014` for 9000-byte hosts) while the trunks carry `mtu 9216`.
* Frames larger than this are dropped when they are **received** (on any port or tunnel), and counted per VLAN in `show vlans extensive`.
* Implemented as an ingress filter (tc/nftables). It is offloaded where possible and costs a little CPU per frame in software.
  Without `mtu` no VLAN filter is installed, and only port MTUs apply.
* W: a member port with a smaller MTU (frames that fit the VLAN are dropped at that port).
* If the VLAN is extended over VXLAN, the tunnel MTU follows the largest VXLAN VLAN MTU (5.7).

#### `l3-interface irb.<n>`
Attaches the VLAN IP interface `irb.<n>` (5.3.3) to this VLAN. E: the irb unit is not configured. E: the same irb unit
on two VLANs. E: the management VLAN of a member (`stack member <id> management vlan`) has an l3-interface (management
and data routing are kept apart).

#### `vxlan vni <vni>`
Extends the VLAN over VXLAN to every other member that has the same VLAN, and to static remote VTEPs listing the VNI.
The VLAN id is the same on all members (the configuration is shared), and frames are carried untagged inside the tunnel.
* E: VNI used twice.
* E: a (non-witness) member without `vtep-address`.
* Details in 5.7.

### 5.5 protocols

#### `protocols rstp { … }`
Enables Rapid Spanning Tree (IEEE 802.1w) on **all** switch ports of all members: every interface with
`unit 0 family ethernet-switching`, and every `ae` (a bundle is one RSTP port; its member ports never run RSTP themselves).
`interface` entries only tune individual ports.

These are never part of RSTP:
* **peer-links**: always forwarding,
* **VXLAN tunnels**: the VXLAN mesh is loop-free by design (5.7),
* **plain ports**.

The two members of an MC-LAG domain act as **one** RSTP bridge. They use the same bridge id, the primary member
computes the state of MC-LAG ports, and BPDUs received on the secondary's leg are relayed over the peer-link.
Neighbours therefore see one switch, and one peer failing does not cause a topology change.

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

#### `protocols layer2-control bpdu-block { interface [ <if> … ]; disable-timeout <s>; }`
BPDU protection. It works with or without RSTP. A listed port that receives any BPDU (STP/RSTP/MSTP, or Cisco PVST+
`01:00:0c:cc:cc:cd`) is **shut down immediately**, an alarm is raised, and a syslog `error` is sent.
* The port stays down until `clear error bpdu interface <if>`, or until `disable-timeout` seconds have passed (if set).
* On an `ae`, the whole bundle is shut down.
* E: the interface is not configured or is a bundle member.
* W: the port runs RSTP as a non-edge port (any neighbouring switch would shut it down).

### 5.6 mclag

#### `mclag domain <1-255> { … }`
An MC-LAG domain is a pair of stack members that act as one LACP partner towards devices connected to both
(`aggregated-ether-options mclag`). Each member can be in at most one domain.

The domain uses all three planes, each for its own purpose:
* **Data plane**: the **peer-link**, a pure data link between the pair.
* **Stacking plane**: MAC synchronisation, bundle state, consistency checks and RSTP relay between the two members.
  If they are not cabled directly, messages are relayed by other members.
* **Management plane**: the BFD **heartbeat**, which decides who stays active when the other paths fail.

Statements:
* `members [ <a> <b> ]`: exactly two configured, non-witness stack members.
  E: not exactly two. E: unknown member. E: witness. E: member already in another domain.
* `peer-link <aeN>`: the data bundle that connects the two members directly.
  * It must have ports on both members (E). Each member's side is a local bundle between the two switches. With `lacp`,
    each side uses its own LACP system id, not the shared one.
  * It carries **all VLANs, tagged**, and nothing else. Its own ethernet-switching settings are ignored (W). It has no IP.
  * The peer-link must be at least as large as every MC-LAG bundle (E: MTU smaller than an MC-LAG bundle of the domain).
  * It is never blocked by RSTP.
  * **Split horizon**: traffic that arrives over the peer-link is never sent out of an MC-LAG bundle that is up on the
    receiving member, because the peer already delivered it on its own leg. When a member's leg of a bundle fails,
    this filter is lifted for that bundle, and the peer's traffic reaches the device via the peer-link.
* `peer-link-bfd { minimum-interval <ms>; multiplier <n>; }`: micro-BFD on **each physical port** of the peer-link.
  It is IP-less (stacking-protocol EtherType, authenticated with keys agreed over the stacking plane) and is consumed
  only on peer-link ports.
  * A port that stops forwarding while its link stays up (for example a broken media converter) leaves the bundle after
    `interval × multiplier` (default 100 ms × 3). LACP alone would need 3 seconds.
  * The port rejoins when BFD is up again.
* `heartbeat { minimum-interval <ms>; multiplier <n>; }`: BFD over UDP (RFC 5881/5883) between the members'
  management addresses, authenticated. Default 300 ms × 3.
  * It carries no configuration or state. Its only job is to tell "peer dead" apart from "paths to the peer cut" (split-brain).
  * W: a member without a management address.
* `system-mac <mac>` / `system-priority <n>`: the shared LACP system id presented by both members on MC-LAG bundles.
  If `system-mac` is unset, a stable locally administered MAC is derived from the stack id and domain id. It never
  changes, because a change would make partners re-negotiate. Default priority 32768.
* `delay-restore <s>`: after a member boots or rejoins, its MC-LAG ports stay out of the bundle for this long
  (default 300 s). During that time the MAC table is synchronised and RSTP converges before traffic is attracted.
* `anycast-vtep <ip>`: VXLAN source address shared by the pair. Remote VTEPs send traffic for devices behind MC-LAG
  bundles to this one address, and either member can receive it (5.7).

**Behaviour:**

* **MAC synchronisation** (stacking plane):
  * A MAC learned on an MC-LAG bundle is installed on the peer on the same bundle.
  * A MAC ages out only when it has aged out on **both** members.
  * MACs learned on single-homed ports are installed on the peer pointing to the peer-link.
* **Failure handling** (*primary* = higher `stack member priority`, ties: lower id):
  | Stacking path | Peer-link | Heartbeat | Interpretation | Behaviour |
  |---|---|---|---|---|
  | down | down | down | peer dead | Survivor carries all traffic as primary. |
  | down | down | **up** | peer alive, both paths cut | **Secondary** takes its MC-LAG ports out of the bundles (LACP out-of-sync). The primary carries all traffic. |
  | up | down | any | peer-link cut | Same as above: the secondary disables its MC-LAG legs, because frames could no longer be delivered via the peer. |
  | down | up | any | stacking path cut | MAC sync pauses. Forwarding continues. Learned MACs are flooded via the peer-link until the stacking path returns. An alarm is raised. |
  | – | one port down | – | peer-link degraded | The port leaves the peer-link bundle (micro-BFD or link loss). The others continue. |
  | A member's leg of an MC-LAG bundle fails | | | | The bundle continues on the other member. The receiving member lifts split horizon for that bundle, and MACs point to the peer-link. |
  | Member returns | | | | `delay-restore` applies, then its legs rejoin. |
* **Consistency checks** at runtime: VLAN membership, MTU, LACP mode and rate of each MC-LAG bundle are compared
  between the members. A mismatch (for example because a member runs an older software version) keeps the
  bundle's leg on the secondary down, with the reason shown in `show mclag consistency`.

### 5.7 switch-options

#### `switch-options mac-table-aging-time <10-1000000>`
Seconds after which an unused dynamically learned MAC is removed. Default 300.
In MC-LAG domains, MACs age out only when both members age them out (5.6).

#### `switch-options vxlan { … }`
Global VXLAN settings. VXLAN is active on a member as soon as any VLAN has `vxlan vni`.

* `mode control-plane|flood-and-learn`: default `control-plane`.
  * `control-plane`: members tell each other over the stack's mTLS channel which MACs they have learned in each VNI.
    Remote MACs are installed directly, and data-plane learning on tunnels is off.
    Broadcast, unknown-unicast and multicast (BUM) traffic is replicated to every member that has the VNI (head-end replication).
  * `flood-and-learn`: remote MACs are learned from received tunnel traffic. BUM traffic is replicated in the same way.
* `udp-port <n>`: VXLAN UDP port. Default 4789.
* `remote-vtep <ip> { vni [ <vni> … ]; }`: a static VTEP outside the stack (a server or another vendor's switch).
  It receives BUM traffic for the listed VNIs, and its MACs are always learned from traffic.
  E: a listed VNI that is not mapped to a VLAN.
* `encryption`: tunnels between stack members run over WireGuard (keys are managed automatically). Static remote
  VTEPs are not encrypted. The cost is software crypto throughput and 60/80 bytes more overhead (see PLAN §6).

**Behaviour and interactions:**
* A member joins a VNI only if one of its switch ports is in the VLAN (or it is in an MC-LAG domain whose peer has
  such a port), so that BUM traffic reaches only members that need it.
* **Loop freedom**: frames received from a tunnel are never sent into another tunnel (split horizon), so the full
  mesh cannot loop. Tunnels do not run RSTP.
* **MTU**:
  * The tunnel accepts frames up to the largest `mtu` of the VXLAN VLANs (default 1514).
  * The underlay's `mtu` (frame size, 1.3) must be at least that plus 50 bytes (IPv4 underlay) or 70 (IPv6),
    plus 80 with `encryption`. For example, VXLAN VLANs with `mtu 9014` need an underlay `mtu` of 9064 (IPv4).
    W: underlay MTU too small.
  * Frames that do not fit are dropped at the tunnel and counted.
* **MC-LAG with `anycast-vtep`**: traffic of devices behind MC-LAG bundles is sent from the anycast address, and
  both members accept traffic to it. Single-homed devices use the member's own `vtep-address`.

### 5.8 routing-options

#### `routing-options static route <prefix> { next-hop [ <ip> … ]; discard; }`
Static routes of the default routing instance (the data plane, not management; management routes are
`stack member <id> management gateway`).
* `next-hop`: one or more gateway addresses. Several next hops share the traffic (ECMP). A next hop must be inside
  a subnet of an L3 interface of the default instance, else the route is inactive (W at commit; it becomes active
  once such an interface exists and is up).
* `discard`: drop matching traffic silently (blackhole). E: `discard` together with `next-hop`. E: neither.
* IPv4 and IPv6 prefixes can be mixed; a next hop must have the family of its prefix (E).
* switchd installs the routes with its own protocol id and only ever removes routes it installed.

### 5.9 forwarding-options

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
* Implemented with tc (`matchall`/`flower` + `mirred`), offloaded to hardware where supported.
* Commit check:
  * E: no output.
  * E: no input.
  * E: the output is also an input.
  * E: an input has no ports on the output's member (mirroring across the stack is not supported).
  * E: the output is a bundle spanning two members.
  * E: an input or output is not configured under `interfaces`, or is a bundle member (use the `ae`).
  * W: the output port is also a switch port.

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
   multicast are flooded to all ports in the VLAN, including VXLAN tunnels. Split horizon applies to peer-link
   and tunnels.
7. Mirroring copies are taken at ingress (step 1, before any filter) and at egress (as sent).

---

## 7. Complete examples

### 7.1 Standalone switch with access and trunk ports

```
system {
    host-name lab-sw;
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
stack {
    member 1 {
        host-name lab-sw;
        management {
            interface 1/3/0;
            address 192.168.1.20/24;
            gateway 192.168.1.1;
        }
    }
}
interfaces {
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
#   request stack port add eno2
set stack member 1 host-name sw-a
set stack member 1 management vlan mgmt
set stack member 1 management address 192.168.1.11/24
set stack member 1 management gateway 192.168.1.1
set stack member 1 vtep-address 10.255.0.1
set stack member 1 underlay interface 1/5/0
set stack member 1 underlay address 10.99.0.1/24
set stack member 2 host-name sw-b
set stack member 2 management vlan mgmt
set stack member 2 management address 192.168.1.12/24
set stack member 2 management gateway 192.168.1.1
set stack member 2 vtep-address 10.255.0.2
set stack member 2 underlay interface 2/5/0
set stack member 2 underlay address 10.99.0.2/24
set interfaces 1/5/0 mtu 9216
set interfaces 2/5/0 mtu 9216
set interfaces 1/2/0 ether-options 802.3ad ae0
set interfaces 1/2/1 ether-options 802.3ad ae0
set interfaces 2/2/0 ether-options 802.3ad ae0
set interfaces 2/2/1 ether-options 802.3ad ae0
set interfaces ae0 description peer-link
set interfaces ae0 mtu 9216
set interfaces ae0 aggregated-ether-options lacp active
set interfaces 1/1/0 ether-options 802.3ad ae1
set interfaces 2/1/0 ether-options 802.3ad ae1
set interfaces ae1 description "server A (dual-homed)"
set interfaces ae1 mtu 9216
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
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
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9014
set mclag domain 1 members [ 1 2 ]
set mclag domain 1 peer-link ae0
set protocols rstp
set forwarding-options analyzer debug input ingress interface ae1
set forwarding-options analyzer debug input egress interface ae1
set forwarding-options analyzer debug output interface 1/3/0
```

---

## 8. Implementation status

| Area | Status |
|---|---|
| Schema, formats (hierarchical, set, JSON), diff | implemented, tested and fuzzed |
| Commit check (the validation rules in this document) | implemented and tested, except where noted below |
| `interface-range` expansion (member-range, wildcards, precedence) | implemented and tested (hot-plug re-evaluation with the data plane) |
| `inactive:` / `activate` / `deactivate`, `replace:`/`delete:` tags, `load`, `copy`, `rename` | implemented and tested (config package); CLI commands next |
| Commit / confirmation / rollback engine (sessions, locks, revisions, persisted confirmation, automatic rollback) | implemented and tested (`internal/commit`); stack-wide replication in Phase 5 |
| CLI engine (modes, commands, pipes, completion, `?`) | implemented and tested (`internal/cli`), with swcli client and switchd (dry-run) |
| Hitless apply (diff-driven, tighten before loosen), self-healing, switch ports, VLANs, static bundles, MTU, storm control, mac-limit, flow control | implemented; unit, property and lab tested |
| `stack member <id> management` (VRF mgmt, IRB or dedicated port, static addresses, gateways) | implemented and lab tested; `dhcp` not yet |
| `system login user` (accounts, keys, classes, `plain-text-password`), `start shell` | implemented and lab tested |
| `system services ssh` (own sshd instance for the CLI), `system ports` (console CLI: serial and display, `login-required`) | implemented and lab tested |
| Interface numbering `<member>/<card>/<port>` (1.6), conversion of old names, `show chassis hardware` | implemented, unit and lab tested (also on physical hardware) |
| L3: `interfaces irb`, routed ports and subinterfaces, `vlans <v> l3-interface`, `routing-options static` | implemented, unit and lab tested (IPv4 and IPv6); anycast MAC across the stack with Phase 5 |
| `system host-name` / `stack member <id> host-name` in the OS, `system name-server`, `domain-name` | implemented and unit tested; switchd's own lookups through VRF mgmt not yet |
| Hardware capability checks (1.7), `show system offload` | implemented, unit tested and checked on physical NICs (tg3, igb, r8169) and virtio |
| Operational commands of 3.5 (`show arp`, `show system uptime`, `show system rollback`, `request system reboot` …) | implemented and tested; stack drain before reboot with Phase 5/7 |
| Multi-user notices, persistent shared candidate, CLI surviving switchd restarts and its own crashes | implemented, unit and lab tested |
| `system services web-management`, `system login message` on serial consoles | not implemented yet |
| `system syslog` (UDP/TCP/TLS from VRF mgmt, local buffer) | implemented and lab tested; kernel messages not yet forwarded |
| `vlans <v> mtu` (VLAN MTU filter) | specified, not implemented yet (needs a per-VLAN length filter; planned with eBPF) |
| Operator permission check at commit, OS account conflicts, cert/key pairing, time-zone check | with the respective subsystems |
| Stacking plane (IP-less transport, TLS, relay, BFD), stack ports | Phase 5 |
| Data plane, services, LACP, MC-LAG (incl. micro-BFD, heartbeat), RSTP, VXLAN | later phases (see PLAN.md §8) |

---

## 9. Statement index (generated)

All statements with their types, ranges and defaults, generated from the schema.
"excl." marks mutually exclusive statements.

<!-- BEGIN GENERATED STATEMENT INDEX (go test ./docs -update) -->
| Statement | Kind | Value | Default | Description |
|---|---|---|---|---|
| `system` | container |  |  | System parameters |
| `system host-name` | leaf | &lt;hostname&gt; |  | Name of the stack/system |
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
| `stack` | container |  |  | Stack (virtual chassis) members |
| `stack bfd` | container |  |  | BFD on stacking ports (IP-less) |
| `stack bfd minimum-interval` | leaf | &lt;ms&gt; 50..10000 | 100 | Transmit/receive interval in milliseconds |
| `stack bfd multiplier` | leaf | &lt;count&gt; 2..255 | 3 | Missed packets before the session goes down |
| `stack member <member-id>` | list | &lt;member-id&gt; 1..16 |  | Stack member |
| `stack member <member-id> host-name` | leaf | &lt;hostname&gt; |  | Host name of this member |
| `stack member <member-id> priority` | leaf | &lt;priority&gt; 0..255 | 128 | Priority for leader election (higher wins) |
| `stack member <member-id> role` | leaf | switch \\| witness | switch | Member role |
| `stack member <member-id> management` | container |  |  | Management IP interface of this member (management VRF) |
| `stack member <member-id> management vlan` | leaf (excl. mgmt-attach) | &lt;vlan&gt; |  | Attach the management IP to this VLAN (IRB-like) |
| `stack member <member-id> management interface` | leaf (excl. mgmt-attach) | &lt;interface-name&gt; |  | Dedicated, non-switched management port |
| `stack member <member-id> management address` | leaf-list | &lt;address/prefix&gt; |  | Static addresses (IPv4 and/or IPv6) |
| `stack member <member-id> management dhcp` | flag |  |  | Obtain the IPv4 address via DHCP |
| `stack member <member-id> management gateway` | leaf-list | &lt;ip-address&gt; |  | Default gateway, at most one per address family |
| `stack member <member-id> vtep-address` | leaf | &lt;ip-address&gt; |  | Local VXLAN tunnel endpoint address |
| `stack member <member-id> underlay` | container |  |  | Layer 3 interface carrying VXLAN tunnels (default VRF) |
| `stack member <member-id> underlay vlan` | leaf (excl. ul-attach) | &lt;vlan&gt; |  | Attach the underlay IP to this VLAN (IRB-like) |
| `stack member <member-id> underlay interface` | leaf (excl. ul-attach) | &lt;interface-name&gt; |  | Dedicated, non-switched underlay port |
| `stack member <member-id> underlay address` | leaf-list | &lt;address/prefix&gt; |  | Underlay addresses |
| `stack member <member-id> underlay gateway` | leaf-list | &lt;ip-address&gt; |  | Next hop towards remote VTEPs, at most one per address family |
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
| `interface-range <name> aggregated-ether-options mclag` | presence |  |  | Bundle spans the two members of an MC-LAG domain |
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
| `interface-range <name> unit <unit> family inet address` | leaf-list | &lt;address/prefix&gt; |  | Interface addresses |
| `interface-range <name> unit <unit> family inet6` | presence |  |  | IPv6 (routed interface) |
| `interface-range <name> unit <unit> family inet6 address` | leaf-list | &lt;address/prefix&gt; |  | Interface addresses |
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
| `interfaces <interface-name> aggregated-ether-options mclag` | presence |  |  | Bundle spans the two members of an MC-LAG domain |
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
| `interfaces <interface-name> unit <unit> family inet address` | leaf-list | &lt;address/prefix&gt; |  | Interface addresses |
| `interfaces <interface-name> unit <unit> family inet6` | presence |  |  | IPv6 (routed interface) |
| `interfaces <interface-name> unit <unit> family inet6 address` | leaf-list | &lt;address/prefix&gt; |  | Interface addresses |
| `vlans <name>` | list | &lt;name&gt; |  | VLAN configuration |
| `vlans <name> vlan-id` | leaf | &lt;vlan-id&gt; 1..4094 |  | 802.1Q VLAN id |
| `vlans <name> description` | leaf | &lt;text&gt; |  | VLAN description |
| `vlans <name> l3-interface` | leaf | &lt;irb-unit&gt; |  | VLAN IP interface (routing between VLANs) |
| `vlans <name> mtu` | leaf | &lt;mtu&gt; 256..16000 |  | Maximum frame size within this VLAN (same meaning as interface mtu) |
| `vlans <name> vxlan` | container |  |  | Extend this VLAN over VXLAN |
| `vlans <name> vxlan vni` | leaf | &lt;vni&gt; 1..16777214 |  | VXLAN network identifier |
| `protocols` | container |  |  | Protocol configuration |
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
| `protocols layer2-control` | container |  |  | Layer 2 protocol protection |
| `protocols layer2-control bpdu-block` | container |  |  | Shut down ports that receive BPDUs |
| `protocols layer2-control bpdu-block interface` | leaf-list | &lt;interface-name&gt; |  | Protected interfaces |
| `protocols layer2-control bpdu-block disable-timeout` | leaf | &lt;seconds&gt; 10..86400 |  | Seconds until a blocked port is re-enabled (never if unset) |
| `mclag` | container |  |  | Multi-chassis link aggregation |
| `mclag domain <domain-id>` | list | &lt;domain-id&gt; 1..255 |  | MC-LAG domain (a pair of stack members) |
| `mclag domain <domain-id> members` | leaf-list | &lt;member-id&gt; 1..16 |  | The two stack members forming this domain |
| `mclag domain <domain-id> peer-link` | leaf | &lt;ae-interface&gt; |  | Aggregated interface connecting the two peers |
| `mclag domain <domain-id> system-mac` | leaf | &lt;mac-address&gt; |  | Shared LACP system MAC (derived if unset) |
| `mclag domain <domain-id> system-priority` | leaf | &lt;priority&gt; 1..65535 | 32768 | Shared LACP system priority |
| `mclag domain <domain-id> anycast-vtep` | leaf | &lt;ip-address&gt; |  | Shared VTEP address of the pair |
| `mclag domain <domain-id> heartbeat` | container |  |  | BFD heartbeat over the management network (split-brain detection) |
| `mclag domain <domain-id> heartbeat minimum-interval` | leaf | &lt;ms&gt; 50..10000 | 300 | Transmit/receive interval in milliseconds |
| `mclag domain <domain-id> heartbeat multiplier` | leaf | &lt;count&gt; 2..255 | 3 | Missed packets before the session goes down |
| `mclag domain <domain-id> peer-link-bfd` | container |  |  | Micro-BFD on every peer-link port (IP-less) |
| `mclag domain <domain-id> peer-link-bfd minimum-interval` | leaf | &lt;ms&gt; 50..10000 | 100 | Transmit/receive interval in milliseconds |
| `mclag domain <domain-id> peer-link-bfd multiplier` | leaf | &lt;count&gt; 2..255 | 3 | Missed packets before the session goes down |
| `mclag domain <domain-id> delay-restore` | leaf | &lt;seconds&gt; 0..3600 | 300 | Seconds to wait after reboot before enabling MC-LAG ports |
| `switch-options` | container |  |  | Global switching options |
| `switch-options mac-table-aging-time` | leaf | &lt;seconds&gt; 10..1000000 | 300 | MAC table aging time in seconds |
| `switch-options vxlan` | container |  |  | VXLAN transport |
| `switch-options vxlan mode` | leaf | control-plane \\| flood-and-learn | control-plane | How remote MACs are learned |
| `switch-options vxlan udp-port` | leaf | &lt;port&gt; 1..65535 | 4789 | VXLAN UDP destination port |
| `switch-options vxlan remote-vtep <ip-address>` | list | &lt;ip-address&gt; |  | Static VTEP outside the stack |
| `switch-options vxlan remote-vtep <ip-address> vni` | leaf-list | &lt;vni&gt; 1..16777214 |  | VNIs to extend to this VTEP |
| `switch-options vxlan encryption` | flag |  |  | Encrypt VXLAN underlay traffic with WireGuard |
| `routing-options` | container |  |  | Routing of the default instance |
| `routing-options static` | container |  |  | Static routes |
| `routing-options static route <prefix>` | list | &lt;prefix&gt; |  | Destination network |
| `routing-options static route <prefix> next-hop` | leaf-list | &lt;ip-address&gt; |  | Gateway addresses (several: ECMP) |
| `routing-options static route <prefix> discard` | flag |  |  | Drop matching traffic silently |
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
<!-- END GENERATED STATEMENT INDEX -->
