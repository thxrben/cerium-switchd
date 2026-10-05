package memslots

// Facts are the configuration's bounds of the tables in the system area
// (reference 5.1).
type Facts struct {
	Interfaces   int // physical ports, ae, units, irb units
	VLANs        int
	VNIs         int
	Members      int
	StaticRoutes int
	DHCPLeases   int // the size of the configured pools
	MACsecPorts  int
	BFDSessions  int
	Ports        int    // physical ports (LLDP neighbours, RSTP, LACP)
	ConfigBytes  uint64 // the active configuration's text
}

// Bytes of the system area's tables per item.
const (
	perInterface   = 8 << 10  // kernel device, statistics, our state
	perVLAN        = 2 << 10  // bridge VLAN entries and our state
	perVNI         = 8 << 10  // a VXLAN device
	perMember      = 64 << 10 // keys, stacking ports, tunnel neighbours, status
	perStaticRoute = 700
	perLease       = 1 << 10
	perMACsec      = 16 << 10 // macsec device, SAs, keys
	perBFD         = 4 << 10
	perPort        = 8 * 2 << 10 // 8 LLDP neighbours, RSTP and LACP state
	raftLog        = 16 << 20    // log until a snapshot, and the snapshot
	alarms         = 64 << 10
	rollbacks      = 50 + 2 // stored rollbacks, the candidate and the active one
)

// SystemArea is the bytes the configuration-bounded tables need.
func SystemArea(f Facts) uint64 {
	n := uint64(f.Interfaces)*perInterface + uint64(f.VLANs)*perVLAN + uint64(f.VNIs)*perVNI +
		uint64(max(f.Members, 1))*perMember + uint64(f.StaticRoutes)*perStaticRoute +
		uint64(f.DHCPLeases)*perLease + uint64(f.MACsecPorts)*perMACsec + uint64(f.BFDSessions)*perBFD +
		uint64(f.Ports)*perPort + raftLog + alarms
	// The configuration as text and as parsed tree (about 8 times the text)
	// for every copy kept.
	n += f.ConfigBytes * 9 * rollbacks
	return n
}

// DaemonBase is the base memory of each program (no tables): measured on
// the lab for this release, rounded up.
var DaemonBase = map[string]uint64{
	"switchd":        64 << 20,
	"switchd-update": 16 << 20,
	"cer-lacpd":      16 << 20,
	"cer-mclagd":     24 << 20,
	"cer-rstpd":      16 << 20,
	"cer-lldpd":      16 << 20,
	"cer-syslogd":    24 << 20,
	"cer-ntpd":       16 << 20,
	"cer-dhcpcd":     16 << 20,
	"cer-ribd":       32 << 20, // + one piece of BGP changes being decoded (PLAN 15b)
	"cer-bfdd":       16 << 20,
	"cer-ospfd":      24 << 20,
	"cer-bgpd":       32 << 20, // + one piece of changes being sent (4096 prefixes)
}
