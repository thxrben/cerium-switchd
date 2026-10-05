package macsec

import (
	"encoding/hex"
	"errors"
	"slices"

	"github.com/vishvananda/netlink/nl"
)

// Device state as the kernel reports it over its generic netlink family
// "macsec" (include/uapi/linux/if_macsec.h). Reading it through netlink
// instead of `ip -j macsec show` keeps switchd independent of iproute2's
// output format.

// SAStatus is one secure association.
type SAStatus struct {
	AN     uint8  `json:"an"`
	Active bool   `json:"active"`
	PN     uint64 `json:"pn"` // next packet number (transmit) or lowest acceptable (receive)
}

// RxSCStatus is one receive secure channel (a peer's transmit channel).
type RxSCStatus struct {
	SCI    string     `json:"sci"` // 16 hex digits
	Active bool       `json:"active"`
	SAs    []SAStatus `json:"sas"`
}

// DevStatus is one MACsec device.
type DevStatus struct {
	Ifindex    int          `json:"ifindex"`
	SCI        string       `json:"sci"` // the transmit SCI, 16 hex digits
	EncodingSA uint8        `json:"encoding_sa"`
	Offload    string       `json:"offload"` // "off", "phy", "mac"
	TxSAs      []SAStatus   `json:"tx_sas"`
	RxSCs      []RxSCStatus `json:"rx_scs"`
	// Counters of the SecY, the transmit SC and every receive SC (summed),
	// named as in IEEE 802.1AE (OutPktsProtected, InPktsOK, ...).
	Counters map[string]uint64 `json:"counters"`
}

// StatusReader is implemented by kernels that can report their MACsec
// devices.
type StatusReader interface {
	Status() (map[int]DevStatus, error)
}

// Generic netlink API.
const (
	genlName    = "macsec"
	genlVersion = 1
	cmdGetTxSC  = 0

	attrIfindex   = 1
	attrSecY      = 4
	attrTxSAList  = 5
	attrRxSCList  = 6
	attrTxSCStats = 7
	attrSecYStats = 8
	attrOffload   = 9

	secyAttrSCI        = 1
	secyAttrEncodingSA = 2

	rxscAttrSCI    = 1
	rxscAttrActive = 2
	rxscAttrSAList = 3
	rxscAttrStats  = 4

	saAttrAN     = 1
	saAttrActive = 2
	saAttrPN     = 3

	offloadAttrType = 1
)

// Counter names per statistics attribute.
var (
	txscStats = map[uint16]string{1: "OutPktsProtected", 2: "OutPktsEncrypted", 3: "OutOctetsProtected", 4: "OutOctetsEncrypted"}
	secyStats = map[uint16]string{1: "OutPktsUntagged", 2: "InPktsUntagged", 3: "OutPktsTooLong", 4: "InPktsNoTag",
		5: "InPktsBadTag", 6: "InPktsUnknownSCI", 7: "InPktsNoSCI", 8: "InPktsOverrun"}
	rxscStats = map[uint16]string{1: "InOctetsValidated", 2: "InOctetsDecrypted", 3: "InPktsUnchecked", 4: "InPktsDelayed",
		5: "InPktsOK", 6: "InPktsInvalid", 7: "InPktsLate", 8: "InPktsNotValid", 9: "InPktsNoSA", 10: "InPktsUnusedSA"}
	offloadNames = map[uint8]string{0: "off", 1: "phy", 2: "mac"}
)

// attrs parses a run of netlink attributes by type (the nested flag
// masked off).
func attrs(b []byte) (map[uint16][]byte, error) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, err
	}
	m := make(map[uint16][]byte, len(as))
	for _, a := range as {
		m[a.Attr.Type&nl.NLA_TYPE_MASK] = a.Value
	}
	return m, nil
}

// list parses the entries of a nested list attribute (each entry is
// itself nested; their types are indices).
func list(b []byte) ([][]byte, error) {
	as, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(as))
	for _, a := range as {
		out = append(out, a.Value)
	}
	return out, nil
}

// num reads an unsigned attribute of 1, 2, 4 or 8 bytes (host order).
func num(b []byte) uint64 {
	switch len(b) {
	case 1:
		return uint64(b[0])
	case 2:
		return uint64(nl.NativeEndian().Uint16(b))
	case 4:
		return uint64(nl.NativeEndian().Uint32(b))
	case 8:
		return nl.NativeEndian().Uint64(b)
	}
	return 0
}

// sci formats an SCI: the kernel keeps it in network order (MAC address,
// then port), so its bytes are printed as they are.
func sci(b []byte) string {
	if len(b) != 8 {
		return ""
	}
	return hex.EncodeToString(b)
}

// stats adds a statistics attribute's counters to c.
func stats(c map[string]uint64, b []byte, names map[uint16]string) error {
	m, err := attrs(b)
	if err != nil {
		return err
	}
	for t, v := range m {
		if n, ok := names[t]; ok {
			c[n] += num(v)
		}
	}
	return nil
}

func parseSA(b []byte) (SAStatus, error) {
	m, err := attrs(b)
	if err != nil {
		return SAStatus{}, err
	}
	return SAStatus{AN: uint8(num(m[saAttrAN])), Active: num(m[saAttrActive]) != 0, PN: num(m[saAttrPN])}, nil
}

func parseSAs(b []byte) ([]SAStatus, error) {
	entries, err := list(b)
	if err != nil {
		return nil, err
	}
	var out []SAStatus
	for _, e := range entries {
		sa, err := parseSA(e)
		if err != nil {
			return nil, err
		}
		out = append(out, sa)
	}
	slices.SortFunc(out, func(a, b SAStatus) int { return int(a.AN) - int(b.AN) })
	return out, nil
}

// parseDevice decodes one MACSEC_CMD_GET_TXSC reply (the attributes after
// the generic netlink header).
func parseDevice(b []byte) (DevStatus, error) {
	m, err := attrs(b)
	if err != nil {
		return DevStatus{}, err
	}
	idx, ok := m[attrIfindex]
	if !ok || len(idx) != 4 {
		return DevStatus{}, errors.New("macsec: reply without an interface index")
	}
	d := DevStatus{Ifindex: int(nl.NativeEndian().Uint32(idx)), Offload: "off", Counters: map[string]uint64{}}
	if v, ok := m[attrSecY]; ok {
		s, err := attrs(v)
		if err != nil {
			return d, err
		}
		d.SCI = sci(s[secyAttrSCI])
		d.EncodingSA = uint8(num(s[secyAttrEncodingSA]))
	}
	if v, ok := m[attrOffload]; ok {
		o, err := attrs(v)
		if err != nil {
			return d, err
		}
		if n, ok := offloadNames[uint8(num(o[offloadAttrType]))]; ok {
			d.Offload = n
		}
	}
	if v, ok := m[attrTxSAList]; ok {
		if d.TxSAs, err = parseSAs(v); err != nil {
			return d, err
		}
	}
	if v, ok := m[attrTxSCStats]; ok {
		if err := stats(d.Counters, v, txscStats); err != nil {
			return d, err
		}
	}
	if v, ok := m[attrSecYStats]; ok {
		if err := stats(d.Counters, v, secyStats); err != nil {
			return d, err
		}
	}
	if v, ok := m[attrRxSCList]; ok {
		entries, err := list(v)
		if err != nil {
			return d, err
		}
		for _, e := range entries {
			r, err := attrs(e)
			if err != nil {
				return d, err
			}
			sc := RxSCStatus{SCI: sci(r[rxscAttrSCI]), Active: num(r[rxscAttrActive]) != 0}
			if sa, ok := r[rxscAttrSAList]; ok {
				if sc.SAs, err = parseSAs(sa); err != nil {
					return d, err
				}
			}
			if st, ok := r[rxscAttrStats]; ok {
				if err := stats(d.Counters, st, rxscStats); err != nil {
					return d, err
				}
			}
			d.RxSCs = append(d.RxSCs, sc)
		}
	}
	return d, nil
}
