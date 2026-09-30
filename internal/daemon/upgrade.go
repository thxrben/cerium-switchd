package daemon

import (
	"encoding/json"
	"log/slog"
	"net/netip"
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

// upgrader converts stored configurations of older versions (the JSON,
// before it is parsed), so that revisions, rollbacks and the shared
// candidate stay readable:
//   - "stack" became "virtual-chassis" (Junos VC names),
//   - interface names "<member>/<linux-name>" became "<member>/<card>/<port>",
//   - "virtual-chassis member <id> management" became routed interfaces in
//     routing instance mgmt_junos (reference 5.9),
//   - address leaf-lists became address entries (with an optional member),
//   - the MC-LAG peer-link, peer-link-bfd and heartbeat were replaced by the
//     stack tunnels (reference 5.6): the statements and the bundle that
//     served as peer-link are removed.
//
// A name that cannot be converted (its port no longer exists) would make the
// whole configuration unreadable, so that statement is dropped and logged.
type upgrader struct {
	names portNames
	me    string
	log   *slog.Logger
}

func newUpgrader(names portNames, member int, log *slog.Logger) *upgrader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &upgrader{names: names, me: strconv.Itoa(member), log: log}
}

// Upgrade implements commit.Options.Upgrade.
func (u *upgrader) Upgrade(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	renameStack(m)
	normalizeAddresses(m)
	u.convertManagement(m)
	u.removePeerLink(m)
	u.walk(schema.Root(), m, "")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// conv converts an old name of this member; ok is false if it is an old
// name that cannot be converted.
func (u *upgrader) conv(v string) (string, bool) {
	if _, ok := schema.ParsePhysical(v); ok || schema.IsAE(v) || v == "irb" {
		return v, true
	}
	m := legacyName.FindStringSubmatch(v)
	if m == nil {
		return v, true // not a name this upgrade knows; the parser decides
	}
	if m[1] == u.me {
		if n, ok := u.names.Name(m[2]); ok {
			return n, true
		}
	}
	return v, false
}

// convUnit converts "<old-name>.<unit>".
func (u *upgrader) convUnit(v string) (string, bool) {
	i := strings.LastIndexByte(v, '.')
	if i <= 0 {
		return v, true
	}
	n, ok := u.conv(v[:i])
	return n + v[i:], ok
}

// expand turns an old wildcard ("1/ens*") into the matching ports.
func (u *upgrader) expand(v string) ([]string, bool) {
	m := legacyName.FindStringSubmatch(v)
	if m == nil || (m[1] != u.me && m[1] != "*") {
		return nil, false
	}
	var out []string
	for _, l := range u.names.LinuxNames() {
		if ok, _ := path.Match(m[2], l); ok {
			n, _ := u.names.Name(l)
			out = append(out, n)
		}
	}
	return out, len(out) > 0
}

// replaced logs a statement removed because the stack tunnels replace it.
func (u *upgrader) replaced(what string) {
	u.log.Warn("stored configuration: statement removed, the stack tunnels replace the MC-LAG peer-link", "statement", what)
}

func (u *upgrader) dropped(what string) {
	u.log.Warn("stored configuration: statement for a port that no longer exists removed", "statement", what)
}

func isIf(t *schema.Type) bool {
	return t != nil && (t.Ref == "interface" || t.Ref == "physical-interface")
}

// walk converts interface names everywhere the schema says a value or key
// is an interface.
func (u *upgrader) walk(sn *schema.Node, m map[string]any, stackMember string) {
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
				u.walk(c, sub, stackMember)
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
					u.walk(c, sub, sm)
				}
				if !isIf(c.Type) {
					continue
				}
				nk, ok := u.conv(key)
				switch {
				case !ok:
					delete(entries, key)
					u.dropped(name + " " + key)
				case nk != key:
					if _, taken := entries[nk]; !taken {
						delete(entries, key)
						entries[nk] = e
					}
				}
			}
		case schema.Leaf:
			s, ok := v.(string)
			if !ok || !isIf(c.Type) {
				continue
			}
			if !strings.Contains(s, "/") && stackMember != "" && !schema.IsAE(s) {
				// virtual-chassis member <id> underlay interface <linux-name>
				s = stackMember + "/" + s
			}
			if n, ok := u.conv(s); ok {
				m[name] = n
			} else {
				delete(m, name)
				u.dropped(name + " " + s)
			}
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
					} else if ex, ok := u.expand(s); ok {
						for _, e := range ex {
							out = append(out, e)
						}
					} else {
						u.dropped(name + " " + s)
					}
				case c.Type == schema.UnitName:
					if n, ok := u.convUnit(s); ok {
						out = append(out, n)
					} else {
						u.dropped(name + " " + s)
					}
				case isIf(c.Type):
					if n, ok := u.conv(s); ok {
						out = append(out, n)
					} else {
						u.dropped(name + " " + s)
					}
				default:
					out = append(out, s)
				}
			}
			m[name] = out
		}
	}
}

// renameStack converts the stack hierarchy of older versions to the Junos
// Virtual Chassis names: "stack" -> "virtual-chassis", member "priority"
// -> "mastership-priority" (reference 5.2).
func renameStack(m map[string]any) {
	st, ok := m["stack"]
	if !ok {
		return
	}
	if _, taken := m["virtual-chassis"]; taken {
		return
	}
	delete(m, "stack")
	m["virtual-chassis"] = st
	if v, ok := m["@inactive:stack"]; ok {
		delete(m, "@inactive:stack")
		m["@inactive:virtual-chassis"] = v
	}
	vc, _ := st.(map[string]any)
	members, _ := vc["member"].(map[string]any)
	for _, e := range members {
		if mem, ok := e.(map[string]any); ok {
			for _, k := range []string{"priority", "@inactive:priority"} {
				if p, ok := mem[k]; ok {
					delete(mem, k)
					mem[strings.Replace(k, "priority", "mastership-priority", 1)] = p
				}
			}
		}
	}
}

// obj returns the object at a path below m, creating it.
func obj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	return m
}

// normalizeAddresses turns address leaf-lists (older versions) into address
// entries.
func normalizeAddresses(m map[string]any) {
	ifs, _ := m["interfaces"].(map[string]any)
	for _, i := range ifs {
		io, ok := i.(map[string]any)
		if !ok {
			continue
		}
		units, _ := io["unit"].(map[string]any)
		for _, un := range units {
			uo, ok := un.(map[string]any)
			if !ok {
				continue
			}
			fam, _ := uo["family"].(map[string]any)
			for _, f := range []string{"inet", "inet6"} {
				fo, _ := fam[f].(map[string]any)
				if arr, ok := fo["address"].([]any); ok {
					addrs := map[string]any{}
					for _, a := range arr {
						if s, ok := a.(string); ok {
							addrs[s] = map[string]any{}
						}
					}
					fo["address"] = addrs
				}
			}
		}
	}
}

// convertManagement turns "virtual-chassis member <id> management { … }"
// into routed interfaces in routing instance mgmt_junos.
func (u *upgrader) convertManagement(m map[string]any) {
	vc, _ := m["virtual-chassis"].(map[string]any)
	members, _ := vc["member"].(map[string]any)
	for id, e := range members {
		mem, _ := e.(map[string]any)
		mg, ok := mem["management"].(map[string]any)
		if !ok {
			continue
		}
		delete(mem, "management")
		strs := func(k string) []string {
			var out []string
			arr, _ := mg[k].([]any)
			for _, a := range arr {
				if s, ok := a.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		addrs, gws := strs("address"), strs("gateway")
		dhcp, _ := mg["dhcp"].(bool)
		var unit string
		addAddr := func(ifname, unitNum string, a string, member bool) {
			fam := "inet"
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is6() {
				fam = "inet6"
			}
			entry := map[string]any{}
			if member {
				entry["member"] = id
			}
			obj(m, "interfaces", ifname, "unit", unitNum, "family", fam, "address")[a] = entry
		}
		switch {
		case mg["interface"] != nil:
			p, _ := mg["interface"].(string)
			if !strings.Contains(p, "/") {
				p = id + "/" + p
			}
			name, ok := u.conv(p)
			if !ok {
				u.dropped("virtual-chassis member " + id + " management interface " + p)
				continue
			}
			for _, a := range addrs {
				addAddr(name, "0", a, false)
			}
			if dhcp {
				obj(m, "interfaces", name, "unit", "0", "family", "inet")["dhcp"] = true
			}
			obj(m, "interfaces", name, "unit", "0", "family")
			unit = name + ".0"
		case mg["vlan"] != nil:
			ref, _ := mg["vlan"].(string)
			vlans := obj(m, "vlans")
			vname, vid := ref, ""
			if v, ok := vlans[ref].(map[string]any); ok {
				vid, _ = v["vlan-id"].(string)
			} else {
				for n, v := range vlans {
					if vo, ok := v.(map[string]any); ok && vo["vlan-id"] == ref {
						vname, vid = n, ref
					}
				}
			}
			if vid == "" {
				u.dropped("virtual-chassis member " + id + " management vlan " + ref)
				continue
			}
			vo := obj(vlans, vname)
			l3, _ := vo["l3-interface"].(string)
			if l3 == "" {
				l3 = "irb." + vid
				vo["l3-interface"] = l3
			}
			num := strings.TrimPrefix(l3, "irb.")
			for _, a := range addrs {
				addAddr("irb", num, a, true)
			}
			obj(m, "interfaces", "irb", "unit", num, "family")
			unit = l3
		default:
			continue
		}
		inst := obj(m, "routing-instances", "mgmt_junos")
		ifs, _ := inst["interface"].([]any)
		found := false
		for _, x := range ifs {
			if x == unit {
				found = true
			}
		}
		if !found {
			inst["interface"] = append(ifs, unit)
		}
		for _, g := range gws {
			dst := "0.0.0.0/0"
			if a, err := netip.ParseAddr(g); err == nil && a.Is6() {
				dst = "::/0"
			}
			r := obj(inst, "routing-options", "static", "route", dst)
			hops, _ := r["next-hop"].([]any)
			dup := false
			for _, h := range hops {
				if h == g {
					dup = true
				}
			}
			if !dup {
				r["next-hop"] = append(hops, g)
			}
		}
		obj(m, "system")["management-instance"] = true
	}
}

// removePeerLink drops the MC-LAG peer-link of older versions together with
// its bundle: the stacking links carry the peer traffic now, and the old
// bundle between the two members would make a loop with the stack tunnels.
func (u *upgrader) removePeerLink(m map[string]any) {
	mclag, _ := m["mclag"].(map[string]any)
	domains, _ := mclag["domain"].(map[string]any)
	for id, dv := range domains {
		d, ok := dv.(map[string]any)
		if !ok {
			continue
		}
		pl, _ := d["peer-link"].(string)
		for _, k := range []string{"peer-link", "peer-link-bfd", "heartbeat"} {
			if _, ok := d[k]; ok {
				delete(d, k)
				u.replaced("mclag domain " + id + " " + k)
			}
		}
		if pl == "" {
			continue
		}
		ifs, _ := m["interfaces"].(map[string]any)
		if _, ok := ifs[pl]; ok {
			delete(ifs, pl)
			u.replaced("interfaces " + pl + " (the former peer-link)")
		}
		for _, top := range []string{"interfaces", "interface-range"} {
			entries, _ := m[top].(map[string]any)
			for name, ev := range entries {
				e, _ := ev.(map[string]any)
				eo, _ := e["ether-options"].(map[string]any)
				if eo == nil || eo["802.3ad"] != pl {
					continue
				}
				delete(eo, "802.3ad")
				if len(eo) == 0 {
					delete(e, "ether-options")
				}
				if len(e) == 0 {
					delete(entries, name)
				}
				u.replaced(top + " " + name + " ether-options 802.3ad " + pl)
			}
		}
	}
}
