package schema

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Type describes the value space of a leaf, leaf-list or list key.
type Type struct {
	// Name is shown in help output, e.g. "<vlan-id>".
	Name string
	// Desc is a human readable description of the value space.
	Desc string
	// Enum, if set, restricts values to the given keywords.
	Enum []EnumValue
	// Check validates a raw value and returns its canonical form.
	Check func(string) (string, error)
	// Ref names a dynamic completion source (e.g. "interface", "vlan").
	// The source is resolved by the CLI at runtime.
	Ref string
}

// EnumValue is one allowed keyword of an enum type.
type EnumValue struct {
	Name string
	Help string
}

// Validate checks v against the type and returns its canonical form.
func (t *Type) Validate(v string) (string, error) {
	if t == nil {
		return v, nil
	}
	for i := 0; i < len(v); i++ {
		// Control characters would break the text formats and could
		// inject terminal escape sequences into show output.
		if v[i] < 0x20 || v[i] == 0x7f {
			return "", fmt.Errorf("value contains control characters")
		}
	}
	if !utf8.ValidString(v) {
		return "", fmt.Errorf("value is not valid UTF-8")
	}
	for _, r := range v {
		if r >= 0x80 && r <= 0x9f {
			return "", fmt.Errorf("value contains control characters")
		}
	}
	if len(t.Enum) > 0 {
		for _, e := range t.Enum {
			if e.Name == v {
				return v, nil
			}
		}
		// Unique-prefix abbreviation is accepted for keyword enums
		// (never for numeric values like baud rates).
		match := ""
		for _, e := range t.Enum {
			if v != "" && isLetter(v[0]) && strings.HasPrefix(e.Name, v) {
				if match != "" {
					return "", fmt.Errorf("ambiguous value %q", v)
				}
				match = e.Name
			}
		}
		if match != "" {
			return match, nil
		}
		return "", fmt.Errorf("invalid value %q, expecting one of: %s", v, t.enumNames())
	}
	if t.Check != nil {
		return t.Check(v)
	}
	return v, nil
}

func (t *Type) enumNames() string {
	n := make([]string, len(t.Enum))
	for i, e := range t.Enum {
		n[i] = e.Name
	}
	return strings.Join(n, ", ")
}

// Enum builds an enum type.
func Enum(values ...EnumValue) *Type {
	return &Type{Name: "<value>", Enum: values}
}

// E is shorthand for an EnumValue.
func E(name, help string) EnumValue { return EnumValue{Name: name, Help: help} }

// Uint returns an unsigned integer type within [min,max].
func Uint(name string, min, max uint64) *Type {
	return &Type{
		Name: name,
		Desc: fmt.Sprintf("%d..%d", min, max),
		Check: func(s string) (string, error) {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return "", fmt.Errorf("invalid number %q", s)
			}
			if n < min || n > max {
				return "", fmt.Errorf("value %d out of range (%d..%d)", n, min, max)
			}
			return strconv.FormatUint(n, 10), nil
		},
	}
}

// UintStep is like Uint but also requires the value to be a multiple of step.
func UintStep(name string, min, max, step uint64) *Type {
	t := Uint(name, min, max)
	inner := t.Check
	t.Desc = fmt.Sprintf("%d..%d in steps of %d", min, max, step)
	t.Check = func(s string) (string, error) {
		v, err := inner(s)
		if err != nil {
			return "", err
		}
		n, _ := strconv.ParseUint(v, 10, 64)
		if n%step != 0 {
			return "", fmt.Errorf("value %d must be a multiple of %d", n, step)
		}
		return v, nil
	}
	return t
}

// String returns a free-form string type limited to maxLen bytes, optionally
// constrained by a regular expression.
func String(name string, maxLen int, pattern string) *Type {
	var re *regexp.Regexp
	if pattern != "" {
		re = regexp.MustCompile(pattern)
	}
	return &Type{
		Name: name,
		Check: func(s string) (string, error) {
			if s == "" {
				return "", fmt.Errorf("empty value")
			}
			if len(s) > maxLen {
				return "", fmt.Errorf("value too long (max %d characters)", maxLen)
			}
			if re != nil && !re.MatchString(s) {
				return "", fmt.Errorf("invalid value %q", s)
			}
			return s, nil
		},
	}
}

var (
	// Text is a free-form description string.
	Text = String("<text>", 255, "")

	// Hostname is an RFC 1123 host name label sequence.
	Hostname = String("<hostname>", 253,
		`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

	// Identifier is a config object name (vlan names, analyzer names, ...).
	Identifier = String("<name>", 64, `^[A-Za-z][A-Za-z0-9_.-]*$`)

	// Username is a local user account name.
	Username = String("<username>", 32, `^[a-z_][a-z0-9_-]*$`)

	IPv4 = &Type{Name: "<ipv4-address>", Check: checkIP(4)}
	IPv6 = &Type{Name: "<ipv6-address>", Check: checkIP(6)}
	IP   = &Type{Name: "<ip-address>", Check: checkIP(0)}

	// IPPrefix is an interface address with prefix length, e.g. 10.0.0.1/24.
	IPPrefix = &Type{Name: "<address/prefix>", Check: checkPrefix}

	// RoutePrefix is a destination network (no host bits).
	RoutePrefix = &Type{Name: "<prefix>", Check: func(s string) (string, error) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return "", fmt.Errorf("invalid prefix %q", s)
		}
		if m := p.Masked(); m != p {
			return "", fmt.Errorf("%s has host bits set; the network is %s", s, m)
		}
		return p.String(), nil
	}}

	// IrbUnit references a VLAN IP interface "irb.<n>".
	IrbUnit = &Type{Name: "<irb-unit>", Ref: "irb-unit", Check: func(s string) (string, error) {
		n, ok := strings.CutPrefix(s, "irb.")
		v, err := strconv.Atoi(n)
		if !ok || err != nil || v < 0 || v > MaxUnit || strconv.Itoa(v) != n {
			return "", fmt.Errorf("invalid irb unit %q (expecting irb.<0-%d>, e.g. irb.10)", s, MaxUnit)
		}
		return s, nil
	}}

	// Host is an IP address or DNS name.
	Host = &Type{Name: "<host>", Check: func(s string) (string, error) {
		if v, err := checkIP(0)(s); err == nil {
			return v, nil
		}
		return Hostname.Check(s)
	}}

	MAC = &Type{Name: "<mac-address>", Check: func(s string) (string, error) {
		hw, err := net.ParseMAC(s)
		if err != nil || len(hw) != 6 {
			return "", fmt.Errorf("invalid MAC address %q", s)
		}
		return hw.String(), nil
	}}

	VlanID = Uint("<vlan-id>", 1, 4094)
	VNI    = Uint("<vni>", 1, 16777214)
	MTU    = Uint("<mtu>", 256, 16000)

	// Interface is a switch interface name: "<member>/<card>/<port>" for
	// physical ports or "ae<N>" for aggregated interfaces.
	Interface = &Type{Name: "<interface-name>", Ref: "interface", Check: CheckInterfaceName}

	// PhysInterface only accepts "<member>/<card>/<port>".
	PhysInterface = &Type{Name: "<interface-name>", Ref: "physical-interface", Check: func(s string) (string, error) {
		p, ok := ParsePhysical(s)
		if !ok {
			return "", fmt.Errorf("invalid physical interface name %q (expecting <member>/<card>/<port>, e.g. 1/0/0)", s)
		}
		return p.String(), nil
	}}

	// AEInterface only accepts "ae<N>".
	AEInterface = &Type{Name: "<ae-interface>", Ref: "ae-interface", Check: func(s string) (string, error) {
		if !aeRe.MatchString(s) {
			return "", fmt.Errorf("invalid aggregated interface name %q (expecting ae0..ae4095)", s)
		}
		return s, nil
	}}

	// VlanRef references a VLAN by name or by numeric id or id range.
	VlanRef = &Type{Name: "<vlan>", Ref: "vlan", Check: func(s string) (string, error) {
		if s == "all" {
			return s, nil
		}
		if _, _, ok := ParseVlanRange(s); ok {
			return s, nil
		}
		return Identifier.Check(s)
	}}

	// VlanSingle references exactly one VLAN by name or id.
	VlanSingle = &Type{Name: "<vlan>", Ref: "vlan", Check: func(s string) (string, error) {
		if lo, hi, ok := ParseVlanRange(s); ok {
			if lo != hi {
				return "", fmt.Errorf("a single VLAN is required, not a range")
			}
			return s, nil
		}
		if s == "all" {
			return "", fmt.Errorf("a single VLAN is required")
		}
		return Identifier.Check(s)
	}}

	// IfPattern selects physical ports: "<m>/<c>/<p>" where each part is
	// a number, "*" or a range "[a-b]".
	IfPattern = &Type{Name: "<pattern>", Check: func(s string) (string, error) {
		if _, err := ParsePortPattern(s); err != nil {
			return "", err
		}
		return s, nil
	}}

	// TTY is a serial device name below /dev.
	TTY = String("<tty>", 32, `^tty[A-Za-z0-9]+$`)

	// MemberID is a stack member number.
	MemberID = Uint("<member-id>", 1, 16)
)

var aeRe = regexp.MustCompile(`^ae(0|[1-9][0-9]{0,3})$`)

// Port is a physical port name "<member>/<card>/<port>".
type Port struct{ Member, Card, Port int }

func (p Port) String() string { return fmt.Sprintf("%d/%d/%d", p.Member, p.Card, p.Port) }

// Card limits: enough for any chassis, small enough to keep names short.
// MaxUnit follows Junos (logical units 0-16385).
const (
	MaxCard = 99
	MaxPort = 999
	MaxUnit = 16385
)

// ParsePhysical parses "<member>/<card>/<port>". Leading zeros are
// accepted and normalised away.
func ParsePhysical(s string) (Port, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return Port{}, false
	}
	var v [3]int
	lim := [3]int{16, MaxCard, MaxPort}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || part == "" || part[0] == '+' || part[0] == '-' || len(part) > 3 || n < 0 || n > lim[i] {
			return Port{}, false
		}
		v[i] = n
	}
	if v[0] < 1 {
		return Port{}, false
	}
	return Port{v[0], v[1], v[2]}, true
}

// PortPattern selects physical ports; each part is a range (lo..hi).
type PortPattern [3][2]int

// ParsePortPattern parses "<m>/<c>/<p>" with numbers, "*" or "[a-b]".
func ParsePortPattern(s string) (PortPattern, error) {
	var pp PortPattern
	parts := strings.Split(s, "/")
	bad := fmt.Errorf("invalid port pattern %q (expecting <member>/<card>/<port>, each a number, * or [a-b], e.g. 1/0/*)", s)
	if len(parts) != 3 {
		return pp, bad
	}
	lim := [3][2]int{{1, 16}, {0, MaxCard}, {0, MaxPort}}
	for i, part := range parts {
		lo, hi := lim[i][0], lim[i][1]
		switch {
		case part == "*":
		case strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]"):
			a, b, ok := strings.Cut(part[1:len(part)-1], "-")
			x, err1 := strconv.Atoi(a)
			y, err2 := strconv.Atoi(b)
			if !ok || err1 != nil || err2 != nil || x > y || x < lo || y > hi {
				return pp, bad
			}
			lo, hi = x, y
		default:
			n, err := strconv.Atoi(part)
			if err != nil || n < lo || n > hi {
				return pp, bad
			}
			lo, hi = n, n
		}
		pp[i] = [2]int{lo, hi}
	}
	return pp, nil
}

// Match reports whether p is selected.
func (pp PortPattern) Match(p Port) bool {
	v := [3]int{p.Member, p.Card, p.Port}
	for i := range v {
		if v[i] < pp[i][0] || v[i] > pp[i][1] {
			return false
		}
	}
	return true
}

// IsAE reports whether s names an aggregated interface.
func IsAE(s string) bool { return aeRe.MatchString(s) }

// CheckInterfaceName validates a physical, aggregated or irb interface
// name.
func CheckInterfaceName(s string) (string, error) {
	if IsAE(s) || s == "irb" {
		return s, nil
	}
	if p, ok := ParsePhysical(s); ok {
		return p.String(), nil
	}
	return "", fmt.Errorf("invalid interface name %q (expecting <member>/<card>/<port> like 1/0/0, ae<N> or irb)", s)
}

// ParseVlanRange parses "10" or "10-20".
func ParseVlanRange(s string) (lo, hi int, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	l, err := strconv.Atoi(a)
	if err != nil || l < 1 || l > 4094 || a != strconv.Itoa(l) {
		return 0, 0, false
	}
	if !isRange {
		return l, l, true
	}
	h, err := strconv.Atoi(b)
	if err != nil || h < l || h > 4094 || b != strconv.Itoa(h) {
		return 0, 0, false
	}
	return l, h, true
}

func checkIP(family int) func(string) (string, error) {
	return func(s string) (string, error) {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return "", fmt.Errorf("invalid IP address %q", s)
		}
		switch {
		case family == 4 && !a.Is4():
			return "", fmt.Errorf("%q is not an IPv4 address", s)
		case family == 6 && !a.Is6():
			return "", fmt.Errorf("%q is not an IPv6 address", s)
		}
		return a.String(), nil
	}
}

func checkPrefix(s string) (string, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return "", fmt.Errorf("invalid address/prefix %q", s)
	}
	return p.String(), nil
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
