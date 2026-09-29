//go:build linux

package dataplane

import "testing"

func TestBurstTicks(t *testing.T) {
	// 100 pps, 16 packets = 160 ms = 2.5e6 ticks of 64 ns.
	if got := burstTicks(100); got != 2500000 {
		t.Errorf("burstTicks(100) = %d", got)
	}
	// 100000 pps, 10000 packets = 100 ms.
	if got := burstTicks(100000); got != 1562500 {
		t.Errorf("burstTicks(100000) = %d", got)
	}
}
