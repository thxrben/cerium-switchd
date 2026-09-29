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
   - [5.8 forwarding-options](#58-forwarding-options)
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
| list | `interfaces 1/eth0 { … }` | Keyed entries. Entries are sorted naturally (`1/eth2` before `1/eth10`). |
| leaf | `mtu 9216;` | One value. Setting it again replaces the value. |
| leaf-list | `members [ 10 20 ];` | A set of values. `set` **adds** values (duplicates are ignored), `delete … <value>` removes one value, and `delete …` without a value removes all. The order of insertion is kept. |
| flag | `disable;` | Present or absent. |

**Mutually exclusive statements** (for example `lacp active` / `lacp passive`, or `management address` / `management dhcp`):
setting one silently removes the other.

**Abbreviations:** keywords may be abbreviated to any unique prefix (`sh int` → `show interfaces`,
`set int 1/eth0 unit 0 fam eth` → `family ethernet-switching`). Keyword enum values may also be abbreviated
(`interface-mode tr` → `trunk`). User-chosen names and numbers are never abbreviated.

### 1.2 Value types

| Type | Format | Notes |
|---|---|---|
| `<interface-name>` | `<member>/<linux-name>` or `ae<N>` | `1/enp3s0`, `2/eth0`, `ae0`…`ae4095`. The member is the stack member id (1–16). The Linux name is the kernel name, up to 15 characters. |
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

### 1.3 MTU semantics (differs from Junos)

`mtu` always means the **Linux MTU**: the maximum payload of an Ethernet frame, excluding the Ethernet header
(14 bytes), 802.1Q tags (4 bytes each) and the FCS (4 bytes). A host that uses `mtu 9000` needs switch ports
with `mtu 9000` too, not 9014 or 9018. The largest frame on the wire is `mtu + 14 + 4 × tags + 4`.

Junos `mtu` includes the Ethernet header. A Junos `mtu 9216` corresponds to `mtu 9198` (untagged) here.

### 1.4 Stack members and interface ownership

* Every switch is a **stack member** with an id from 1 to 16. A switch without any `stack` configuration is member 1.
* Interface names carry the member id, so the whole stack is configured in one place.
* **switchd only touches interfaces that appear under `interfaces`** (plus management and underlay interfaces
  that are configured explicitly). A NIC that is not in the configuration stays exactly as the OS left it:
  it is not brought up, not bridged, and its addresses are not changed. New NICs never start switching traffic
  on their own.
* **Switch ports never carry IP.** switchd disables IPv6 (and with it link-local addresses, router solicitations
  and neighbour discovery) on every switch port, bundle member, `ae` and peer-link, and never assigns addresses to
  them. IP exists only on the management interface/VLAN (5.2) and the VXLAN underlay interface.
* An interface that is configured but not physically present (not plugged in, or on a member that has not joined yet)
  keeps its configuration. The configuration is applied as soon as the interface appears. `commit check` warns about this.

---

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
    1/eth1 {
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
set interfaces 1/eth1 description "server A"
set interfaces 1/eth1 mtu 9000
set interfaces 1/eth1 unit 0 family ethernet-switching interface-mode access
set interfaces 1/eth1 unit 0 family ethernet-switching vlan members storage
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
    "1/eth1": {
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
  | `configure private` | Edit a private copy of the committed configuration. It is not allowed while the shared candidate has uncommitted changes. Your commit applies only your changes. If someone else committed in between, commit fails and asks you to run `update`. |
  | `configure exclusive` | Lock the configuration. Nobody else can commit or change the shared candidate until you leave. Uncommitted changes are discarded when you exit. |

  Leaving plain `configure` keeps uncommitted changes in the shared candidate, and you are warned about them.
  Leaving `private` or `exclusive` discards uncommitted changes after a confirmation prompt.

### 3.2 Configuration-mode commands

| Command | Effect |
|---|---|
| `set <path> [<value>]` | Create the statement or change its value. Missing parents are created. |
| `delete [<path> [<value>]]` | Remove the statement and everything below it. With a leaf-list value, only that value is removed. `delete` on its own asks for confirmation and removes everything below the current level. Deleting something absent prints `warning: statement not found` and is otherwise harmless. |
| `edit <path>` | Move the edit level to a container or list entry (it is created on the first `set` below it). |
| `up [<n>]`, `top`, `exit` | Move up one or n levels, go to the top, or leave the level (at the top: leave configuration mode). |
| `show [<path>]` | Show the candidate below the current level. |
| `copy <path> to <key>` | Duplicate a list entry: `copy interfaces 1/eth1 to 1/eth2`. |
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
    inactive: 1/eth7 {
        mtu 9000;
    }
}
```
In set format: `set interfaces 1/eth7 mtu 9000` followed by `deactivate interfaces 1/eth7`.
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
| `start shell` (Linux root shell) | ✔ | – | – |

An operator whose candidate touches a forbidden hierarchy gets an error at commit time naming the forbidden paths.

---

## 5. Statement reference

Each statement lists **behaviour**, **interactions** with related statements, and the **commit check** rules
(E = error, W = warning). Defaults apply whenever a statement is absent.

### 5.1 system

#### `system host-name <hostname>`
Name of the stack as a whole. It appears in syslog messages as the application name prefix, and in the CLI
prompt for members that have no `stack member <id> host-name`. Default: `switch`.

#### `system domain-name <hostname>`
DNS search domain written to the resolver configuration of every member (management VRF).

#### `system time-zone <tz>`
IANA time zone (e.g. `Europe/Berlin`). It sets the system time zone on all members, and CLI output shows local time.
An unknown zone is a runtime alarm, and UTC stays in effect. Default: UTC.

#### `system name-server [ <ip> … ]`
DNS resolvers. They are queried from the management VRF.
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
* `severity <level>`: send messages of this severity **or more severe**. Default `info`. `any` sends everything.
* `ca-certificate <path>`: PEM CA certificate used to verify a TLS server. Default: the OS CA store.
  The server certificate must match `<host>`.

#### `system syslog local-buffer-size <lines>`
Number of recent messages kept in memory per member for `show log`. Default 5000.

#### `system login message <text>`
Banner shown before authentication on SSH and serial logins.

#### `system login user <username> { … }`
Creates a local Linux account on **every** member. Its login shell is the CLI, so logging in via SSH or a serial
console lands directly in the CLI.
* `class super-user|operator|read-only`: permissions (4.3). Default `read-only`.
* `uid <1000-64000>`: numeric user id. Default: assigned automatically from 2000 upwards and kept stable afterwards.
* `full-name <text>`: stored as the account's GECOS field.
* `authentication encrypted-password <hash>`: a crypt(3) hash (`$6$…` SHA-512 or `$y$…` yescrypt). Use `plain-text-password` in the CLI to create one.
* `authentication ssh-key <key>`: an OpenSSH public key line. Several keys are allowed.
* Removing a user deletes the account, but its home directory is kept. switchd only ever modifies accounts that it created itself.
  Existing OS accounts with the same name are **not** taken over, and that conflict is reported as an E at commit.
* `root` is not managed. Its password is maintained by the OS, as a break-glass login on the serial console.
* W: a user with neither password nor key (they cannot log in).

#### `system services ssh { port <n>; root-login deny|allow|key-only; }`
switchd manages a drop-in configuration for the system's OpenSSH server. It listens in the management VRF
(including an in-band management VLAN). Password authentication is offered only to users with an `encrypted-password`.
Default: port 22, `root-login deny`.

#### `system services web-management { port <n>; certificate <file>; key <file>; disable; }`
HTTPS web interface and REST API in the management VRF. Default: port 443 with a self-signed certificate generated
at first start. `certificate` and `key` must be given together (E otherwise). `disable` turns it off.

#### `system commit confirmation { mode required|optional; timeout <minutes>; }`
See 4.2. Defaults: `required`, 10 minutes.

#### `system ports { no-auto-detect; console <tty> { speed <baud>; disable; } }`
Login on serial consoles (the login shell is the CLI).
* **Auto-detection** (the default) starts a login on each of the following:
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

#### `stack member <1-16> { … }`
Declares a stack member and its per-member settings. Members join with a token (`request stack join …`). Their
member id is assigned at join time and is **not** part of the configuration. Configuration for a member that has
not joined yet is kept and applied when it joins. Without any `stack member` entry, the switch is standalone
member 1, and interfaces of other members are rejected (E).
* `host-name <hostname>`: sets the Linux host name and the CLI prompt of this member.
  E: the same host name on two members.
* `priority <0-255>`: default 128. The highest priority healthy member becomes the stack leader (it coordinates
  commits). In an MC-LAG domain the higher priority member is *primary* (ties: lower member id).
* `role switch|witness`: a `witness` member only takes part in stack quorum. Use one to keep a two-switch stack
  able to commit when one switch is down. E: interfaces configured on a witness.
* `vtep-address <ip>`: source address of this member's VXLAN tunnels (5.7). If it differs from the underlay address,
  it is added to a loopback interface and must be routable in the underlay.

#### `stack member <id> management { … }`
Out-of-band management of this member. It is placed in a separate VRF (`mgmt`), so management traffic never mixes
with switched traffic and the switch never routes between management and data networks. SSH, the web interface,
syslog, NTP, DNS and stack communication all use this VRF.
* `interface <linux-name>`: dedicated management NIC. It is kept out of the bridge.
  E: the same port configured as a switch port under `interfaces`.
* `vlan <vlan-id>`: in-band management instead: an IP interface on this VLAN of the bridge.
  E: the VLAN is not defined. E: both `interface` and `vlan` are set.
* `address <address/prefix>` **or** `dhcp` (mutually exclusive).
* `gateway <ip>`: default route of the management VRF. W: `gateway` without `address`/`dhcp`.
* **Without a `management` block, switchd does not touch the host's existing network configuration of that NIC.**
  This is the safe default for first installation. Once you configure it, switchd takes over, and commit confirmation
  protects you against locking yourself out.

#### `stack member <id> underlay { interface <linux-name>; address <address/prefix>; gateway <ip>; }`
Layer 3 interface that carries VXLAN tunnels (default VRF, not the management VRF). `gateway` is used only for routes
to remote VTEPs that are not directly connected; no default route is installed. The underlay NIC's MTU is set by
configuring it under `interfaces <member>/<linux-name> mtu …` **without** `unit 0 family ethernet-switching`.
* E: the underlay interface is configured as a switch port.
* W: the underlay shares the management interface.
* W: underlay MTU < largest VXLAN VLAN MTU + encapsulation overhead (5.7).

### 5.3 interfaces

`interfaces <interface-name> { … }` configures a physical port (`<member>/<linux-name>`) or an aggregated
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
Linux MTU (1.3). Default 1500.
* The same value is used for receiving and sending. Frames larger than the MTU are dropped: on receive by the NIC,
  on send by the bridge. Both are counted in `show interfaces extensive` (`mtu-exceeded`).
* Bundle members always use the MTU of their `ae`. W: a different `mtu` on a member port (it is ignored).
* E: MTU above the NIC's hardware maximum (known once the member has reported its inventory).
* W: the port belongs to a VLAN whose `mtu` is larger than the port MTU. Such frames are dropped at this port.
* The effective limit for a frame is **min(ingress port MTU, VLAN MTU, egress port MTU)**. See also `vlans <v> mtu`.

#### `ether-options { 802.3ad <aeN>; flow-control | no-flow-control; }`
Only on physical ports (E on `ae`).
* `802.3ad <aeN>`: makes the port a member of `aeN`. E: `aeN` is not configured. E: the port also has
  `unit 0 family ethernet-switching`, `storm-control` or `mac-limit` (configure those on the `ae`).
  Still effective on a member port: `description`, `disable`, `ether-options flow-control`, `offload disable`.
* `flow-control` / `no-flow-control` (mutually exclusive): enable or disable IEEE 802.3x pause frames. Pause frames
  let a congested receiver slow the sender down instead of dropping frames. That helps against drops, at the cost of
  head-of-line blocking. Default: leave the driver's default.

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
Maximum number of MAC addresses **learned** on this port (kernel bridge learning limit). Once reached, new source
addresses are *not learned*, but their frames are **still forwarded**: replies to them are flooded instead of switched.
An alarm is raised. The switch never drops traffic because of the limit. Default: unlimited.

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
Makes the port a switch port. Only unit 0 exists. It is kept for Junos-style syntax.
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

### 5.4 vlans

`vlans <name> { … }` defines a VLAN (a broadcast domain). The name is what you reference in `vlan members`.

#### `vlan-id <1-4094>`
802.1Q id. E: missing. E: the same id in two VLANs. No VLAN id is reserved: all of 1–4094 are available.
A VLAN exists on a member's bridge only if a port of that member, the peer-link or VXLAN needs it.

#### `description <text>`
Free text.

#### `mtu <256-16000>`
Maximum payload of frames **within this VLAN**, independent of port MTUs. Use it for example to allow jumbo
frames only in a storage VLAN while trunks carry `mtu 9216`.
* Frames larger than this are dropped when they are **received** (on any port or tunnel), and counted per VLAN in `show vlans extensive`.
* Implemented as an ingress filter (tc/nftables). It is offloaded where possible and costs a little CPU per frame in software.
  Without `mtu` no VLAN filter is installed, and only port MTUs apply.
* W: a member port with a smaller MTU (frames that fit the VLAN are dropped at that port).
* If the VLAN is extended over VXLAN, the tunnel MTU follows the largest VXLAN VLAN MTU (5.7).

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

* `members [ <a> <b> ]`: exactly two configured, non-witness stack members.
  E: not exactly two. E: unknown member. E: witness. E: member already in another domain.
* `peer-link <aeN>`: the bundle that connects the two members directly.
  * It must have ports on both members (E). Each member's side is a local bundle between the two switches. With
    `lacp`, each side uses its own LACP system id, not the shared one.
  * The peer-link carries **all VLANs**, tagged. Its own ethernet-switching settings are ignored (W).
  * The peer-link must be at least as large as every MC-LAG bundle (E: MTU smaller than an MC-LAG bundle of the domain).
  * It is never blocked by RSTP.
  * The peer-link is a direct 1:1 cable between the two switches (one or more ports), with no switch in between.
    It carries **no IP** and no reserved VLAN. The members' **control session** runs directly on Ethernet:
    * Frames are untagged, with EtherType `0x88b5` (IEEE 802 local experimental). Switched traffic on the peer-link
      is always tagged, so the two can never be confused.
    * The session carries MAC synchronisation, port and bundle state, consistency checks, RSTP relay and peer hellos.
    * An ingress filter on the peer-link passes these frames to switchd and drops them before the bridge, so they
      never reach a VLAN or another port. Hardware offload is used where available.
    * Security: a small reliable stream over Ethernet (sequence numbers, acknowledgements, retransmission,
      fragmentation to the link MTU) carries **TLS 1.3 with mutual certificate authentication**, using the
      stack's own certificates. Frames from anything that is not an authenticated peer are ignored.
    * Hellos are also sent on each physical port of the peer-link, so a failed cable inside the peer-link bundle is
      detected per port.
  * Traffic that arrives over the peer-link is never sent out of an MC-LAG bundle that is up on the receiving member
    (split horizon), because the peer already delivered it on its own leg. When a member's leg of a bundle fails,
    this filter is lifted for that bundle, and the peer's traffic reaches the device via the peer-link.
* `system-mac <mac>` / `system-priority <n>`: the shared LACP system id presented by both members on MC-LAG bundles.
  If `system-mac` is unset, a stable locally administered MAC is derived from the stack id and domain id. It never
  changes, because a change would make partners re-negotiate. Default priority 32768.
* `keepalive { interval <ms>; timeout <count>; }`: liveness check between the two members over the **management
  network**, independent of the peer-link. Defaults: 1000 ms and 3 missed keepalives.
  W: a member without a management address (split-brain detection is impossible).
* `delay-restore <s>`: after a member boots or rejoins, its MC-LAG ports stay out of the bundle for this long
  (default 300 s). During that time the MAC table is synchronised and RSTP converges before traffic is attracted.
* `anycast-vtep <ip>`: VXLAN source address shared by the pair. Remote VTEPs send traffic for devices behind MC-LAG
  bundles to this one address, and either member can receive it (5.7).

**Behaviour:**

* **MAC synchronisation**
  * A MAC learned on an MC-LAG bundle is installed on the peer on the same bundle.
  * A MAC ages out only when it has aged out on **both** members.
  * MACs learned on single-homed ports are installed on the peer pointing to the peer-link.
* **Failure handling**
  | Situation | Behaviour |
  |---|---|
  | A member's leg of an MC-LAG bundle fails | The bundle continues on the other member. The receiving member lifts split horizon for that bundle, and MACs point to the peer-link. |
  | Peer-link down (all ports), keepalive up | The **secondary** takes its MC-LAG ports out of the bundles (LACP out-of-sync) to avoid split-brain. The primary carries all traffic. |
  | Peer-link down, keepalive down (peer dead) | The survivor carries all traffic, as the primary. |
  | Member returns | `delay-restore` applies, then its legs rejoin. |
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
  * The tunnel accepts frames up to the largest `mtu` of the VXLAN VLANs (default 1500).
  * The underlay needs 50 more bytes (IPv4) or 70 (IPv6), plus 80 with `encryption`. W: underlay MTU too small.
  * Frames that do not fit are dropped at the tunnel and counted.
* **MC-LAG with `anycast-vtep`**: traffic of devices behind MC-LAG bundles is sent from the anycast address, and
  both members accept traffic to it. Single-homed devices use the member's own `vtep-address`.

### 5.8 forwarding-options

#### `forwarding-options analyzer <name> { input { … }; output { interface <if>; } }`
Port mirroring. Copies of the selected traffic are sent out of the output interface. Mirroring never affects
the original traffic. If the output port is congested, only mirrored copies are dropped.
* `input ingress interface [ <if> … ]`: frames **received** on these interfaces, as received (including their VLAN tag).
* `input egress interface [ <if> … ]`: frames **sent** on these interfaces, as sent.
* `input ingress vlan [ <vlan> … ]`: frames received in these VLANs, on any port of the output port's member.
* `output interface <if>`: destination. A dedicated plain port (without `family ethernet-switching`) is recommended.
  Mirrored frames are sent unmodified. An `ae` output spreads copies by its hash policy.
* Mirroring an `ae` mirrors all its member ports on that member.
* Several analyzers may share an output port, and one interface may be an input of several analyzers.
* Implemented with tc (`matchall`/`flower` + `mirred`), offloaded to hardware where supported.
* Commit check:
  * E: no output.
  * E: no input.
  * E: the output is also an input.
  * E: inputs and output on different members (mirroring across the stack is not supported).
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
            interface eno1;
            address 192.168.1.20/24;
            gateway 192.168.1.1;
        }
    }
}
interfaces {
    1/enp1s0 {
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
    1/enp1s1 {
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
        mtu 9000;
    }
    users {
        vlan-id 10;
    }
}
protocols {
    rstp {
        interface 1/enp1s0 {
            edge;
        }
    }
    layer2-control {
        bpdu-block {
            interface 1/enp1s0;
        }
    }
}
```

### 7.2 MC-LAG pair with a dual-homed server, mirroring and VXLAN (set format)

```
set stack member 1 host-name sw-a
set stack member 1 management interface eno1
set stack member 1 management address 192.168.1.11/24
set stack member 1 vtep-address 10.255.0.1
set stack member 1 underlay interface enp5s0
set stack member 1 underlay address 10.99.0.1/24
set stack member 2 host-name sw-b
set stack member 2 management interface eno1
set stack member 2 management address 192.168.1.12/24
set stack member 2 vtep-address 10.255.0.2
set stack member 2 underlay interface enp5s0
set stack member 2 underlay address 10.99.0.2/24
set interfaces 1/enp5s0 mtu 9216
set interfaces 2/enp5s0 mtu 9216
set interfaces 1/enp2s0 ether-options 802.3ad ae0
set interfaces 1/enp2s1 ether-options 802.3ad ae0
set interfaces 2/enp2s0 ether-options 802.3ad ae0
set interfaces 2/enp2s1 ether-options 802.3ad ae0
set interfaces ae0 description peer-link
set interfaces ae0 mtu 9216
set interfaces ae0 aggregated-ether-options lacp active
set interfaces 1/enp1s0 ether-options 802.3ad ae1
set interfaces 2/enp1s0 ether-options 802.3ad ae1
set interfaces ae1 description "server A (dual-homed)"
set interfaces ae1 mtu 9000
set interfaces ae1 aggregated-ether-options lacp active
set interfaces ae1 aggregated-ether-options mclag
set interfaces ae1 native-vlan-id users
set interfaces ae1 unit 0 family ethernet-switching interface-mode trunk
set interfaces ae1 unit 0 family ethernet-switching vlan members storage
set interfaces 1/enp3s0 description "mirror to analyzer laptop"
set vlans users vlan-id 10
set vlans users vxlan vni 10010
set vlans storage vlan-id 20
set vlans storage mtu 9000
set mclag domain 1 members [ 1 2 ]
set mclag domain 1 peer-link ae0
set protocols rstp
set forwarding-options analyzer debug input ingress interface ae1
set forwarding-options analyzer debug input egress interface ae1
set forwarding-options analyzer debug output interface 1/enp3s0
```

---

## 8. Implementation status

| Area | Status |
|---|---|
| Schema, formats (hierarchical, set, JSON), diff | implemented, tested and fuzzed |
| Commit check (the validation rules in this document) | implemented and tested, except where noted below |
| `inactive:` / `activate` / `deactivate`, `replace:`/`delete:` tags, `load`, `save`, `copy`, `rename` | specified, next (CLI phase) |
| Commit / confirmation / rollback engine, CLI | next |
| Operator permission check at commit, OS account conflicts, cert/key pairing, time-zone check | with the respective subsystems |
| Data plane, services, stack, LACP, MC-LAG, RSTP, VXLAN | later phases (see PLAN.md §8) |

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
| `system services ssh` | container |  |  | SSH access to the CLI |
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
| `stack member <member-id>` | list | &lt;member-id&gt; 1..16 |  | Stack member |
| `stack member <member-id> host-name` | leaf | &lt;hostname&gt; |  | Host name of this member |
| `stack member <member-id> priority` | leaf | &lt;priority&gt; 0..255 | 128 | Priority for leader election (higher wins) |
| `stack member <member-id> role` | leaf | switch \\| witness | switch | Member role |
| `stack member <member-id> management` | container |  |  | Out-of-band management interface of this member |
| `stack member <member-id> management interface` | leaf | &lt;linux-interface&gt; |  | Linux interface used for management (kept outside the switch bridge) |
| `stack member <member-id> management address` | leaf (excl. mgmt-addr) | &lt;address/prefix&gt; |  | Static management address |
| `stack member <member-id> management dhcp` | flag (excl. mgmt-addr) |  |  | Obtain management address via DHCP |
| `stack member <member-id> management gateway` | leaf | &lt;ip-address&gt; |  | Default gateway in the management VRF |
| `stack member <member-id> management vlan` | leaf | &lt;vlan-id&gt; 1..4094 |  | Use an in-band VLAN instead of a dedicated interface |
| `stack member <member-id> vtep-address` | leaf | &lt;ip-address&gt; |  | Local VXLAN tunnel endpoint address |
| `stack member <member-id> underlay` | container |  |  | Layer 3 interface used for VXLAN transport |
| `stack member <member-id> underlay interface` | leaf | &lt;linux-interface&gt; |  | Linux interface |
| `stack member <member-id> underlay address` | leaf | &lt;address/prefix&gt; |  | Underlay address |
| `stack member <member-id> underlay gateway` | leaf | &lt;ip-address&gt; |  | Underlay gateway |
| `interfaces <interface-name>` | list | &lt;interface-name&gt; |  | Interface configuration |
| `interfaces <interface-name> description` | leaf | &lt;text&gt; |  | Interface description |
| `interfaces <interface-name> disable` | flag |  |  | Administratively disable the interface |
| `interfaces <interface-name> mtu` | leaf | &lt;mtu&gt; 256..16000 | 1500 | Maximum frame payload size (jumbo frames up to 16000) |
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
| `interfaces <interface-name> unit <unit>` | list | &lt;unit&gt; 0..0 |  | Logical unit |
| `interfaces <interface-name> unit <unit> description` | leaf | &lt;text&gt; |  | Unit description |
| `interfaces <interface-name> unit <unit> family` | container |  |  | Protocol family |
| `interfaces <interface-name> unit <unit> family ethernet-switching` | presence |  |  | Layer 2 switching |
| `interfaces <interface-name> unit <unit> family ethernet-switching interface-mode` | leaf | access \\| trunk |  | Port mode |
| `interfaces <interface-name> unit <unit> family ethernet-switching vlan` | container |  |  | VLAN membership |
| `interfaces <interface-name> unit <unit> family ethernet-switching vlan members` | leaf-list | &lt;vlan&gt; |  | VLAN names or ids (ranges like 10-20 allowed) |
| `vlans <name>` | list | &lt;name&gt; |  | VLAN configuration |
| `vlans <name> vlan-id` | leaf | &lt;vlan-id&gt; 1..4094 |  | 802.1Q VLAN id |
| `vlans <name> description` | leaf | &lt;text&gt; |  | VLAN description |
| `vlans <name> mtu` | leaf | &lt;mtu&gt; 256..16000 |  | Maximum frame payload size within this VLAN |
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
| `mclag domain <domain-id> keepalive` | container |  |  | Peer liveness detection over the management network |
| `mclag domain <domain-id> keepalive interval` | leaf | &lt;ms&gt; 100..10000 | 1000 | Milliseconds between keepalives |
| `mclag domain <domain-id> keepalive timeout` | leaf | &lt;count&gt; 2..30 | 3 | Missed keepalives before the peer is declared dead |
| `mclag domain <domain-id> delay-restore` | leaf | &lt;seconds&gt; 0..3600 | 300 | Seconds to wait after reboot before enabling MC-LAG ports |
| `switch-options` | container |  |  | Global switching options |
| `switch-options mac-table-aging-time` | leaf | &lt;seconds&gt; 10..1000000 | 300 | MAC table aging time in seconds |
| `switch-options vxlan` | container |  |  | VXLAN transport |
| `switch-options vxlan mode` | leaf | control-plane \\| flood-and-learn | control-plane | How remote MACs are learned |
| `switch-options vxlan udp-port` | leaf | &lt;port&gt; 1..65535 | 4789 | VXLAN UDP destination port |
| `switch-options vxlan remote-vtep <ip-address>` | list | &lt;ip-address&gt; |  | Static VTEP outside the stack |
| `switch-options vxlan remote-vtep <ip-address> vni` | leaf-list | &lt;vni&gt; 1..16777214 |  | VNIs to extend to this VTEP |
| `switch-options vxlan encryption` | flag |  |  | Encrypt VXLAN underlay traffic with WireGuard |
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
