package daemon

import (
	"slices"
	"testing"
)

func TestCutPairs(t *testing.T) {
	ring := map[int][]int{1: {2, 3}, 2: {1, 3}, 3: {1, 2}}
	if got := cutPairs(ring, 1, []int{2, 3}); got != nil {
		t.Errorf("ring: %v", got)
	}
	chain := map[int][]int{1: {2}, 2: {1, 3}, 3: {2}}
	if got := cutPairs(chain, 2, []int{1, 3}); !slices.Equal(got, []string{"1-3"}) {
		t.Errorf("chain through 2: %v", got)
	}
	if got := cutPairs(chain, 1, []int{2, 3}); got != nil {
		t.Errorf("chain end: %v", got)
	}
}
