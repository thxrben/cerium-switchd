package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSensors(t *testing.T) {
	root := t.TempDir()
	write := func(dev, file, v string) {
		d := filepath.Join(root, "class", "hwmon", dev)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, file), []byte(v+"\n"), 0o644)
	}
	write("hwmon0", "name", "coretemp")
	write("hwmon0", "temp1_input", "45000")
	write("hwmon0", "temp1_label", "Package id 0")
	write("hwmon0", "temp1_max", "80000")
	write("hwmon0", "temp1_crit", "100000")
	write("hwmon0", "temp10_input", "85000")
	write("hwmon0", "temp10_max", "80000")
	write("hwmon0", "temp2_input", "101000")
	write("hwmon0", "temp2_crit", "100000")
	write("hwmon1", "name", "nct6775")
	write("hwmon1", "fan1_input", "2400")
	write("hwmon1", "fan1_min", "500")
	write("hwmon1", "fan2_input", "0")
	write("hwmon1", "fan2_min", "500")
	write("hwmon1", "in0_input", "3312")
	write("hwmon2", "name", "pmbus")
	write("hwmon2", "power1_input", "120500000")
	write("hwmon2", "power1_label", "PSU1 input")
	ss := ReadSensors(root)
	if len(ss) != 7 {
		t.Fatalf("%d sensors: %+v", len(ss), ss)
	}
	want := []struct{ id, status, m string }{
		{"coretemp/Package id 0", "OK", "45 degrees C / 113 degrees F"},
		{"coretemp/temp2", "Critical", "101 degrees C / 214 degrees F"},
		{"coretemp/temp10", "Warning", "85 degrees C / 185 degrees F"},
		{"nct6775/fan1", "OK", "Spinning at 2400 RPM"},
		{"nct6775/fan2", "Critical", "Not spinning"},
		{"nct6775/in0", "OK", "3.312 V"},
		{"pmbus/PSU1 input", "OK", "120.5 W"},
	}
	for i, w := range want {
		if s := ss[i]; s.ID() != w.id || s.Status != w.status || s.Measurement() != w.m {
			t.Errorf("sensor %d: %s %s %q, want %+v", i, s.ID(), s.Status, s.Measurement(), w)
		}
	}
	if len(ReadSensors(t.TempDir())) != 0 {
		t.Fatal("no hwmon: no sensors")
	}
}
