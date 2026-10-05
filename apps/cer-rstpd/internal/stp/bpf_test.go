//go:build linux

package stp

import (
	"encoding/binary"
	"testing"
)

// runBPF interprets the subset of classic BPF the filter uses.
func runBPF(prog []struct {
	code   uint16
	jt, jf uint8
	k      uint32
}, pkt []byte) uint32 {
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.code {
		case 0x20:
			a = binary.BigEndian.Uint32(pkt[in.k:])
		case 0x28:
			a = uint32(binary.BigEndian.Uint16(pkt[in.k:]))
		case 0x15:
			if a == in.k {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case 0x06:
			return in.k
		}
	}
	return 0
}

func TestBPDUFilter(t *testing.T) {
	var prog []struct {
		code   uint16
		jt, jf uint8
		k      uint32
	}
	for _, f := range bpduFilter {
		prog = append(prog, struct {
			code   uint16
			jt, jf uint8
			k      uint32
		}{f.Code, f.Jt, f.Jf, f.K})
	}
	frame := func(dst ...byte) []byte { return append(dst, make([]byte, 60)...) }
	cases := map[string]struct {
		pkt    []byte
		accept bool
	}{
		"IEEE BPDU":   {frame(0x01, 0x80, 0xc2, 0, 0, 0), true},
		"Cisco PVST+": {frame(0x01, 0x00, 0x0c, 0xcc, 0xcc, 0xcd), true},
		"LACP":        {frame(0x01, 0x80, 0xc2, 0, 0, 2), false},
		"LLDP":        {frame(0x01, 0x80, 0xc2, 0, 0, 0x0e), false},
		"Cisco CDP":   {frame(0x01, 0x00, 0x0c, 0xcc, 0xcc, 0xcc), false},
		"unicast":     {frame(0x02, 0, 0, 0, 0, 1), false},
	}
	for name, c := range cases {
		if got := runBPF(prog, c.pkt) != 0; got != c.accept {
			t.Errorf("%s: accepted %v", name, got)
		}
	}
}
