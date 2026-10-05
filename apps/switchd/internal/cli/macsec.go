package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
)

// MACsecConn is one secured interface (show security macsec connections,
// reference 5.15).
type MACsecConn struct {
	Member    int       `json:"member"`
	Interface string    `json:"interface"` // switch name ("1/1/0")
	Dev       string    `json:"dev"`
	CA        string    `json:"ca"` // "stack" for a stacking link
	Cipher    string    `json:"cipher"`
	State     string    `json:"state"`
	TxSCI     string    `json:"tx_sci,omitempty"`
	TxAN      int       `json:"tx_an"`
	RxSCs     []string  `json:"rx_scs,omitempty"`
	Offload   string    `json:"offload,omitempty"`
	KeySince  time.Time `json:"key_since,omitzero"`
	Neighbour string    `json:"neighbour,omitempty"`
	// Counters of the kernel (show security macsec statistics).
	Counters map[string]uint64 `json:"counters,omitempty"`
}

// MACsec is implemented by switches with MACsec.
type MACsec interface {
	MACsec() ([]MACsecConn, error)
}

func (sh *Shell) macsecConns(c *call) ([]MACsecConn, error) {
	o, ok := sh.env.Ops.(MACsec)
	if sh.env.Ops == nil || !ok {
		return nil, errors.New("MACsec information is not available")
	}
	var only string
	switch {
	case len(c.args) == 0:
	case len(c.args) == 2 && prefixOf(c.args[0].Text, "interface"):
		only = c.args[1].Text
	default:
		return nil, &posError{pos: c.argPos(0), msg: "syntax error, expecting 'interface <name>'"}
	}
	cs, err := o.MACsec()
	if err := partial(c, err); err != nil {
		return nil, err
	}
	out := cs[:0]
	for _, x := range cs {
		if (only == "" || x.Interface == only) && c.shows(x.Interface) {
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return config.NaturalLess(out[i].Interface, out[j].Interface) })
	return out, nil
}

// showMACsecConnections is "show security macsec connections".
func (sh *Shell) showMACsecConnections(c *call) error {
	cs, err := sh.macsecConns(c)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		c.out.WriteString("No MACsec interfaces\n")
		return nil
	}
	for i, x := range cs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Interface %s (%s), connectivity association %s\n", x.Interface, x.Dev, x.CA)
		line := func(n, v string) { fmt.Fprintf(c.out, "  %-22s %s\n", n+":", v) }
		line("State", x.State)
		line("Cipher suite", x.Cipher)
		if x.Neighbour != "" {
			line("Neighbour", x.Neighbour)
		}
		tx := "-"
		if x.TxAN >= 0 && x.TxSCI != "" {
			tx = fmt.Sprintf("SCI %s, association %d", x.TxSCI, x.TxAN)
		}
		line("Transmit", tx)
		line("Receive", orDash(strings.Join(x.RxSCs, ", ")))
		off := x.Offload
		if off == "" || off == "off" {
			off = "software"
		}
		line("Encryption", off)
		if !x.KeySince.IsZero() {
			line("Key installed", x.KeySince.Format("2006-01-02 15:04:05 MST"))
		}
	}
	return nil
}

// macsecCounters are the kernel's counters shown, in order.
var macsecCounters = []struct{ key, name string }{
	{"OutPktsProtected", "Packets protected"}, {"OutPktsEncrypted", "Packets encrypted"},
	{"OutOctetsEncrypted", "Bytes encrypted"}, {"InPktsOK", "Packets validated"}, {"InOctetsDecrypted", "Bytes decrypted"},
	{"InPktsNoSA", "Dropped: no SA"}, {"InPktsBadTag", "Dropped: bad tag"}, {"InPktsNotValid", "Dropped: not valid"},
	{"InPktsInvalid", "Dropped: invalid"}, {"InPktsLate", "Dropped: late (replay)"}, {"InPktsNoTag", "Untagged received"},
	{"InPktsUnknownSCI", "Unknown SCI"},
}

// showMACsecStatistics is "show security macsec statistics".
func (sh *Shell) showMACsecStatistics(c *call) error {
	cs, err := sh.macsecConns(c)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		c.out.WriteString("No MACsec interfaces\n")
		return nil
	}
	for i, x := range cs {
		if i > 0 {
			c.out.WriteString("\n")
		}
		fmt.Fprintf(c.out, "Interface %s (%s)\n", x.Interface, x.Dev)
		for _, k := range macsecCounters {
			fmt.Fprintf(c.out, "  %-24s %d\n", k.name+":", x.Counters[k.key])
		}
	}
	return nil
}
