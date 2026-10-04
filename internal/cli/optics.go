package cli

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/inventory"
)

// OpticsPort is one port's transceiver (Err: why there is none).
type OpticsPort struct {
	Name   string            `json:"name"`
	Optics *inventory.Optics `json:"optics,omitempty"`
	Err    string            `json:"err,omitempty"`
}

// Optics is implemented by members that read their transceivers.
type Optics interface {
	Optics(iface string) ([]OpticsPort, error)
}

// showOptics is "show interfaces diagnostics optics [<interface>]".
func (sh *Shell) showOptics(c *call) error {
	iface := ""
	switch len(c.args) {
	case 0:
	case 1:
		iface = c.args[0].Text
	default:
		return &posError{pos: c.argPos(1), msg: "syntax error, expecting one interface"}
	}
	o, ok := sh.env.Ops.(Optics)
	if sh.env.Ops == nil || !ok {
		return errors.New("transceiver information is not available")
	}
	ps, err := o.Optics(iface)
	if err := partial(c, err); err != nil {
		return err
	}
	ps = slices.DeleteFunc(ps, func(p OpticsPort) bool { return !c.shows(p.Name) })
	sort.SliceStable(ps, func(i, j int) bool { return config.NaturalLess(ps[i].Name, ps[j].Name) })
	if iface != "" && len(ps) == 0 {
		return fmt.Errorf("%s: no such port", iface)
	}
	for i, p := range ps {
		if i > 0 {
			c.out.WriteString("\n")
		}
		writeOptics(c.out, p, iface != "")
	}
	return nil
}

func writeOptics(out *strings.Builder, p OpticsPort, explicit bool) {
	fmt.Fprintf(out, "Physical interface: %s\n", p.Name)
	o := p.Optics
	if o == nil {
		if explicit || !strings.Contains(p.Err, "no transceiver") {
			fmt.Fprintf(out, "    %s\n", p.Err)
		} else {
			out.WriteString("    No transceiver module\n")
		}
		return
	}
	line := func(name, value string) { fmt.Fprintf(out, "    %-42s:  %s\n", name, value) }
	id := strings.TrimSpace(o.Vendor + " " + o.PartNumber)
	if o.Serial != "" {
		id += ", serial " + o.Serial
	}
	if o.Wavelength > 0 {
		id += fmt.Sprintf(", %.0f nm", o.Wavelength)
	}
	line("Module", o.Type+" "+id)
	if !o.Diagnostics {
		out.WriteString("    The module reports no digital diagnostics\n")
		return
	}
	if o.External {
		out.WriteString("    (externally calibrated module: values shown as read)\n")
	}
	power := func(mw float64) string { return fmt.Sprintf("%.4f mW / %.2f dBm", mw, inventory.DBm(mw)) }
	lanes := func(name string, vs []float64, f func(float64) string) {
		if len(vs) == 1 {
			line(name, f(vs[0]))
			return
		}
		for i, v := range vs {
			line(fmt.Sprintf("Lane %d %s", i, strings.ToLower(name[:1])+name[1:]), f(v))
		}
	}
	lanes("Laser bias current", o.Bias, func(v float64) string { return fmt.Sprintf("%.3f mA", v) })
	lanes("Laser output power", o.TxPower, power)
	line("Module temperature", fmt.Sprintf("%.0f degrees C / %.0f degrees F", o.Temperature, o.Temperature*9/5+32))
	line("Module voltage", fmt.Sprintf("%.4f V", o.Voltage))
	lanes("Receiver signal average optical power", o.RxPower, power)
	t := o.Thresholds
	if t == nil {
		return
	}
	state := func(name string, l inventory.Limits, v float64) {
		c := l.Crossed(v)
		for _, kind := range []string{"high alarm", "low alarm", "high warning", "low warning"} {
			on := "Off"
			if c == kind {
				on = "On"
			}
			line(name+" "+kind, on)
		}
	}
	state("Laser bias current", t.Bias, o.Bias[0])
	state("Laser output power", t.TxPower, o.TxPower[0])
	state("Module temperature", t.Temperature, o.Temperature)
	state("Module voltage", t.Voltage, o.Voltage)
	state("Laser rx power", t.RxPower, o.RxPower[0])
	limits := func(name string, l inventory.Limits, f func(float64) string) {
		line(name+" high alarm threshold", f(l.HighAlarm))
		line(name+" low alarm threshold", f(l.LowAlarm))
		line(name+" high warning threshold", f(l.HighWarn))
		line(name+" low warning threshold", f(l.LowWarn))
	}
	limits("Laser bias current", t.Bias, func(v float64) string { return fmt.Sprintf("%.3f mA", v) })
	limits("Laser output power", t.TxPower, power)
	limits("Module temperature", t.Temperature, func(v float64) string { return fmt.Sprintf("%.0f degrees C", v) })
	limits("Module voltage", t.Voltage, func(v float64) string { return fmt.Sprintf("%.4f V", v) })
	limits("Laser rx power", t.RxPower, power)
}
