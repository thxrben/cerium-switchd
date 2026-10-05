package daemon

import (
	"fmt"
	"testing"
)

func TestLACPPortNumber(t *testing.T) {
	for name, want := range map[string]uint16{
		"1/0/0": 1024, "1/0/63": 1087, "1/1/0": 1088, "1/15/63": 2047, "2/0/0": 2048,
		"16/15/63": 17407, "1/16/0": 0, "1/0/64": 0, "ae1": 0,
	} {
		if got := lacpPortNumber(name); got != want {
			t.Errorf("lacpPortNumber(%s) = %d, want %d", name, got, want)
		}
	}
	seen := map[uint16]string{}
	for m := 1; m <= 16; m++ {
		for c := 0; c <= 15; c++ {
			for p := 0; p <= 63; p++ {
				name := fmt.Sprintf("%d/%d/%d", m, c, p)
				n := lacpPortNumber(name)
				if prev, dup := seen[n]; dup || n == 0 {
					t.Fatalf("%s: number %d (also %s)", name, n, prev)
				}
				seen[n] = name
			}
		}
	}
}
