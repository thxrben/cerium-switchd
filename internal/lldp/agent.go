package lldp

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// System is what every port of the stack announces about the system.
type System struct {
	ChassisMAC     net.HardwareAddr // the same on every member
	Name, Desc     string           // chassis name, "cerOS <version>"
	Caps, Enabled  uint16
	Mgmt           []netip.Addr
	Interval, Hold int // seconds, multiplier
}

// PortSpec is one port LLDP runs on.
type PortSpec struct {
	Linux string // kernel name
	Name  string // interface name, e.g. "2/0/3" (the port ID)
	Desc  string // description ("": the name)
	PVID  uint16
	// Bundle: the ae the port belongs to ("": none); InBundle: it is
	// aggregated now; BundleIndex: the aggregated port id announced, N+1
	// for aeN (the same on every member: an MC-LAG is one bundle).
	Bundle      string
	InBundle    bool
	BundleIndex uint32
	MaxFrame    uint16
}

// Neighbor is one neighbour learned on a port.
type Neighbor struct {
	Port       string // interface name
	Chassis    string // chassis ID for display
	PortID     string
	PortDesc   string
	SysName    string
	SysDesc    string
	Caps       uint16
	Enabled    uint16
	Mgmt       []string
	PVID       uint16
	MaxFrame   uint16
	Expires    time.Time
	key        string
	FirstSeen  time.Time
	Aggregated bool
}

// Stats counts one port's LLDPDUs.
type Stats struct {
	Port                      string
	Sent, Received, Discarded uint64
	AgedOut                   uint64
}

// MaxNeighbors per port.
const MaxNeighbors = 8

// pdu builds the LLDPDU of a port (ttl 0: shutdown).
func (s *System) pdu(p PortSpec, ttl uint16) *PDU {
	desc := p.Desc
	if desc == "" {
		desc = p.Name
	}
	d := &PDU{ChassisSubtype: ChassisMAC, ChassisID: slices.Clone(s.ChassisMAC), PortSubtype: PortIfName, PortID: []byte(p.Name),
		TTL: ttl, PortDesc: desc, SysName: s.Name, SysDesc: s.Desc, Caps: s.Caps, Enabled: s.Enabled,
		MgmtAddrs: s.Mgmt, PVID: p.PVID, MaxFrame: p.MaxFrame}
	if p.Bundle != "" {
		d.Agg = &Agg{Capable: true, Enabled: p.InBundle, PortID: p.BundleIndex}
	}
	return d
}

// TTL returns the time to live the system announces.
func (s *System) TTL() uint16 {
	t := s.Interval * s.Hold
	if t > 65535 {
		t = 65535
	}
	return uint16(t)
}

// table holds the neighbours of the ports (not safe for concurrent use).
type table struct {
	byPort map[string][]*Neighbor // by interface name
}

// learn records d received on port at now; it reports whether the table
// changed and whether the frame was discarded (table full).
func (t *table) learn(port string, d *PDU, now time.Time) (changed, discarded bool) {
	if t.byPort == nil {
		t.byPort = map[string][]*Neighbor{}
	}
	key := string(d.ChassisID) + "\x00" + string(d.PortID)
	ns := t.byPort[port]
	i := slices.IndexFunc(ns, func(n *Neighbor) bool { return n.key == key })
	if d.TTL == 0 {
		if i >= 0 {
			t.byPort[port] = slices.Delete(ns, i, i+1)
			return true, false
		}
		return false, false
	}
	n := &Neighbor{Port: port, Chassis: IDString(d.ChassisSubtype, d.ChassisID, true), PortID: IDString(d.PortSubtype, d.PortID, false),
		PortDesc: d.PortDesc, SysName: d.SysName, SysDesc: d.SysDesc, Caps: d.Caps, Enabled: d.Enabled, PVID: d.PVID,
		MaxFrame: d.MaxFrame, Expires: now.Add(time.Duration(d.TTL) * time.Second), key: key, FirstSeen: now}
	if d.Agg != nil {
		n.Aggregated = d.Agg.Enabled
	}
	for _, a := range d.MgmtAddrs {
		n.Mgmt = append(n.Mgmt, a.String())
	}
	if i >= 0 {
		n.FirstSeen = ns[i].FirstSeen
		ns[i] = n
		return true, false
	}
	if len(ns) >= MaxNeighbors {
		return false, true
	}
	t.byPort[port] = append(ns, n)
	return true, false
}

// age removes expired neighbours and returns how many per port.
func (t *table) age(now time.Time) map[string]int {
	out := map[string]int{}
	for p, ns := range t.byPort {
		keep := ns[:0]
		for _, n := range ns {
			if now.Before(n.Expires) {
				keep = append(keep, n)
			} else {
				out[p]++
			}
		}
		if len(keep) == 0 {
			delete(t.byPort, p)
		} else {
			t.byPort[p] = keep
		}
	}
	return out
}

// drop forgets a port's neighbours (LLDP stopped there or the link went
// down).
func (t *table) drop(port string) { delete(t.byPort, port) }

// list returns every neighbour, sorted by port.
func (t *table) list() []Neighbor {
	var out []Neighbor
	for _, ns := range t.byPort {
		for _, n := range ns {
			out = append(out, *n)
		}
	}
	slices.SortFunc(out, func(a, b Neighbor) int {
		if c := strings.Compare(a.Port, b.Port); c != 0 {
			return c
		}
		return strings.Compare(a.Chassis+a.PortID, b.Chassis+b.PortID)
	})
	return out
}
