# Test lab (Proxmox)

Five Debian 13 VMs (PLAN.md §11). **MGMT** is the interface for SSH/Ansible and has internet access; it is the
only NIC configured by the installer. All other NICs are attached to one Proxmox bridge each, named after the
link (the role below). These bridges must pass LACP and BPDUs (OVS with `forward-bpdu=true`, PLAN.md §11); `underlay` is shared by sw1–sw3, every other link connects exactly two VMs.

Switch VMs: 4 vCPU, 2 GB RAM, disk 32 GB (sw1, sw2) / 16 GB (sw3).

## sw1 = switchd-dev-1 (10.5.176.95)

| Role | MAC |
|---|---|
| MGMT (10.5.176.95) | `bc:24:11:16:b5:23` |
| stk-12 | `bc:24:11:ca:3b:4f` |
| stk-13 | `bc:24:11:13:89:e1` |
| peer1 | `bc:24:11:c9:42:76` |
| peer2 | `bc:24:11:f1:45:c7` |
| srv1-a | `bc:24:11:c3:60:48` |
| underlay | `bc:24:11:71:e1:51` |
| loop-13 | `bc:24:11:fe:f0:9f` |

## sw2 = switchd-dev-2 (10.5.176.96)

| Role | MAC |
|---|---|
| MGMT (10.5.176.96) | `bc:24:11:15:69:b4` |
| stk-12 | `bc:24:11:45:4c:63` |
| stk-23 | `bc:24:11:0e:54:f5` |
| peer1 | `bc:24:11:6d:85:ee` |
| peer2 | `bc:24:11:21:45:b7` |
| srv1-b | `bc:24:11:af:3a:43` |
| underlay | `bc:24:11:ec:f0:43` |
| loop-23 | `bc:24:11:b4:ce:3a` |

## sw3 = switchd-dev-3 (10.5.176.97)

| Role | MAC |
|---|---|
| MGMT (10.5.176.97) | `bc:24:11:74:91:d4` |
| stk-13 | `bc:24:11:16:6c:67` |
| stk-23 | `bc:24:11:77:62:f8` |
| underlay | `bc:24:11:7f:53:18` |
| srv2 | `bc:24:11:a1:4c:04` |
| loop-13 | `bc:24:11:0b:5f:e7` |
| loop-23 | `bc:24:11:ac:2c:b3` |

## srv1 = srv-dev-1 (10.5.176.101)

| Role | MAC |
|---|---|
| MGMT (10.5.176.101) | `bc:24:11:ab:08:3a` |
| srv1-a | `bc:24:11:c8:cd:f2` |
| srv1-b | `bc:24:11:33:47:ed` |

## srv2 = srv-dev-2 (10.5.176.102)

| Role | MAC |
|---|---|
| MGMT (10.5.176.102) | `bc:24:11:64:6a:ce` |
| srv2 | `bc:24:11:d5:40:52` |

## Links

| Link | Ends |
|---|---|
| stk-12 | sw1, sw2 |
| stk-13 | sw1, sw3 |
| peer1 | sw1, sw2 |
| peer2 | sw1, sw2 |
| srv1-a | sw1, srv1 |
| underlay | sw1, sw2, sw3 |
| loop-13 | sw1, sw3 |
| stk-23 | sw2, sw3 |
| srv1-b | sw2, srv1 |
| loop-23 | sw2, sw3 |
| srv2 | sw3, srv2 |

Stacking ring: stk-12, stk-13, stk-23. MC-LAG peer-link: peer1 + peer2 (sw1–sw2). srv1 is dual-homed via
srv1-a (sw1) and srv1-b (sw2). loop-13 (sw1–sw3) and loop-23 (sw2–sw3) are data links that form a loop
together with the peer-link, for RSTP tests.

## Management network

10.5.0.0/16, gateway 10.5.0.1, DNS/DHCP server 10.5.150.1. The VMs use their addresses **statically**
(`static-ip.yml`, applied 2026-09-29; the DHCP pool should exclude or reserve them). IPv6 stays on SLAAC.

## Ports

- 22: the OS SSH server (root with the dev machine's key; Ansible). switchd never modifies it.
- 2222: switchd's CLI SSH server when `system services ssh port 2222` is configured (reachable through the
  Proxmox firewall). Other ports are filtered from outside the lab subnet; use a jump host
  (`ssh -J root@10.5.176.96 ...`) if needed.

## Usage

```
cd lab
# first run per VM (only 'user' + password, no sudo, no python):
ansible-playbook bootstrap.yml -u user -e ansible_become_method=su -e ansible_become_password=<root password>
# afterwards: ansible-playbook <play>.yml -u root
ansible-playbook static-ip.yml -u root   # DHCP -> static, with an automatic revert guard
make cross && ansible-playbook deploy.yml -u root   # deploy switchd to the switches
```

Kernel interface names are not fixed; playbooks and tests resolve NICs by MAC from `host_vars/<host>.yml` (`nics`).
