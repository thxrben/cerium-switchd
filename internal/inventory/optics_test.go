package inventory

import (
	"encoding/binary"
	"math"
	"testing"
)

func put16(b []byte, off int, v uint16) { binary.BigEndian.PutUint16(b[off:], v) }

// An SFP+ with diagnostics: identity, values and thresholds.
func TestParseSFP(t *testing.T) {
	e := make([]byte, 512)
	copy(e[20:], "FS              ")
	copy(e[40:], "SFP-10GSR-85    ")
	copy(e[68:], "F1234567        ")
	put16(e, 60, 850)
	e[92] = 0x68 // DDM implemented, internally calibrated
	a2 := e[256:]
	put16(a2, 96, uint16(int16(32*256+128))) // 32.5 °C
	put16(a2, 98, 33000)                     // 3.3 V
	put16(a2, 100, 3456)                     // 6.912 mA
	put16(a2, 102, 5420)                     // 0.542 mW
	put16(a2, 104, 4532)                     // 0.4532 mW
	put16(a2, 32, 10000)                     // rx high alarm 1 mW
	put16(a2, 34, 300)                       // rx low alarm 0.03 mW
	put16(a2, 38, 5000)                      // rx low warning 0.5 mW
	o, err := ParseModule(ModuleSFF8472, e)
	if err != nil {
		t.Fatal(err)
	}
	if o.Vendor != "FS" || o.PartNumber != "SFP-10GSR-85" || o.Serial != "F1234567" || o.Wavelength != 850 || !o.Diagnostics {
		t.Fatalf("identity %+v", o)
	}
	if o.Temperature != 32.5 || o.Voltage != 3.3 || math.Abs(o.Bias[0]-6.912) > 1e-9 || o.TxPower[0] != 0.542 || o.RxPower[0] != 0.4532 {
		t.Fatalf("values %+v", o)
	}
	if c := o.Thresholds.RxPower.Crossed(o.RxPower[0]); c != "low warning" {
		t.Fatalf("rx power crossed %q", c)
	}
	if d := DBm(0.5); math.Abs(d+3.0103) > 0.001 {
		t.Fatalf("dBm %v", d)
	}
	// Without diagnostics (SFF-8079): identity only.
	if o, _ := ParseModule(ModuleSFF8079, e[:256]); o.Diagnostics || o.Vendor != "FS" {
		t.Fatalf("plain SFP %+v", o)
	}
}

func TestParseQSFP(t *testing.T) {
	e := make([]byte, 256)
	copy(e[148:], "Mellanox        ")
	copy(e[168:], "MMA1B00-C100D   ")
	put16(e, 186, 850*20)
	put16(e, 22, 40*256)
	put16(e, 26, 32500)
	for lane := range 4 {
		put16(e, 34+2*lane, uint16(8000+lane))
		put16(e, 50+2*lane, 9000)
		put16(e, 42+2*lane, 3000)
	}
	o, err := ParseModule(ModuleSFF8636, e)
	if err != nil || o.Type != "QSFP" || o.Vendor != "Mellanox" || o.Wavelength != 850 || len(o.RxPower) != 4 || o.RxPower[3] != 0.8003 || o.Temperature != 40 {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := ParseModule(99, e); err != ErrNoModule {
		t.Fatal("unknown type")
	}
}
