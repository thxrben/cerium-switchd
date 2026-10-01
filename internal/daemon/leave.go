package daemon

import (
	"log/slog"
	"strconv"
	"strings"

	"mclag/internal/schema"
)

// standalone rewrites the configuration of a member that was removed from
// its stack, so that it runs on its own as member 1 (reference 5.2,
// removal): the ports of the old member number get number 1, everything
// that belongs to other members goes, and the stacking and MC-LAG
// statements are removed. The rest (VLANs, users, routing, …) stays.
type standalone struct {
	from string
	log  *slog.Logger
}

// Rewrite converts the configuration JSON tree m in place.
func (s *standalone) Rewrite(m map[string]any) {
	delete(m, "virtual-chassis")
	delete(m, "mclag")
	s.walk(schema.Root(), m)
}

// conv renames "<from>/<card>/<port>" to "1/<card>/<port>"; ok is false for
// ports of other members.
func (s *standalone) conv(v string) (string, bool) {
	p, ok := schema.ParsePhysical(v)
	if !ok {
		return v, true // ae, irb or unknown: the parser decides
	}
	if strconv.Itoa(p.Member) != s.from {
		return v, false
	}
	p.Member = 1
	return p.String(), true
}

func (s *standalone) convUnit(v string) (string, bool) {
	i := strings.LastIndexByte(v, '.')
	if i <= 0 {
		return v, true
	}
	n, ok := s.conv(v[:i])
	return n + v[i:], ok
}

// convPattern converts "<member>/<card>/<port>" patterns: "*" and ranges
// that contain the old member stay (as "*" or the member 1).
func (s *standalone) convPattern(v string) (string, bool) {
	first, rest, ok := strings.Cut(v, "/")
	if !ok {
		return v, true
	}
	switch {
	case first == "*":
		return v, true
	case first == s.from:
		return "1/" + rest, true
	case strings.HasPrefix(first, "[") && strings.HasSuffix(first, "]"):
		lo, hi, ok := strings.Cut(first[1:len(first)-1], "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		me, _ := strconv.Atoi(s.from)
		if ok && err1 == nil && err2 == nil && a <= me && me <= b {
			return "1/" + rest, true
		}
	}
	return v, false
}

func (s *standalone) dropped(what string) {
	if s.log == nil {
		return
	}
	s.log.Info("left the virtual chassis: statement of another member removed", "statement", what)
}

func (s *standalone) walk(sn *schema.Node, m map[string]any) {
	for name, v := range m {
		if strings.HasPrefix(name, "@") {
			continue
		}
		c := sn.Child(name)
		if c == nil {
			continue
		}
		switch c.Kind {
		case schema.Container:
			if sub, ok := v.(map[string]any); ok {
				s.walk(c, sub)
			}
		case schema.List:
			entries, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for key, e := range entries {
				sub, _ := e.(map[string]any)
				if isIf(c.Type) {
					nk, ok := s.conv(key)
					if !ok {
						delete(entries, key)
						s.dropped(name + " " + key)
						continue
					}
					if nk != key {
						delete(entries, key)
						entries[nk] = e
					}
				}
				if sub == nil {
					continue
				}
				// An address that belongs to one member (irb units).
				if who, ok := sub["member"].(string); ok && c.Child("member") != nil {
					if who != s.from {
						delete(entries, key)
						s.dropped(name + " " + key + " member " + who)
						continue
					}
					delete(sub, "member")
				}
				s.walk(c, sub)
			}
		case schema.Leaf:
			str, ok := v.(string)
			if !ok || !isIf(c.Type) {
				continue
			}
			if n, ok := s.conv(str); ok {
				m[name] = n
			} else {
				delete(m, name)
				s.dropped(name + " " + str)
			}
		case schema.LeafList:
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			var out []any
			for _, x := range arr {
				str, ok := x.(string)
				var n string
				keep := true
				switch {
				case !ok:
					out = append(out, x)
					continue
				case c.Type == schema.IfPattern:
					n, keep = s.convPattern(str)
				case c.Type == schema.UnitName:
					n, keep = s.convUnit(str)
				case isIf(c.Type):
					n, keep = s.conv(str)
				default:
					n = str
				}
				if keep {
					out = append(out, n)
				} else {
					s.dropped(name + " " + str)
				}
			}
			m[name] = out
		}
	}
}
