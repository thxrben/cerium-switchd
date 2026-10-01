package schema

// The routing statements (reference 5.8, 5.11-5.14): routing-options,
// policy-options and the routing protocols, used by the default instance
// and every routing instance.

var (
	preference = Uint("<preference>", 0, 255)
	metric     = Uint("<metric>", 0, 4294967295)
)

// bfdNode is bfd-liveness-detection (reference 5.12).
func bfdNode() *Node {
	return C("bfd-liveness-detection", "BFD failure detection for the neighbours",
		VD("minimum-interval", "Transmit and receive interval in milliseconds", Uint("<ms>", 50, 60000), "300"),
		VD("multiplier", "Missed packets until the neighbour is down", Uint("<n>", 1, 255), "3"),
		C("authentication", "BFD authentication",
			V("algorithm", "Authentication algorithm", Enum(
				E("keyed-sha-1", "Keyed SHA-1"),
				E("keyed-md5", "Keyed MD5"),
			)),
			V("key", "Shared secret", String("<secret>", 80, "")),
			VD("key-id", "Key id", Uint("<key-id>", 0, 255), "1"),
		),
	)
}

// RoutingOptions is routing-options of the default instance or of a
// routing instance (reference 5.8).
func RoutingOptions(help string) *Node {
	return C("routing-options", help,
		C("static", "Static routes",
			L("route", "Destination network", RoutePrefix,
				LL("next-hop", "Gateway addresses (several: ECMP)", IP),
				F("discard", "Drop matching traffic silently"),
				V("preference", "Preference instead of 5 (lower wins)", preference),
			),
		),
		V("router-id", "Router id of OSPF and BGP", IPv4),
		V("autonomous-system", "AS number of BGP", ASN),
	)
}

// Protocols are the routing protocols of an instance (reference 5.13,
// 5.14).
func RoutingProtocols() []*Node {
	return []*Node{ospfNode("ospf", "OSPF version 2 (IPv4)", true), ospfNode("ospf3", "OSPFv3 (IPv6)", false), bgpNode()}
}

func ospfNode(name, help string, v2 bool) *Node {
	ifChildren := []*Node{
		F("passive", "Announce the subnets, form no neighbours"),
		V("metric", "Cost (default: reference-bandwidth / speed)", Uint("<metric>", 1, 65535)),
		V("interface-type", "Network type", Enum(E("p2p", "Point-to-point (no DR election)"))),
		VD("priority", "DR election priority (0: never DR)", Uint("<priority>", 0, 255), "128"),
		VD("hello-interval", "Seconds between hellos", Uint("<seconds>", 1, 255), "10"),
		V("dead-interval", "Seconds without hellos until a neighbour is down (default 4 x hello)", Uint("<seconds>", 2, 65535)),
		VD("retransmit-interval", "Seconds until an unacknowledged LSA is sent again", Uint("<seconds>", 1, 65535), "5"),
		VD("transit-delay", "Seconds added to LSA ages when flooding", Uint("<seconds>", 1, 65535), "1"),
		bfdNode(),
	}
	if v2 {
		ifChildren = append(ifChildren, C("authentication", "OSPF authentication",
			Grouped("auth",
				V("simple-password", "Plain-text password (8 characters at most)", String("<key>", 8, "")),
				L("md5", "MD5 key (several: rollover)", Uint("<key-id>", 0, 255),
					V("key", "Shared secret", String("<key>", 16, "")),
				),
			)...,
		))
	}
	return P(name, help,
		L("area", "OSPF area", AreaID,
			L("interface", "Routed unit in this area", UnitName, ifChildren...),
		),
		LL("export", "Policies redistributing routes into OSPF", PolicyName),
		VD("reference-bandwidth", "Bandwidth with cost 1", Bandwidth, "100g"),
		P("overload", "Announce maximum metric (no transit traffic)",
			V("timeout", "Seconds after every start (default: always)", Uint("<seconds>", 60, 1800)),
		),
		C("graceful-restart", "Graceful restart (RFC 3623)",
			F("disable", "No graceful restart"),
			VD("restart-duration", "Seconds a restart may take", Uint("<seconds>", 1, 3600), "120"),
		),
		F("disable", "Configured but not running"),
	)
}

func bgpNode() *Node {
	common := func() []*Node {
		return []*Node{
			V("description", "Description", Text),
			V("peer-as", "AS number of the neighbour", ASN),
			V("local-address", "Source address of the session", IP),
			V("local-as", "Local AS towards this neighbour (AS migration)", ASN),
			V("authentication-key", "TCP MD5 signature secret", String("<secret>", 80, "")),
			V("hold-time", "Hold time in seconds (0: no keepalives)", Uint("<seconds>", 0, 65535)),
			F("passive", "Only accept connections, never connect"),
			P("multihop", "The neighbour is not directly connected",
				V("ttl", "TTL of the session's packets", Uint("<ttl>", 1, 255)),
			),
			C("family", "Address families",
				C("inet", "IPv4", F("unicast", "IPv4 unicast routes")),
				C("inet6", "IPv6", F("unicast", "IPv6 unicast routes")),
			),
			LL("import", "Import policies", PolicyName),
			LL("export", "Export policies", PolicyName),
			F("remove-private", "Remove private AS numbers towards external neighbours"),
			bfdNode(),
			C("graceful-restart", "Graceful restart (RFC 4724)",
				F("disable", "No graceful restart"),
				VD("restart-time", "Seconds the neighbour keeps our routes", Uint("<seconds>", 1, 4095), "120"),
				VD("stale-routes-time", "Seconds we keep a restarting neighbour's routes", Uint("<seconds>", 1, 3600), "300"),
			),
			F("disable", "Configured, but no session"),
		}
	}
	group := append([]*Node{
		V("type", "Internal or external BGP", Enum(
			E("internal", "iBGP: neighbours in the own AS"),
			E("external", "eBGP: neighbours in other ASes"),
		)),
		L("neighbor", "Neighbour address", IP, common()...),
		P("multipath", "Install equal BGP paths as ECMP",
			F("multiple-as", "Also across neighbouring ASes"),
		),
		V("cluster", "Route reflector cluster id (the neighbours are clients)", IPv4),
	}, common()...)
	return P("bgp", "BGP-4 (IPv4 and IPv6 unicast)",
		L("group", "Neighbour group", Identifier, group...),
		F("disable", "Configured but not running"),
	)
}

// PolicyOptions is policy-options (reference 5.11).
func PolicyOptions() *Node {
	routeFilterMatch := Enum(
		E("exact", "Only this prefix"),
		E("orlonger", "This prefix and more specific ones"),
		E("longer", "Only more specific prefixes"),
	)
	from := C("from", "Match conditions (all must match)",
		LL("protocol", "Route source", Enum(
			E("direct", "Interface subnets"), E("local", "Own addresses"), E("static", "Static routes"),
			E("ospf", "OSPF"), E("ospf3", "OSPFv3"), E("bgp", "BGP"), E("aggregate", "Aggregate routes"),
		)),
		L("route-filter", "Prefix match", RoutePrefix,
			Grouped("match",
				F("exact", "Only this prefix"),
				F("orlonger", "This prefix and more specific ones"),
				F("longer", "Only more specific prefixes"),
				V("upto", "Up to this prefix length (/n)", String("</n>", 4, `^/[0-9]{1,3}$`)),
				V("prefix-length-range", "Prefix lengths (/a-/b)", String("</a-/b>", 9, `^/[0-9]{1,3}-/[0-9]{1,3}$`)),
			)...,
		),
		LL("prefix-list", "Exact prefixes of a prefix list", PolicyName),
		L("prefix-list-filter", "Prefix list with a match type", PolicyName,
			V("match", "Match type", routeFilterMatch),
		),
		LL("community", "Communities (all members)", PolicyName),
		LL("as-path", "AS path expressions", PolicyName),
		LL("neighbor", "BGP neighbour", IP),
		LL("area", "OSPF area", AreaID),
		V("family", "Address family", Enum(E("inet", "IPv4"), E("inet6", "IPv6"))),
		V("tag", "OSPF external route tag", metric),
	)
	then := C("then", "Actions",
		Grouped("flow",
			F("accept", "Accept (ends the evaluation)"),
			F("reject", "Reject (ends the evaluation)"),
			V("next", "Continue with the next term or policy", Enum(E("term", "Next term"), E("policy", "Next policy"))),
		)...,
	)
	then.Children = append(then.Children,
		V("metric", "BGP MED / OSPF external metric", metric),
		V("metric-add", "Add to the metric", metric),
		V("local-preference", "BGP local preference", metric),
		V("preference", "Preference in this switch's RIB", preference),
		C("community", "Community changes",
			LL("add", "Add the members of these communities", PolicyName),
			LL("delete", "Remove the members of these communities", PolicyName),
			LL("set", "Replace the communities", PolicyName),
		),
		V("as-path-prepend", "AS numbers to prepend (\"65000 65000\")", String("<as-path>", 255, `^[0-9]+( [0-9]+)*$`)),
		V("next-hop", "Next hop", &Type{Name: "self|discard|<ip>", Check: func(s string) (string, error) {
			if s == "self" || s == "discard" {
				return s, nil
			}
			return IP.Check(s)
		}}),
		V("external-type", "OSPF external type", Enum(E("1", "Type 1"), E("2", "Type 2"))),
		V("tag", "OSPF external route tag", metric),
	)
	return C("policy-options", "Routing policies",
		L("prefix-list", "List of prefixes", PolicyName, LL("prefix", "Prefixes", RoutePrefix)),
		L("community", "Named community set", PolicyName, LL("members", "Community members", Community)),
		L("as-path", "Named AS path expression", PolicyName, V("path", "Regular expression over AS numbers", String("<regex>", 255, ""))),
		L("policy-statement", "Routing policy", PolicyName,
			L("term", "Term (evaluated in order)", Identifier, from, then),
			C("then", "Actions for routes no term terminated",
				Grouped("flow",
					F("accept", "Accept"),
					F("reject", "Reject"),
				)...,
			),
		),
	)
}
