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

	// Interface is a switch interface name: "<member>/<linux-name>" for
	// physical ports or "ae<N>" for aggregated interfaces.
	Interface = &Type{Name: "<interface-name>", Ref: "interface", Check: CheckInterfaceName}

	// PhysInterface only accepts "<member>/<linux-name>".
	PhysInterface = &Type{Name: "<interface-name>", Ref: "physical-interface", Check: func(s string) (string, error) {
		if _, _, ok := SplitPhysical(s); !ok {
			return "", fmt.Errorf("invalid physical interface name %q (expecting <member>/<name>, e.g. 1/eth0)", s)
		}
		return s, nil
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

	// LinuxIfName is a raw kernel interface name.
	LinuxIfName = String("<linux-interface>", 15, `^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

	// TTY is a serial device name below /dev.
	TTY = String("<tty>", 32, `^tty[A-Za-z0-9]+$`)

	// MemberID is a stack member number.
	MemberID = Uint("<member-id>", 1, 16)
)

var (
	aeRe   = regexp.MustCompile(`^ae(0|[1-9][0-9]{0,3})$`)
	physRe = regexp.MustCompile(`^([1-9][0-9]?)/([A-Za-z0-9][A-Za-z0-9._@-]{0,14})$`)
)

// SplitPhysical splits "<member>/<linux-name>".
func SplitPhysical(s string) (member int, linux string, ok bool) {
	m := physRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > 16 {
		return 0, "", false
	}
	return n, m[2], true
}

// IsAE reports whether s names an aggregated interface.
func IsAE(s string) bool { return aeRe.MatchString(s) }

// CheckInterfaceName validates a physical or aggregated interface name.
func CheckInterfaceName(s string) (string, error) {
	if IsAE(s) {
		return s, nil
	}
	if _, _, ok := SplitPhysical(s); ok {
		return s, nil
	}
	return "", fmt.Errorf("invalid interface name %q (expecting <member>/<name> like 1/eth0, or ae<N>)", s)
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
