// Package inventory discovers the local hardware (network ports).
package inventory

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/thxrben/cerium-switchd/internal/config"
)

// PhysicalPorts lists physical network interfaces from sysfs (those with a
// backing device), excluding loopback and virtual devices. sysRoot is
// normally "/sys".
func PhysicalPorts(sysRoot string) []string {
	ents, err := os.ReadDir(filepath.Join(sysRoot, "class", "net"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(sysRoot, "class", "net", e.Name(), "device")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return config.NaturalLess(out[i], out[j]) })
	return out
}
