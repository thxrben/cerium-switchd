package cli

import (
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/apps/switchd/internal/inventory"
)

type opticsOps struct{ fakeOps }

func (opticsOps) Optics(iface string) ([]OpticsPort, error) {
	all := []OpticsPort{
		{Name: "1/0/1", Optics: &inventory.Optics{Type: "SFP", Vendor: "FS", PartNumber: "SFP-10GSR-85", Serial: "F1", Wavelength: 850,
			Diagnostics: true, Temperature: 32.5, Voltage: 3.3, Bias: []float64{6.912}, TxPower: []float64{0.542}, RxPower: []float64{0.4532},
			Thresholds: &inventory.Thresholds{RxPower: inventory.Limits{HighAlarm: 1, LowAlarm: 0.03, HighWarn: 0.9, LowWarn: 0.5}}}},
		{Name: "1/0/2", Err: inventory.ErrNoModule.Error()},
		{Name: "2/0/1", Optics: &inventory.Optics{Type: "QSFP", Vendor: "Mellanox", Diagnostics: true, Temperature: 40, Voltage: 3.25,
			Bias: []float64{6, 6, 6, 6}, TxPower: []float64{0.9, 0.9, 0.9, 0.9}, RxPower: []float64{0.8, 0.8, 0.8, 0}}},
	}
	if iface == "" {
		return all, nil
	}
	var out []OpticsPort
	for _, p := range all {
		if p.Name == iface {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestShowOptics(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.sh.env.Ops = &opticsOps{}
	out := ts.ok("show interfaces diagnostics optics")
	contains(t, out, "Physical interface: 1/0/1", "Module                                    :  SFP FS SFP-10GSR-85, serial F1, 850 nm",
		"Laser bias current                        :  6.912 mA", "Laser output power                        :  0.5420 mW / -2.66 dBm",
		"Module temperature                        :  32 degrees C / 90 degrees F", "Laser rx power low warning                :  On",
		"Laser rx power high alarm                 :  Off", "Physical interface: 1/0/2", "No transceiver module",
		"Lane 3 receiver signal average optical power:  0.0000 mW / -40.00 dBm")
	contains(t, ts.ok("show interfaces diagnostics optics 1/0/2"), "no transceiver module")
	contains(t, ts.run("show interfaces diagnostics optics 9/9/9"), "no such port")
	contains(t, ts.run("show interfaces 1/0/1"), "interface 1/0/1 not found") // the plain command still takes an interface
}
