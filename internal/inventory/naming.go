package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"mclag/internal/config"
	"mclag/internal/schema"
)

// Port is a physical port of this member with its Junos-style name
// (reference 1.6).
type Port struct {
	Name   string // "<member>/<card>/<port>"
	Linux  string
	Card   string // card identity, e.g. "pci:0000:01:00", "usb:1-2"
	Bus    string // bus address of the port, e.g. PCI "0000:01:00.1"
	Driver string
	MAC    string
}

// Naming numbers the physical ports of one member: the card is the parent
// device (a PCI device without the function number, a USB device, or
// another device such as an ARM SoC's Ethernet block), the port is the
// position on that card. Card numbers are assigned in PCI address order on
// first sight and pinned in StateFile, so later hardware changes never
// renumber existing ports.
type Naming struct {
	SysRoot   string // "/sys"
	StateFile string
	Member    int

	mu      sync.Mutex
	cards   map[string]int // card identity -> number (pinned)
	loaded  bool
	ports   []Port
	byName  map[string]Port
	byLinux map[string]Port
}

type namingState struct {
	Cards map[string]int `json:"cards"`
}

var (
	pciRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	usbRe = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)
)

// found is a port during discovery.
type found struct {
	Port
	cardKind int // 0 = PCI, 1 = other
	function int
	devPort  int
	physName string
}

func (n *Naming) readFile(parts ...string) string {
	raw, err := os.ReadFile(filepath.Join(append([]string{n.SysRoot}, parts...)...))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// discover lists the physical ports with their card identity.
func (n *Naming) discover() []found {
	netDir := filepath.Join(n.SysRoot, "class", "net")
	ents, _ := os.ReadDir(netDir)
	var out []found
	for _, e := range ents {
		linux := e.Name()
		dev, err := filepath.EvalSymlinks(filepath.Join(netDir, linux, "device"))
		if err != nil {
			continue // virtual device
		}
		if _, err := os.Stat(filepath.Join(dev, "physfn")); err == nil {
			continue // SR-IOV virtual function
		}
		f := found{Port: Port{Linux: linux}, cardKind: 1}
		// The device nearest to the interface decides: a USB adapter sits
		// below a PCI USB controller.
		parts := strings.Split(dev, string(filepath.Separator))
		kind, comp := "", ""
		for i := len(parts) - 1; i >= 0 && kind == ""; i-- {
			switch {
			case pciRe.MatchString(parts[i]):
				kind, comp = "pci", parts[i]
			case usbRe.MatchString(parts[i]):
				kind, comp = "usb", parts[i]
			}
		}
		switch kind {
		case "pci":
			f.Card, f.Bus = "pci:"+comp[:len(comp)-2], comp
			f.cardKind = 0
			f.function, _ = strconv.Atoi(comp[len(comp)-1:])
		case "usb":
			f.Card, f.Bus = "usb:"+comp, filepath.Base(dev)
		default:
			rel, err := filepath.Rel(filepath.Join(n.SysRoot, "devices"), dev)
			if err != nil {
				rel = dev
			}
			f.Card, f.Bus = "dev:"+rel, filepath.Base(dev)
		}
		f.devPort, _ = strconv.Atoi(n.readFile("class", "net", linux, "dev_port"))
		f.physName = n.readFile("class", "net", linux, "phys_port_name")
		f.MAC = n.readFile("class", "net", linux, "address")
		if drv, err := filepath.EvalSymlinks(filepath.Join(dev, "driver")); err == nil {
			f.Driver = filepath.Base(drv)
		}
		out = append(out, f)
	}
	return out
}

func (n *Naming) load() {
	if n.loaded {
		return
	}
	n.loaded = true
	n.cards = map[string]int{}
	if raw, err := os.ReadFile(n.StateFile); err == nil {
		var st namingState
		if json.Unmarshal(raw, &st) == nil && st.Cards != nil {
			n.cards = st.Cards
		}
	}
}

// Refresh rediscovers the ports. New cards get numbers (and are stored);
// it reports whether the set of ports changed.
func (n *Naming) Refresh() (changed bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.load()
	fs := n.discover()

	// Number the new cards: PCI in address order first, then the others.
	type card struct {
		key  string
		kind int
	}
	var fresh []card
	seen := map[string]bool{}
	for _, f := range fs {
		if _, ok := n.cards[f.Card]; !ok && !seen[f.Card] {
			seen[f.Card] = true
			fresh = append(fresh, card{f.Card, f.cardKind})
		}
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].kind != fresh[j].kind {
			return fresh[i].kind < fresh[j].kind
		}
		return config.NaturalLess(fresh[i].key, fresh[j].key)
	})
	used := map[int]bool{}
	for _, v := range n.cards {
		used[v] = true
	}
	next := 0
	for _, c := range fresh {
		for used[next] {
			next++
		}
		if next > schema.MaxCard {
			err = errors.Join(err, fmt.Errorf("no card number left for %s", c.key))
			continue
		}
		n.cards[c.key] = next
		used[next] = true
	}
	if len(fresh) > 0 {
		err = errors.Join(err, n.save())
	}

	// Ports within a card: by PCI function, then the driver's port number.
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.Card != b.Card:
			return n.cards[a.Card] < n.cards[b.Card]
		case a.function != b.function:
			return a.function < b.function
		case a.devPort != b.devPort:
			return a.devPort < b.devPort
		case a.physName != b.physName:
			return config.NaturalLess(a.physName, b.physName)
		}
		return config.NaturalLess(a.Linux, b.Linux)
	})
	var ports []Port
	idx := map[string]int{}
	for _, f := range fs {
		num, ok := n.cards[f.Card]
		if !ok {
			continue
		}
		p := f.Port
		p.Name = schema.Port{Member: n.Member, Card: num, Port: idx[f.Card]}.String()
		idx[f.Card]++
		ports = append(ports, p)
	}
	changed = !samePorts(ports, n.ports)
	n.ports = ports
	n.byName, n.byLinux = map[string]Port{}, map[string]Port{}
	for _, p := range ports {
		n.byName[p.Name], n.byLinux[p.Linux] = p, p
	}
	return changed, err
}

func samePorts(a, b []Port) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (n *Naming) save() error {
	if n.StateFile == "" {
		return nil
	}
	raw, err := json.Marshal(namingState{Cards: n.cards})
	if err != nil {
		return err
	}
	tmp := n.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.StateFile)
}

// Linux returns the kernel name of a port ("1/0/3").
func (n *Naming) Linux(name string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.byName[name]
	return p.Linux, ok
}

// Name returns the interface name of a kernel interface.
func (n *Naming) Name(linux string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.byLinux[linux]
	return p.Name, ok
}

// Ports returns the present ports in name order.
func (n *Naming) Ports() []Port {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Port(nil), n.ports...)
}

// LinuxNames returns the kernel names of the present ports.
func (n *Naming) LinuxNames() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.ports))
	for _, p := range n.ports {
		out = append(out, p.Linux)
	}
	return out
}
