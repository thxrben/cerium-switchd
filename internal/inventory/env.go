package inventory

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Sensor is one hwmon sensor (show chassis environment, reference 3.5).
type Sensor struct {
	Class  string  `json:"class"` // Temp, Fans, Voltage, Power
	Chip   string  `json:"chip"`  // the hwmon device's name (coretemp, nct6775, pmbus)
	Label  string  `json:"label"` // its label, else temp1, fan2 …
	Value  float64 `json:"value"` // °C, RPM, V, W
	Max    float64 `json:"max,omitempty"`
	Crit   float64 `json:"crit,omitempty"`
	Min    float64 `json:"min,omitempty"` // fans
	Status string  `json:"status"`        // OK, Warning, Critical
}

// ID identifies a sensor on a member ("coretemp/Core 0").
func (s Sensor) ID() string { return s.Chip + "/" + s.Label }

// sensor kinds: file prefix, class, divisor of the raw value.
var sensorKinds = []struct {
	prefix, class string
	div           float64
}{
	{"temp", "Temp", 1000}, {"fan", "Fans", 1}, {"in", "Voltage", 1000}, {"power", "Power", 1e6},
}

// ReadSensors reads every hwmon sensor under sysRoot (/sys).
func ReadSensors(sysRoot string) []Sensor {
	devs, _ := filepath.Glob(filepath.Join(sysRoot, "class", "hwmon", "hwmon*"))
	slices.Sort(devs)
	var out []Sensor
	for _, d := range devs {
		chip := readTrim(filepath.Join(d, "name"))
		if chip == "" {
			chip = filepath.Base(d)
		}
		for _, k := range sensorKinds {
			files, _ := filepath.Glob(filepath.Join(d, k.prefix+"*_input"))
			slices.SortFunc(files, func(a, b string) int { return naturalCmp(a, b) })
			for _, f := range files {
				base := strings.TrimSuffix(filepath.Base(f), "_input")
				if _, err := strconv.Atoi(strings.TrimPrefix(base, k.prefix)); err != nil {
					continue
				}
				v, ok := readNum(f, k.div)
				if !ok {
					continue
				}
				s := Sensor{Class: k.class, Chip: chip, Label: readTrim(filepath.Join(d, base+"_label")), Value: v}
				if s.Label == "" {
					s.Label = base
				}
				s.Max, _ = readNum(filepath.Join(d, base+"_max"), k.div)
				s.Crit, _ = readNum(filepath.Join(d, base+"_crit"), k.div)
				s.Min, _ = readNum(filepath.Join(d, base+"_min"), k.div)
				s.Status = status(s)
				out = append(out, s)
			}
		}
	}
	return out
}

func status(s Sensor) string {
	if s.Class == "Fans" {
		switch {
		case s.Value == 0 && s.Min > 0:
			return "Critical" // stopped
		case s.Min > 0 && s.Value < s.Min:
			return "Warning"
		}
		return "OK"
	}
	switch {
	case s.Crit > 0 && s.Value >= s.Crit:
		return "Critical"
	case s.Max > 0 && s.Value >= s.Max:
		return "Warning"
	}
	return "OK"
}

// Measurement is the value as shown.
func (s Sensor) Measurement() string {
	switch s.Class {
	case "Temp":
		return fmt.Sprintf("%.0f degrees C / %.0f degrees F", s.Value, s.Value*9/5+32)
	case "Fans":
		if s.Value == 0 {
			return "Not spinning"
		}
		return fmt.Sprintf("Spinning at %.0f RPM", s.Value)
	case "Voltage":
		return fmt.Sprintf("%.3f V", s.Value)
	case "Power":
		return fmt.Sprintf("%.1f W", s.Value)
	}
	return fmt.Sprint(s.Value)
}

func readTrim(path string) string {
	raw, err := hwio.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func readNum(path string, div float64) (float64, bool) {
	s := readTrim(path)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n / div, true
}

// naturalCmp orders temp2 before temp10.
func naturalCmp(a, b string) int {
	na, nb := trailingNum(a), trailingNum(b)
	if na != nb {
		return na - nb
	}
	return strings.Compare(a, b)
}

func trailingNum(s string) int {
	s = strings.TrimSuffix(filepath.Base(s), "_input")
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return n
}
