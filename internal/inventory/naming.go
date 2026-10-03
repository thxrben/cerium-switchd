package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/schema"
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
	cards   map[string]int       // card identity -> number (pinned)
	info    map[string]*CardInfo // card identity -> what it was when last seen
	notes   map[int]string       // card number -> change noticed in this run
	moved   map[int]int          // new card number -> number of the absent card it matches
	present map[string]bool
	loaded  bool
	ports   []Port
	byName  map[string]Port
	byLinux map[string]Port
}

type namingState struct {
	Cards map[string]int       `json:"cards"`
	Info  map[string]*CardInfo `json:"info,omitempty"`
}

// CardInfo describes a card as it was last seen.
type CardInfo struct {
	Driver   string    `json:"driver"`
	Ports    int       `json:"ports"`
	MACs     []string  `json:"macs"`
	LastSeen time.Time `json:"last_seen"`
}

// CardStatus is a known card for "show chassis hardware".
type CardStatus struct {
	Number  int
	Key     string // "pci:0000:01:00"
	Present bool
	CardInfo
	// Note: the card changed model in its slot this run; MovedFrom: a new
	// card whose ports carry the MAC addresses of this absent card.
	Note      string
	MovedFrom int // -1: none
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
	n.cards, n.info, n.notes, n.moved = map[string]int{}, map[string]*CardInfo{}, map[int]string{}, map[int]int{}
	if raw, err := os.ReadFile(n.StateFile); err == nil {
		var st namingState
		if json.Unmarshal(raw, &st) == nil && st.Cards != nil {
			n.cards = st.Cards
			if st.Info != nil {
				n.info = st.Info
			}
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
	// What each present card is now.
	now := time.Now().UTC().Truncate(time.Second)
	cur := map[string]*CardInfo{}
	for _, f := range fs {
		ci := cur[f.Card]
		if ci == nil {
			ci = &CardInfo{Driver: f.Driver, LastSeen: now}
			cur[f.Card] = ci
		}
		ci.Ports++
		if f.MAC != "" {
			ci.MACs = append(ci.MACs, f.MAC)
		}
	}
	dirty := len(fresh) > 0
	for key, ci := range cur {
		slices.Sort(ci.MACs)
		old := n.info[key]
		num := n.cards[key]
		switch {
		case old == nil:
			// New (or first seen by this version): does it carry the MAC
			// addresses of an absent card, i.e. did a card move slots?
			if slices.ContainsFunc(fresh, func(c card) bool { return c.key == key }) {
				for k2, o2 := range n.info {
					if _, here := cur[k2]; !here && slices.ContainsFunc(ci.MACs, func(m string) bool { return slices.Contains(o2.MACs, m) }) {
						n.moved[num] = n.cards[k2]
					}
				}
			}
		case old.Driver != ci.Driver || old.Ports != ci.Ports:
			n.notes[num] = fmt.Sprintf("changed model in its slot: was %s with %d ports, now %s with %d", old.Driver, old.Ports, ci.Driver, ci.Ports)
		}
		if old == nil || old.Driver != ci.Driver || old.Ports != ci.Ports || !slices.Equal(old.MACs, ci.MACs) {
			dirty = true
		}
		n.info[key] = ci
	}
	// Cards that went away: their last-seen time is the last save.
	for key := range n.present {
		if _, ok := cur[key]; !ok {
			dirty = true
		}
	}
	n.present = map[string]bool{}
	for key := range cur {
		n.present[key] = true
	}
	if dirty {
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
	raw, err := json.Marshal(namingState{Cards: n.cards, Info: n.info})
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

// Cards lists the known cards in number order.
func (n *Naming) Cards() []CardStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.load()
	var out []CardStatus
	for key, num := range n.cards {
		cs := CardStatus{Number: num, Key: key, Present: n.present[key], Note: n.notes[num], MovedFrom: -1}
		if ci := n.info[key]; ci != nil {
			cs.CardInfo = *ci
		}
		if m, ok := n.moved[num]; ok {
			cs.MovedFrom = m
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func (n *Naming) keyOf(num int) string {
	for k, v := range n.cards {
		if v == num {
			return k
		}
	}
	return ""
}

// Renumber gives present card from the number to, which must be free or
// belong to an absent card (that card is forgotten): e.g. a card moved to
// another slot takes back its old number, so its ports keep their names.
func (n *Naming) Renumber(from, to int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.load()
	if to < 0 || to > schema.MaxCard {
		return fmt.Errorf("card number %d is out of range (0-%d)", to, schema.MaxCard)
	}
	kf := n.keyOf(from)
	if kf == "" || !n.present[kf] {
		return fmt.Errorf("card %d is not present", from)
	}
	if from == to {
		return nil
	}
	if kt := n.keyOf(to); kt != "" {
		if n.present[kt] {
			return fmt.Errorf("card %d is present; only the number of an absent card can be taken", to)
		}
		delete(n.cards, kt)
		delete(n.info, kt)
	}
	n.cards[kf] = to
	delete(n.moved, from)
	delete(n.notes, from)
	return n.save()
}

// Forget releases the number of an absent card.
func (n *Naming) Forget(num int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.load()
	k := n.keyOf(num)
	switch {
	case k == "":
		return fmt.Errorf("there is no card %d", num)
	case n.present[k]:
		return fmt.Errorf("card %d is present; only absent cards can be forgotten", num)
	}
	delete(n.cards, k)
	delete(n.info, k)
	for nw, old := range n.moved {
		if old == num {
			delete(n.moved, nw)
		}
	}
	return n.save()
}
