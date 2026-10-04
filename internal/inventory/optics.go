package inventory

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
)

// Module types of ETHTOOL_GMODULEINFO.
const (
	ModuleSFF8079 = 1 // SFP without diagnostics (256 bytes)
	ModuleSFF8472 = 2 // SFP/SFP+ with diagnostics (512 bytes: A0 and A2)
	ModuleSFF8636 = 3 // QSFP+/QSFP28
	ModuleSFF8436 = 4 // QSFP
)

// Optics are a transceiver's identity and digital diagnostics (show
// interfaces diagnostics optics, reference 3.5).
type Optics struct {
	Type       string  `json:"type"` // SFP, QSFP
	Vendor     string  `json:"vendor,omitempty"`
	PartNumber string  `json:"part_number,omitempty"`
	Serial     string  `json:"serial,omitempty"`
	Wavelength float64 `json:"wavelength,omitempty"` // nm (0: copper or unknown)
	// Diagnostics: the module reports them (DOM); else only the identity.
	Diagnostics bool `json:"diagnostics"`
	// External: the values need external calibration (not applied:
	// shown as read).
	External    bool      `json:"external,omitempty"`
	Temperature float64   `json:"temperature"` // °C
	Voltage     float64   `json:"voltage"`     // V
	Bias        []float64 `json:"bias"`        // mA, per lane
	TxPower     []float64 `json:"tx_power"`    // mW, per lane
	RxPower     []float64 `json:"rx_power"`    // mW, per lane
	// Thresholds (SFF-8472 A2 page; nil: none).
	Thresholds *Thresholds `json:"thresholds,omitempty"`
}

// Limits are the alarm and warning limits of one value.
type Limits struct {
	HighAlarm, LowAlarm, HighWarn, LowWarn float64
}

// Thresholds are a module's limits.
type Thresholds struct {
	Temperature, Voltage, Bias, TxPower, RxPower Limits
}

// DBm converts milliwatts to dBm (-40 for no light).
func DBm(mw float64) float64 {
	if mw <= 0.0001 {
		return -40
	}
	return 10 * math.Log10(mw)
}

// ErrNoModule is a port without a readable module.
var ErrNoModule = errors.New("no transceiver module (or the driver cannot read it)")

func text(b []byte) string { return strings.TrimSpace(strings.TrimRight(string(b), "\x00")) }

func u16(b []byte, off int) float64 { return float64(binary.BigEndian.Uint16(b[off:])) }
func s16(b []byte, off int) float64 { return float64(int16(binary.BigEndian.Uint16(b[off:]))) }

// ParseModule reads a module EEPROM as ETHTOOL_GMODULEEEPROM returns it.
func ParseModule(typ int, eeprom []byte) (Optics, error) {
	switch typ {
	case ModuleSFF8079, ModuleSFF8472:
		return parseSFP(typ, eeprom)
	case ModuleSFF8636, ModuleSFF8436:
		return parseQSFP(eeprom)
	}
	return Optics{}, ErrNoModule
}

// parseSFP: SFF-8472, A0 (bytes 0-255) and A2 (256-511).
func parseSFP(typ int, e []byte) (Optics, error) {
	if len(e) < 96 {
		return Optics{}, ErrNoModule
	}
	o := Optics{Type: "SFP", Vendor: text(e[20:36]), PartNumber: text(e[40:56]), Serial: text(e[68:84])}
	// Wavelength (60-61, nm) only for optical modules (SFF-8472 8.1: copper
	// modules put cable compliance there; byte 8 bits 2-3).
	if e[8]&0x0c == 0 {
		o.Wavelength = u16(e, 60)
	}
	ddm := e[92]
	if typ != ModuleSFF8472 || ddm&0x40 == 0 || len(e) < 512 {
		return o, nil
	}
	o.Diagnostics, o.External = true, ddm&0x10 != 0
	a2 := e[256:]
	o.Temperature = s16(a2, 96) / 256
	o.Voltage = u16(a2, 98) / 10000
	o.Bias = []float64{u16(a2, 100) * 2 / 1000}
	o.TxPower = []float64{u16(a2, 102) / 10000}
	o.RxPower = []float64{u16(a2, 104) / 10000}
	lim := func(off int, conv func([]byte, int) float64) Limits {
		return Limits{HighAlarm: conv(a2, off), LowAlarm: conv(a2, off+2), HighWarn: conv(a2, off+4), LowWarn: conv(a2, off+6)}
	}
	o.Thresholds = &Thresholds{
		Temperature: lim(0, func(b []byte, i int) float64 { return s16(b, i) / 256 }),
		Voltage:     lim(8, func(b []byte, i int) float64 { return u16(b, i) / 10000 }),
		Bias:        lim(16, func(b []byte, i int) float64 { return u16(b, i) * 2 / 1000 }),
		TxPower:     lim(24, func(b []byte, i int) float64 { return u16(b, i) / 10000 }),
		RxPower:     lim(32, func(b []byte, i int) float64 { return u16(b, i) / 10000 }),
	}
	return o, nil
}

// parseQSFP: SFF-8636 lower page 0 (diagnostics) and upper page 0
// (identity), 256 bytes.
func parseQSFP(e []byte) (Optics, error) {
	if len(e) < 256 {
		return Optics{}, ErrNoModule
	}
	o := Optics{Type: "QSFP", Vendor: text(e[148:164]), PartNumber: text(e[168:184]), Serial: text(e[196:212]),
		Diagnostics: true}
	if wl := u16(e, 186); wl > 0 && e[147]>>4 < 0x0a { // not a copper transmitter technology
		o.Wavelength = wl / 20
	}
	o.Temperature = s16(e, 22) / 256
	o.Voltage = u16(e, 26) / 10000
	for lane := range 4 {
		o.RxPower = append(o.RxPower, u16(e, 34+2*lane)/10000)
		o.Bias = append(o.Bias, u16(e, 42+2*lane)*2/1000)
		o.TxPower = append(o.TxPower, u16(e, 50+2*lane)/10000)
	}
	return o, nil
}

// Crossed names the limits a value crosses ("high alarm", "low warning";
// "" none).
func (l Limits) Crossed(v float64) string {
	switch {
	case l.HighAlarm != 0 && v >= l.HighAlarm:
		return "high alarm"
	case l.LowAlarm != 0 && v <= l.LowAlarm:
		return "low alarm"
	case l.HighWarn != 0 && v >= l.HighWarn:
		return "high warning"
	case l.LowWarn != 0 && v <= l.LowWarn:
		return "low warning"
	}
	return ""
}
