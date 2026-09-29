package daemon

import (
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"

	"mclag/internal/schema"
)

// legacyName is the interface name form before reference 1.6:
// "<member>/<linux-name>".
var legacyName = regexp.MustCompile(`^([0-9]{1,2}|\*)/([A-Za-z][A-Za-z0-9._@*?-]{0,14})$`)

// portNames is what the upgrade needs from the port numbering.
type portNames interface {
	Name(linux string) (string, bool)
	LinuxNames() []string
}

// upgradeNames returns a store upgrade (commit.Options.Upgrade) that
// converts old interface names of this member into "<member>/<card>/<port>"
// (reference 1.6). Names of ports that do not exist, and of other members,
// are left alone; commit check reports them.
func upgradeNames(names portNames, member int) func(json.RawMessage) json.RawMessage {
	me := strconv.Itoa(member)
	conv := func(v string) string {
		m := legacyName.FindStringSubmatch(v)
		if m == nil || m[1] != me {
			return v
		}
		if n, ok := names.Name(m[2]); ok {
			return n
		}
		return v
	}
	// expand turns an old wildcard ("1/ens*") into the matching ports.
	expand := func(v string) []string {
		m := legacyName.FindStringSubmatch(v)
		if m == nil || (m[1] != me && m[1] != "*") {
			return []string{v}
		}
		var out []string
		for _, l := range names.LinuxNames() {
			if ok, _ := path.Match(m[2], l); ok {
				n, _ := names.Name(l)
				out = append(out, n)
			}
		}
		if len(out) == 0 {
			return []string{v}
		}
		return out
	}
	isIf := func(t *schema.Type) bool {
		return t != nil && (t.Ref == "interface" || t.Ref == "physical-interface")
	}
	var walk func(sn *schema.Node, m map[string]any, stackMember string)
	walk = func(sn *schema.Node, m map[string]any, stackMember string) {
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
					walk(c, sub, stackMember)
				}
			case schema.List:
				entries, ok := v.(map[string]any)
				if !ok {
					continue
				}
				for key, e := range entries {
					sub, _ := e.(map[string]any)
					sm := stackMember
					if c.Type == schema.MemberID {
						sm = key
					}
					if sub != nil {
						walk(c, sub, sm)
					}
					if isIf(c.Type) {
						if nk := conv(key); nk != key {
							if _, taken := entries[nk]; !taken {
								delete(entries, key)
								entries[nk] = e
							}
						}
					}
				}
			case schema.Leaf:
				s, ok := v.(string)
				if !ok || !isIf(c.Type) {
					continue
				}
				if !strings.Contains(s, "/") && stackMember != "" {
					// stack member <id> management|underlay interface <linux-name>
					if stackMember == me {
						if n, ok := names.Name(s); ok {
							m[name] = n
						}
					}
					continue
				}
				m[name] = conv(s)
			case schema.LeafList:
				arr, ok := v.([]any)
				if !ok {
					continue
				}
				var out []any
				for _, x := range arr {
					s, ok := x.(string)
					switch {
					case !ok:
						out = append(out, x)
					case c.Type == schema.IfPattern:
						if _, err := schema.ParsePortPattern(s); err == nil {
							out = append(out, s)
							continue
						}
						for _, e := range expand(s) {
							out = append(out, e)
						}
					case isIf(c.Type):
						out = append(out, conv(s))
					default:
						out = append(out, s)
					}
				}
				m[name] = out
			}
		}
	}
	return func(raw json.RawMessage) json.RawMessage {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return raw
		}
		walk(schema.Root(), m, "")
		out, err := json.Marshal(m)
		if err != nil {
			return raw
		}
		return out
	}
}
