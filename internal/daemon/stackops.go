package daemon

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"mclag/internal/cli"
	"mclag/internal/lacp"
)

// stackOps is the operational data of the whole stack: the members work as
// one switch, so listings of interfaces (and of what is learned on them)
// collect every member's rows over the stacking protocol (reference 3.5).
// Everything else is this member's (ops).
type stackOps struct {
	*ops
	ctl *stackCtl
}

// opsTimeout bounds how long a listing waits for another member.
const opsTimeout = 5 * time.Second

type opsRequest struct {
	Method string `json:"method"`
	IPv6   bool   `json:"ipv6,omitempty"`
	VLAN   int    `json:"vlan,omitempty"`
	Iface  string `json:"iface,omitempty"`
}

// serveOps answers other members' listing requests from this member's data.
func (s *stackCtl) serveOps(o *ops) {
	s.node.Handle("ops", func(_ int, raw json.RawMessage) (any, error) {
		var r opsRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return localOps(o, r)
	})
}

func localOps(o *ops, r opsRequest) (any, error) {
	switch r.Method {
	case "interfaces":
		return o.Interfaces()
	case "hardware":
		return o.Hardware()
	case "cards":
		return o.Cards()
	case "mac":
		return o.MACTable()
	case "clear-mac":
		return o.ClearMACTable(r.VLAN, r.Iface)
	case "neighbors":
		return o.Neighbors(r.IPv6)
	case "offload":
		return o.Offload()
	case "lacp":
		return o.LACP()
	case "lldp":
		return o.LLDP()
	case "dhcp":
		return o.DHCPBindings()
	case "vlan-drops":
		return o.VLANMTUDrops()
	case "vc":
		return o.VirtualChassis()
	case "mclag":
		return o.MCLAG()
	case "multicast":
		return o.Multicast()
	case "stack-mtu":
		return o.StackMTU()
	}
	return nil, fmt.Errorf("unknown listing %q", r.Method)
}

// each runs r on every member that is present (this one locally) and
// returns the answers by member; a present member that did not answer is
// reported as a *cli.PartialError. Members that are not there (not joined,
// switched off, cut off) have no interfaces to list.
func each[T any](s *stackOps, r opsRequest) (map[int]T, error) {
	ids := []int{s.member}
	if s.ctl != nil && s.vc != nil && s.vc.Mesh() != nil {
		members := s.ctl.node.Members()
		for _, id := range s.vc.Mesh().Reachable() {
			if _, listed := members[id]; listed && id != s.member {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	out := map[int]T{}
	var missing []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	var localErr error
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v T
			var err error
			if id == s.member {
				var a any
				if a, err = localOps(s.ops, r); err == nil {
					v = a.(T)
				}
			} else {
				var raw json.RawMessage
				if raw, err = s.ctl.node.Call(id, "ops", r, opsTimeout); err == nil {
					err = json.Unmarshal(raw, &v)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				out[id] = v
			case id == s.member:
				localErr = err
			default:
				missing = append(missing, id)
			}
		}()
	}
	wg.Wait()
	if localErr != nil {
		return out, localErr
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return out, &cli.PartialError{Members: missing}
	}
	return out, nil
}

// rows concatenates the members' lists in member order.
func rows[T any](by map[int][]T) []T {
	var out []T
	for _, id := range sortedIDs(by) {
		out = append(out, by[id]...)
	}
	return out
}

func sortedIDs[V any](m map[int]V) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *stackOps) Interfaces() ([]cli.IfStatus, error) {
	by, err := each[[]cli.IfStatus](s, opsRequest{Method: "interfaces"})
	// Units of the whole stack (irb, bundles) are listed by every member
	// that has them: once is enough, this member's first.
	// A bundle's parts on two members (MC-LAG) are one interface: up when
	// either part is, with the sum of both parts' speed and counters.
	at := map[string]int{}
	var out []cli.IfStatus
	for _, id := range append([]int{s.member}, sortedIDs(by)...) {
		for _, i := range by[id] {
			n, dup := at[i.Name]
			if !dup {
				at[i.Name] = len(out)
				out = append(out, i)
				continue
			}
			if len(i.Members) == 0 || len(out[n].Members) == 0 {
				continue // a unit of the whole stack (irb): once is enough
			}
			m := &out[n]
			m.OperUp = m.OperUp || i.OperUp
			m.SpeedMbps += i.SpeedMbps
			m.Members = append(m.Members, i.Members...)
			k, x := &m.Counters, i.Counters
			k.RxPackets, k.TxPackets, k.RxBytes, k.TxBytes = k.RxPackets+x.RxPackets, k.TxPackets+x.TxPackets, k.RxBytes+x.RxBytes, k.TxBytes+x.TxBytes
			k.RxErrors, k.TxErrors, k.RxDropped, k.TxDropped = k.RxErrors+x.RxErrors, k.TxErrors+x.TxErrors, k.RxDropped+x.RxDropped, k.TxDropped+x.TxDropped
			k.RxMulticast += x.RxMulticast
			m.TaggedDrops += i.TaggedDrops
		}
		delete(by, id)
	}
	return out, err
}

func (s *stackOps) Hardware() ([]cli.HardwarePort, error) {
	by, err := each[[]cli.HardwarePort](s, opsRequest{Method: "hardware"})
	return rows(by), err
}

func (s *stackOps) Cards() ([]cli.CardStatus, error) {
	by, err := each[[]cli.CardStatus](s, opsRequest{Method: "cards"})
	for id, cs := range by {
		for i := range cs {
			cs[i].Member = id
		}
	}
	return rows(by), err
}

// MACTable lists what the stack learned where it was learned: the copies
// the other members hold on their stack tunnels (vc-<n>) are left out,
// and an address on an MC-LAG bundle is listed once.
func (s *stackOps) MACTable() ([]cli.MACEntry, error) {
	by, err := each[[]cli.MACEntry](s, opsRequest{Method: "mac"})
	seen := map[string]bool{}
	var out []cli.MACEntry
	for _, e := range rows(by) {
		key := fmt.Sprintf("%d %s %s", e.VLAN, e.MAC, e.Interface)
		if strings.HasPrefix(e.Interface, "vc-") || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out, err
}

func (s *stackOps) ClearMACTable(vlan int, iface string) (int, error) {
	by, err := each[int](s, opsRequest{Method: "clear-mac", VLAN: vlan, Iface: iface})
	n := 0
	for _, c := range by {
		n += c
	}
	return n, err
}

// Neighbors lists the stack's neighbours; the hidden instance of the
// stack tunnels is internal.
func (s *stackOps) Neighbors(ipv6 bool) ([]cli.Neighbor, error) {
	by, err := each[[]cli.Neighbor](s, opsRequest{Method: "neighbors", IPv6: ipv6})
	seen := map[string]bool{}
	var out []cli.Neighbor
	for _, n := range rows(by) {
		key := n.IP + " " + n.MAC + " " + n.Interface + " " + n.Instance
		if n.Instance == "swstack" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out, err
}

func (s *stackOps) Offload() ([]cli.OffloadPort, error) {
	by, err := each[[]cli.OffloadPort](s, opsRequest{Method: "offload"})
	return rows(by), err
}

// LLDP merges every member's ports, counters and neighbours (the system
// part is the same everywhere: this member's).
func (s *stackOps) LLDP() (cli.LLDPStatus, error) {
	by, err := each[cli.LLDPStatus](s, opsRequest{Method: "lldp"})
	st := by[s.member]
	st.Ports, st.Stats, st.Neighbors = nil, nil, nil
	for _, id := range sortedIDs(by) {
		m := by[id]
		st.Running = st.Running || m.Running
		st.Ports = append(st.Ports, m.Ports...)
		st.Stats = append(st.Stats, m.Stats...)
		st.Neighbors = append(st.Neighbors, m.Neighbors...)
	}
	return st, err
}

// LACP merges a bundle's ports on every member (MC-LAG bundles span two).
func (s *stackOps) LACP() ([]lacp.BundleStatus, error) {
	by, err := each[[]lacp.BundleStatus](s, opsRequest{Method: "lacp"})
	var out []lacp.BundleStatus
	at := map[string]int{}
	for _, b := range rows(by) {
		i, ok := at[b.Name]
		if !ok {
			at[b.Name] = len(out)
			i = len(out)
			out = append(out, lacp.BundleStatus{Name: b.Name, PortNames: map[string]string{}})
		}
		// Kernel names repeat across members: key every port by its
		// configuration name.
		m := &out[i]
		for _, p := range b.Ports {
			if name := b.PortNames[p.Name]; name != "" {
				p.Name = name
			}
			m.PortNames[p.Name] = p.Name
			m.Ports = append(m.Ports, p)
		}
	}
	return out, err
}

// Multicast collects every member's snooping state.
func (s *stackOps) Multicast() ([]cli.McastStatus, error) {
	by, err := each[[]cli.McastStatus](s, opsRequest{Method: "multicast"})
	return rows(by), err
}

// MCLAG collects every member's view of its MC-LAG pair.
func (s *stackOps) MCLAG() ([]cli.MCLAGStatus, error) {
	by, err := each[[]cli.MCLAGStatus](s, opsRequest{Method: "mclag"})
	return rows(by), err
}

func (s *stackOps) DHCPBindings() ([]cli.DHCPBinding, error) {
	by, err := each[[]cli.DHCPBinding](s, opsRequest{Method: "dhcp"})
	return rows(by), err
}

func (s *stackOps) VLANMTUDrops() (map[int]uint64, error) {
	by, err := each[map[int]uint64](s, opsRequest{Method: "vlan-drops"})
	out := map[int]uint64{}
	for _, d := range by {
		for v, n := range d {
			out[v] += n
		}
	}
	return out, err
}

// VirtualChassis is this member's view of the stack with every member's VC
// ports.
func (s *stackOps) VirtualChassis() (cli.VCStatus, error) {
	by, err := each[cli.VCStatus](s, opsRequest{Method: "vc"})
	st, ok := by[s.member]
	if !ok {
		return st, err
	}
	st.Ports = nil
	for _, id := range sortedIDs(by) {
		st.Ports = append(st.Ports, by[id].Ports...)
	}
	return st, err
}

// StackMTU is the stack's MTU with every member's stacking ports.
func (s *stackOps) StackMTU() (cli.StackMTUStatus, error) {
	by, err := each[cli.StackMTUStatus](s, opsRequest{Method: "stack-mtu"})
	st, ok := by[s.member]
	if !ok {
		return st, err
	}
	st.Ports = nil
	for _, id := range sortedIDs(by) {
		st.Ports = append(st.Ports, by[id].Ports...)
	}
	return st, err
}
