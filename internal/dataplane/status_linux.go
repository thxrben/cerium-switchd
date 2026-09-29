//go:build linux

package dataplane

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Status returns the operational state of all links except loopback.
func (k *Netlink) Status() ([]PortStatus, error) {
	st, err := k.Read()
	if err != nil {
		return nil, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	var out []PortStatus
	for _, l := range links {
		a := l.Attrs()
		sl := st.Links[a.Name]
		if sl == nil {
			continue
		}
		p := PortStatus{
			Name: a.Name, Kind: sl.Kind, AdminUp: sl.Up, OperUp: a.OperState == netlink.OperUp || a.RawFlags&unix.IFF_RUNNING != 0,
			MTU: a.MTU, Alias: a.Alias, Master: sl.Master, VLANs: sl.VLANs, MAC: a.HardwareAddr.String(),
			DropTagged: sl.DropTagged,
		}
		if raw, err := os.ReadFile(filepath.Join(k.sysRoot(), "class", "net", a.Name, "speed")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && v > 0 {
				p.SpeedMbps = v
			}
		}
		if s := a.Statistics; s != nil {
			p.Counters = Counters{s.RxPackets, s.TxPackets, s.RxBytes, s.TxBytes, s.RxErrors, s.TxErrors, s.RxDropped, s.TxDropped, s.Multicast}
		}
		if sl.DropTagged {
			if fs, err := netlink.FilterList(l, netlink.HANDLE_MIN_INGRESS); err == nil {
				for _, f := range fs {
					if f.Attrs().Priority == prioDropTagged && f.Attrs().Chain != nil && *f.Attrs().Chain == chainTagged {
						if m, ok := f.(*netlink.MatchAll); ok {
							for _, act := range m.Actions {
								if st := act.Attrs().Statistics; st != nil {
									p.TaggedDrops += uint64(st.Basic.Packets)
								}
							}
						}
					}
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// FDB returns the MAC table of the switch bridge (learned and static
// entries; the ports' own addresses are left out).
func (k *Netlink) FDB() ([]FDBEntry, error) {
	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return nil, nil // no bridge yet: empty table
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	ns, err := netlink.NeighList(0, unix.AF_BRIDGE)
	if err != nil {
		return nil, err
	}
	hz := 100
	var out []FDBEntry
	for _, n := range ns {
		if n.MasterIndex != br.Attrs().Index || n.LinkIndex == br.Attrs().Index || n.Vlan == 0 {
			continue
		}
		local := n.State&netlink.NUD_PERMANENT != 0
		if local {
			continue // the port's own address
		}
		out = append(out, FDBEntry{
			MAC: n.HardwareAddr.String(), VLAN: n.Vlan, Port: names[n.LinkIndex],
			Static:     n.State&netlink.NUD_NOARP != 0,
			AgeSeconds: int(n.Updated) / hz,
		})
	}
	return out, nil
}

// FlushFDB removes learned entries, optionally only of one VLAN or port.
// It returns the number removed.
func (k *Netlink) FlushFDB(vlan int, port string) (int, error) {
	entries, err := k.FDB()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.Static || (vlan != 0 && e.VLAN != vlan) || (port != "" && e.Port != port) {
			continue
		}
		l, err := netlink.LinkByName(e.Port)
		if err != nil {
			continue
		}
		mac, _ := parseMAC(e.MAC)
		err = netlink.NeighDel(&netlink.Neigh{
			LinkIndex: l.Attrs().Index, Family: unix.AF_BRIDGE, HardwareAddr: mac, Vlan: e.VLAN,
			Flags: netlink.NTF_MASTER,
		})
		if err == nil {
			n++
		}
	}
	return n, nil
}
