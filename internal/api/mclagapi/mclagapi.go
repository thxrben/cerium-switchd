// Package mclagapi is the API of cer-mclagd: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package mclagapi

import "time"

// Config is what the controller runs (computed by switchd from the
// configuration).
type Config struct {
	Member int `json:"member"`
	// Pair is this member's MC-LAG pair (nil: no MC-LAG bundle has a leg
	// here).
	Pair *Pair `json:"pair,omitempty"`
	// Priority is each member's mastership priority (the primary of a
	// pair: the higher, ties: the lower id).
	Priority      map[int]int `json:"priority,omitempty"`
	SwitchMembers []int       `json:"switch_members,omitempty"`
	DelayRestore  int         `json:"delay_restore"`
	// Facts: per bundle what both members must agree on (consistency).
	Facts map[string]string `json:"facts,omitempty"`
}

// Pair is an MC-LAG pair.
type Pair struct {
	ID      int      `json:"id"`
	Members [2]int   `json:"members"`
	Bundles []string `json:"bundles"` // sorted
}

// Peer returns the other member of the pair.
func (p *Pair) Peer(member int) int {
	if p.Members[0] == member {
		return p.Members[1]
	}
	return p.Members[0]
}

// Status is "show mclag" of one pair (this member's view).
type Status struct {
	Pair, Member, Peer int
	Primary            bool
	PeerReachable      bool // over the stack (and so its stack tunnel)
	PeerKnown          bool // leg states received from the peer
	PeerSeen           time.Time
	Reach, Members     int // switch members reached (itself included) / in the stack
	Bundles            []Bundle
}

// Bundle is one MC-LAG bundle of Status.
type Bundle struct {
	Name                       string
	LocalUp, PeerUp, PeerKnown bool
	SplitHorizon               bool
	Hold                       string // reason ("": not held)
	Facts, PeerFacts           string
	DiffersSince               time.Time // zero: consistent
}

// RejoinAfter: how long legs held for the minority rule (or maintenance
// mode) wait after the peer is reachable again (the MAC tables are
// exchanged at once).
const RejoinAfter = 2 * time.Second
