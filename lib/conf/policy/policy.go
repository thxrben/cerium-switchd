// Package policy evaluates routing policies (reference 5.11) on routes:
// the Junos evaluation of terms and policy chains, the match conditions
// and the actions. It is pure and used by OSPF export and BGP import and
// export.
package policy

import (
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

// Route is what a policy sees of a route and may change.
type Route struct {
	Prefix   netip.Prefix
	Protocol string // direct, local, static, ospf, ospf3, bgp, aggregate
	Neighbor netip.Addr
	Area     netip.Addr // OSPF
	Tag      uint32     // OSPF external tag

	// BGP attributes.
	ASPath      []uint32
	Communities []string // "65000:1", "large:1:2:3", well-known names
	LocalPref   uint32
	MED         uint32
	HasMED      bool

	// Results of actions.
	Metric       uint32 // MED / OSPF external metric
	HasMetric    bool
	Preference   int // -1: unchanged
	NextHop      string
	ExternalType int
}

// Result of an evaluation.
type Result int

const (
	// Default: no policy accepted or rejected; the protocol's default
	// policy decides.
	Default Result = iota
	Accept
	Reject
)

// Engine evaluates policies of one configuration (compiled once).
type Engine struct {
	p       *model.Policies
	asPaths map[string]*regexp.Regexp
	commRE  map[string]*regexp.Regexp
}

// New compiles the policies of a configuration.
func New(p *model.Policies) *Engine {
	e := &Engine{p: p, asPaths: map[string]*regexp.Regexp{}, commRE: map[string]*regexp.Regexp{}}
	if p == nil {
		e.p = &model.Policies{}
		return e
	}
	for n, expr := range p.ASPaths {
		if re, err := model.CompileASPath(expr); err == nil {
			e.asPaths[n] = re
		}
	}
	for _, ms := range p.Communities {
		for _, m := range ms {
			if isCommunityRE(m) {
				if re, err := regexp.Compile(m); err == nil {
					e.commRE[m] = re
				}
			}
		}
	}
	return e
}

func isCommunityRE(m string) bool {
	return strings.ContainsAny(m, "^$.*+?[](){}|\\") && !strings.HasPrefix(m, "large:")
}

// Evaluate runs the policy chain on r (changing r by the actions) and
// returns the result. Unknown policies are skipped (commit check refuses
// them).
func (e *Engine) Evaluate(chain []string, r *Route) Result {
	for _, name := range chain {
		ps := e.p.Statements[name]
		if ps == nil {
			continue
		}
		switch res, next := e.evalPolicy(ps, r); {
		case next:
			continue
		default:
			return res
		}
	}
	return Default
}

// evalPolicy returns the terminating result, or next=true to continue
// with the next policy.
func (e *Engine) evalPolicy(ps *model.PolicyStatement, r *Route) (Result, bool) {
	for _, t := range ps.Terms {
		if !e.matches(&t.From, r) {
			continue
		}
		e.apply(&t.Then, r)
		switch t.Then.Flow {
		case "accept":
			return Accept, false
		case "reject":
			return Reject, false
		case "next policy":
			return Default, true
		}
		// next term (default)
	}
	switch ps.Final {
	case "accept":
		return Accept, false
	case "reject":
		return Reject, false
	}
	return Default, true
}

func (e *Engine) matches(f *model.PolicyFrom, r *Route) bool {
	if len(f.Protocols) > 0 && !slices.Contains(f.Protocols, r.Protocol) {
		return false
	}
	if f.Family != "" && (f.Family == "inet") != r.Prefix.Addr().Is4() {
		return false
	}
	if f.Tag != nil && *f.Tag != r.Tag {
		return false
	}
	if len(f.Neighbors) > 0 && !slices.Contains(f.Neighbors, r.Neighbor) {
		return false
	}
	if len(f.Areas) > 0 && !slices.Contains(f.Areas, r.Area) {
		return false
	}
	// Prefix conditions: route filters, prefix lists and prefix list
	// filters together match when any of them matches (Junos).
	if len(f.RouteFilters) > 0 || len(f.PrefixLists) > 0 || len(f.PrefixListFilters) > 0 {
		ok := false
		for _, rf := range f.RouteFilters {
			if RouteFilterMatch(rf, r.Prefix) {
				ok = true
			}
		}
		for _, l := range f.PrefixLists {
			for _, p := range e.p.PrefixLists[l] {
				if p == r.Prefix {
					ok = true
				}
			}
		}
		for _, l := range f.PrefixListFilters {
			for _, p := range e.p.PrefixLists[l.List] {
				if RouteFilterMatch(model.RouteFilter{Prefix: p, Match: l.Match}, r.Prefix) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	for _, c := range f.Communities {
		if !e.hasCommunity(c, r) {
			return false
		}
	}
	if len(f.ASPaths) > 0 {
		ok := false
		for _, a := range f.ASPaths {
			if re := e.asPaths[a]; re != nil && model.MatchASPath(re, r.ASPath) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// RouteFilterMatch reports whether p matches the route filter.
func RouteFilterMatch(rf model.RouteFilter, p netip.Prefix) bool {
	if rf.Prefix.Addr().Is4() != p.Addr().Is4() || p.Bits() < rf.Prefix.Bits() || !rf.Prefix.Contains(p.Addr()) {
		return false
	}
	switch rf.Match {
	case "exact":
		return p.Bits() == rf.Prefix.Bits()
	case "orlonger":
		return true
	case "longer":
		return p.Bits() > rf.Prefix.Bits()
	case "range", "upto":
		return p.Bits() >= rf.Lo && p.Bits() <= rf.Hi
	}
	return false
}

// hasCommunity: the route carries every member of the named community.
func (e *Engine) hasCommunity(name string, r *Route) bool {
	ms := e.p.Communities[name]
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if re := e.commRE[m]; re != nil {
			if !slices.ContainsFunc(r.Communities, func(c string) bool { return !strings.HasPrefix(c, "large:") && re.MatchString(c) }) {
				return false
			}
			continue
		}
		if !slices.Contains(r.Communities, m) {
			return false
		}
	}
	return true
}

func (e *Engine) apply(t *model.PolicyThen, r *Route) {
	if t.Metric != nil {
		r.Metric, r.HasMetric = *t.Metric, true
	}
	if t.MetricAdd != nil {
		r.Metric, r.HasMetric = r.Metric+*t.MetricAdd, true
	}
	if t.LocalPref != nil {
		r.LocalPref = *t.LocalPref
	}
	if t.Preference != nil {
		r.Preference = *t.Preference
	}
	if t.SetCommunity {
		r.Communities = nil
		for _, n := range t.CommunitySet {
			r.Communities = appendLiteral(r.Communities, e.p.Communities[n])
		}
	}
	for _, n := range t.CommunityDelete {
		for _, m := range e.p.Communities[n] {
			re := e.commRE[m]
			r.Communities = slices.DeleteFunc(r.Communities, func(c string) bool {
				if re != nil {
					return !strings.HasPrefix(c, "large:") && re.MatchString(c)
				}
				return c == m
			})
		}
	}
	for _, n := range t.CommunityAdd {
		r.Communities = appendLiteral(r.Communities, e.p.Communities[n])
	}
	if len(t.ASPathPrepend) > 0 {
		r.ASPath = append(slices.Clone(t.ASPathPrepend), r.ASPath...)
	}
	if t.NextHop != "" {
		r.NextHop = t.NextHop
	}
	if t.ExternalType != 0 {
		r.ExternalType = t.ExternalType
	}
	if t.Tag != nil {
		r.Tag = *t.Tag
	}
}

// appendLiteral adds the literal (non-regex) members not yet present.
func appendLiteral(cs []string, ms []string) []string {
	for _, m := range ms {
		if isCommunityRE(m) || slices.Contains(cs, m) {
			continue
		}
		cs = append(cs, m)
	}
	return cs
}

// FormatCommunity formats a standard community value.
func FormatCommunity(v uint32) string {
	switch v {
	case 0xFFFFFF01:
		return "no-export"
	case 0xFFFFFF02:
		return "no-advertise"
	case 0xFFFFFF03:
		return "no-export-subconfed"
	}
	return strconv.Itoa(int(v>>16)) + ":" + strconv.Itoa(int(v&0xffff))
}

// ParseCommunity reads a standard community ("65000:1" or a well-known
// name); ok is false for large communities and expressions.
func ParseCommunity(s string) (uint32, bool) {
	switch s {
	case "no-export":
		return 0xFFFFFF01, true
	case "no-advertise":
		return 0xFFFFFF02, true
	case "no-export-subconfed":
		return 0xFFFFFF03, true
	}
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	x, e1 := strconv.ParseUint(a, 10, 16)
	y, e2 := strconv.ParseUint(b, 10, 16)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	return uint32(x)<<16 | uint32(y), true
}
