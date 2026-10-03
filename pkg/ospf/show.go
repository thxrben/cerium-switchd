package ospf

import (
	"net/netip"
	"time"
)

// Snapshots for the show commands (reference 5.13).

// IfaceStatus is show ospf interface.
type IfaceStatus struct {
	Name      string        `json:"name"`
	Area      ID            `json:"area"`
	State     string        `json:"state"`
	DR        ID            `json:"dr"`
	BDR       ID            `json:"bdr"`
	DRs       string        `json:"dr_bdr,omitempty"` // as shown: DR/BDR
	Neighbors int           `json:"neighbors"`
	Adjacent  int           `json:"adjacent"`
	Cost      uint16        `json:"cost"`
	Hello     uint16        `json:"hello"`
	Dead      uint32        `json:"dead"`
	P2P       bool          `json:"p2p,omitempty"`
	Passive   bool          `json:"passive,omitempty"`
	Addr      netip.Addr    `json:"addr"`
	Since     time.Duration `json:"since"`
}

// NeighborStatus is show ospf neighbor.
type NeighborStatus struct {
	ID       ID            `json:"id"`
	Addr     netip.Addr    `json:"addr"`
	Iface    string        `json:"iface"`
	Area     ID            `json:"area"`
	State    string        `json:"state"`
	Priority uint8         `json:"priority"`
	DeadIn   time.Duration `json:"dead_in"`
	DR       ID            `json:"dr"`
	BDR      ID            `json:"bdr"`
	Up       time.Duration `json:"up"` // time in the current state
	Options  uint32        `json:"options"`
	Events   uint64        `json:"events"`
	Retrans  int           `json:"retrans"`
	Requests int           `json:"requests"`
}

// LSAStatus is an entry of show ospf database.
type LSAStatus struct {
	Area     ID     `json:"area"`  // area scope; link scope: the area too
	Scope    string `json:"scope"` // link, area, as
	Iface    string `json:"iface,omitempty"`
	Type     LSType `json:"type"`
	TypeName string `json:"type_name"`
	ID       ID     `json:"id"`
	AdvRtr   ID     `json:"adv_rtr"`
	Seq      uint32 `json:"seq"`
	Age      uint16 `json:"age"`
	Checksum uint16 `json:"checksum"`
	Length   uint16 `json:"length"`
	Self     bool   `json:"self"`
	LSA      *LSA   `json:"lsa,omitempty"` // the parsed body (detail)
}

// Overview is show ospf overview.
type Overview struct {
	Version     Version       `json:"version"`
	RouterID    ID            `json:"router_id"`
	ABR, ASBR   bool          `json:"-"`
	Roles       []string      `json:"roles,omitempty"`
	Areas       []ID          `json:"areas"`
	Overloaded  bool          `json:"overloaded"`
	SPFRuns     uint64        `json:"spf_runs"`
	LastSPF     time.Duration `json:"last_spf"` // ago
	SPFDuration time.Duration `json:"spf_duration"`
	LSAs        int           `json:"lsas"`
	Externals   int           `json:"externals"`
	Uptime      time.Duration `json:"uptime"`
}

// Status is everything show ospf … needs of one router.
type Status struct {
	Overview   Overview         `json:"overview"`
	Interfaces []IfaceStatus    `json:"interfaces"`
	Neighbors  []NeighborStatus `json:"neighbors"`
	Database   []LSAStatus      `json:"database"`
	Routes     []Route          `json:"routes"`
	Stats      Stats            `json:"stats"`
}

// Status returns the router's state (detail: with the LSA bodies).
func (r *Router) Status(detail bool) Status {
	now := r.now
	var s Status
	o := &s.Overview
	o.Version, o.RouterID, o.ABR, o.ASBR, o.Overloaded = r.v, r.rid, r.isABR(), r.isASBR(), r.cfg.Overload
	if o.ABR {
		o.Roles = append(o.Roles, "Area Border Router")
	}
	if o.ASBR {
		o.Roles = append(o.Roles, "AS Boundary Router")
	}
	o.SPFRuns, o.SPFDuration = r.Stats.SPFRuns, r.Stats.SPFDuration
	if !r.Stats.LastSPF.IsZero() {
		o.LastSPF = now.Sub(r.Stats.LastSPF)
	}
	o.Uptime = now.Sub(r.started)
	for _, a := range r.sortedAreas() {
		o.Areas = append(o.Areas, a.id)
		for _, i := range sortedIfs(a) {
			is := IfaceStatus{Name: i.cfg.Name, Area: a.id, State: i.state.String(), DR: i.dr, BDR: i.bdr, Neighbors: len(i.nbrs),
				Adjacent: len(i.fullNbrs()), Cost: i.cfg.Cost, Hello: i.cfg.Hello, Dead: i.cfg.Dead, P2P: i.cfg.P2P,
				Passive: i.cfg.Passive, Addr: i.cfg.Addr, Since: now.Sub(i.since)}
			if !i.cfg.P2P && i.state != IfPassive && i.state != IfDown {
				is.DRs = i.dr.String() + "/" + i.bdr.String()
			}
			s.Interfaces = append(s.Interfaces, is)
			for _, n := range i.sortedNbrs() {
				s.Neighbors = append(s.Neighbors, NeighborStatus{ID: n.id, Addr: n.addr, Iface: i.cfg.Name, Area: a.id,
					State: n.state.String(), Priority: n.prio, DeadIn: max(n.inactAt.Sub(now), 0), DR: n.dr, BDR: n.bdr,
					Up: now.Sub(n.since), Options: n.options, Events: n.Events, Retrans: len(n.retrans), Requests: len(n.requests)})
			}
		}
	}
	add := func(sc *scope, area ID, scopeName, iface string) {
		for _, l := range sc.db.All(now) {
			e := LSAStatus{Area: area, Scope: scopeName, Iface: iface, Type: l.Type, TypeName: r.v.TypeName(l.Type), ID: l.ID,
				AdvRtr: l.AdvRtr, Seq: uint32(l.Seq), Age: l.Age, Checksum: l.Checksum, Length: l.Length, Self: l.AdvRtr == r.rid}
			if detail {
				e.LSA = l
			}
			s.Database = append(s.Database, e)
			o.LSAs++
		}
	}
	for _, a := range r.sortedAreas() {
		add(a.sc, a.id, "area", "")
		for _, i := range sortedIfs(a) {
			add(i.sc, a.id, "link", i.cfg.Name)
		}
	}
	add(r.as, 0, "as", "")
	o.Externals = r.as.db.Len()
	s.Routes = r.Routes()
	s.Stats = r.Stats
	return s
}

// ClearNeighbors restarts the adjacencies (all, or the neighbour with
// router id or address nbr).
func (r *Router) ClearNeighbors(nbr netip.Addr, now time.Time) int {
	r.now = now
	defer r.settle()
	n := 0
	for _, i := range r.sortedIfaces() {
		for _, nb := range i.sortedNbrs() {
			if nbr.IsValid() && nb.addr != nbr && nb.id.Addr() != nbr {
				continue
			}
			nb.kill()
			delete(i.nbrs, i.nbrKey(nb.addr, nb.id))
			n++
		}
	}
	return n
}
