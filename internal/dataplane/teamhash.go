package dataplane

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/net/bpf"
)

// Transmit hash of LACP bundles (team devices): a classic BPF program that
// returns a flow hash; the team driver sends a frame on port
// hash % distributing ports. Frames of one flow always take the same port,
// so their order is kept (reference 5.1.3, hash-policy). Non-IP frames
// always use layer 2; the hash is symmetric (both directions of a flow
// hash the same).

// asm assembles BPF with symbolic jump targets.
type asm struct {
	ins    []bpf.Instruction
	labels map[string]int
	fixes  []asmFix
}

type asmFix struct {
	at   int
	t, f string // conditional targets ("" = next instruction); t alone for Jump
}

func (a *asm) emit(is ...bpf.Instruction) { a.ins = append(a.ins, is...) }
func (a *asm) label(n string)             { a.labels[n] = len(a.ins) }

func (a *asm) jif(cond bpf.JumpTest, val uint32, t, f string) {
	a.fixes = append(a.fixes, asmFix{len(a.ins), t, f})
	a.emit(bpf.JumpIf{Cond: cond, Val: val})
}

func (a *asm) jump(t string) {
	a.fixes = append(a.fixes, asmFix{at: len(a.ins), t: t})
	a.emit(bpf.Jump{})
}

func (a *asm) resolve() []bpf.Instruction {
	skip := func(at int, l string) uint32 {
		if l == "" {
			return 0
		}
		to, ok := a.labels[l]
		if !ok || to <= at {
			panic("bpf: bad label " + l)
		}
		return uint32(to - at - 1)
	}
	for _, f := range a.fixes {
		switch j := a.ins[f.at].(type) {
		case bpf.JumpIf:
			j.SkipTrue, j.SkipFalse = uint8(skip(f.at, f.t)), uint8(skip(f.at, f.f))
			a.ins[f.at] = j
		case bpf.Jump:
			j.Skip = skip(f.at, f.t)
			a.ins[f.at] = j
		}
	}
	return a.ins
}

// xorWords xors the 32-bit words at the offsets into A.
func (a *asm) xorWords(size int, offs ...uint32) {
	for i, o := range offs {
		if i == 0 {
			a.emit(bpf.LoadAbsolute{Off: o, Size: size})
			continue
		}
		a.emit(bpf.TAX{}, bpf.LoadAbsolute{Off: o, Size: size}, bpf.ALUOpX{Op: bpf.ALUOpXor})
	}
}

// ports16 turns A = sport<<16 | dport into sport ^ dport (symmetric).
func (a *asm) ports16() {
	a.emit(bpf.StoreScratch{Src: bpf.RegA, N: 1}, bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 16}, bpf.TAX{},
		bpf.LoadScratch{Dst: bpf.RegA, N: 1}, bpf.ALUOpX{Op: bpf.ALUOpXor}, bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xffff})
}

// mix xors A into scratch word 0.
func (a *asm) mix() {
	a.emit(bpf.TAX{}, bpf.LoadScratch{Dst: bpf.RegA, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpXor}, bpf.StoreScratch{Src: bpf.RegA, N: 0})
}

const (
	ipProtoTCP = 6
	ipProtoUDP = 17
)

// hashProgram returns the transmit hash for a hash policy (layer2,
// layer2+3, layer3+4).
func hashProgram(policy string) ([]bpf.Instruction, error) {
	a := &asm{labels: map[string]int{}}
	l2 := func() {
		a.xorWords(4, 0, 6) // first 4 bytes of both MACs
		a.mix()
		a.xorWords(2, 4, 10) // last 2 bytes
		a.mix()
	}
	a.emit(bpf.LoadConstant{Dst: bpf.RegA, Val: 0}, bpf.StoreScratch{Src: bpf.RegA, N: 0})
	switch policy {
	case "layer2":
		l2()
	case "layer2+3", "layer3+4":
		ports := policy == "layer3+4"
		if !ports {
			l2()
		}
		a.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
		a.jif(bpf.JumpEqual, 0x0800, "v4", "")
		a.jif(bpf.JumpEqual, 0x86dd, "v6", "nonip")
		a.label("v4")
		a.xorWords(4, 26, 30)
		a.mix()
		if ports {
			a.emit(bpf.LoadAbsolute{Off: 23, Size: 1})
			a.jif(bpf.JumpEqual, ipProtoTCP, "p4", "")
			a.jif(bpf.JumpEqual, ipProtoUDP, "p4", "out")
			a.label("p4")
			a.emit(bpf.LoadAbsolute{Off: 20, Size: 2})
			a.jif(bpf.JumpBitsSet, 0x1fff, "out", "") // a fragment: no ports
			a.emit(bpf.LoadMemShift{Off: 14}, bpf.LoadIndirect{Off: 14, Size: 4})
			a.ports16()
			a.mix()
		}
		a.jump("out")
		a.label("v6")
		a.xorWords(4, 22, 26, 30, 34, 38, 42, 46, 50)
		a.mix()
		if ports {
			a.emit(bpf.LoadAbsolute{Off: 20, Size: 1})
			a.jif(bpf.JumpEqual, ipProtoTCP, "p6", "")
			a.jif(bpf.JumpEqual, ipProtoUDP, "p6", "out")
			a.label("p6")
			a.emit(bpf.LoadAbsolute{Off: 54, Size: 4})
			a.ports16()
			a.mix()
		}
		a.jump("out")
		a.label("nonip")
		if ports {
			l2()
		}
	default:
		return nil, fmt.Errorf("unknown hash policy %q", policy)
	}
	a.label("out")
	// Fold the high bits in: the driver uses hash % ports.
	a.emit(bpf.LoadScratch{Dst: bpf.RegA, N: 0}, bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 16}, bpf.TAX{},
		bpf.LoadScratch{Dst: bpf.RegA, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpXor}, bpf.StoreScratch{Src: bpf.RegA, N: 0},
		bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 8}, bpf.TAX{},
		bpf.LoadScratch{Dst: bpf.RegA, N: 0}, bpf.ALUOpX{Op: bpf.ALUOpXor}, bpf.RetA{})
	return a.resolve(), nil
}

// hashProgramBytes is the program as the kernel takes it (struct
// sock_filter array, host byte order).
func hashProgramBytes(policy string) ([]byte, error) {
	prog, err := hashProgram(policy)
	if err != nil {
		return nil, err
	}
	raw, err := bpf.Assemble(prog)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, r := range raw {
		binary.Write(&b, binary.NativeEndian, r.Op)
		b.WriteByte(r.Jt)
		b.WriteByte(r.Jf)
		binary.Write(&b, binary.NativeEndian, r.K)
	}
	return b.Bytes(), nil
}

// hashPolicyOf recognises one of our programs ("" if it is none of them).
func hashPolicyOf(prog []byte) string {
	for _, p := range []string{"layer2", "layer2+3", "layer3+4"} {
		if b, _ := hashProgramBytes(p); bytes.Equal(b, prog) {
			return p
		}
	}
	return ""
}
