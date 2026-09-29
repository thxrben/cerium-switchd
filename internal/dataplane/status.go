package dataplane

import "net"

// PortStatus is the operational state of a link.
type PortStatus struct {
	Name        string
	Kind        Kind
	AdminUp     bool
	OperUp      bool // carrier
	MTU         int  // kernel MTU
	SpeedMbps   int  // 0 unknown
	Alias       string
	Master      string
	VLANs       map[uint16]VlanFlags
	MAC         string
	Counters    Counters
	DropTagged  bool
	TaggedDrops uint64 // frames dropped by the access-port filter
}

// Counters are interface statistics.
type Counters struct {
	RxPackets, TxPackets, RxBytes, TxBytes   uint64
	RxErrors, TxErrors, RxDropped, TxDropped uint64
	RxMulticast                              uint64
}

// FDBEntry is one entry of the bridge MAC table.
type FDBEntry struct {
	MAC    string
	VLAN   int
	Port   string
	Static bool
	// AgeSeconds since the entry was last updated (learned entries).
	AgeSeconds int
}

func parseMAC(s string) ([]byte, error) { return net.ParseMAC(s) }
