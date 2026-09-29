package schema

import "sync"

var (
	rootOnce sync.Once
	root     *Node
)

// Root returns the configuration schema root. The schema is immutable and
// shared.
func Root() *Node {
	rootOnce.Do(func() { root = link(build()) })
	return root
}

func g(group string, n *Node) *Node { n.Group = group; return n }

// bfd builds a BFD timer container.
func bfd(name, help, interval string) *Node {
	return C(name, help,
		VD("minimum-interval", "Transmit/receive interval in milliseconds", Uint("<ms>", 50, 10000), interval),
		VD("multiplier", "Missed packets before the session goes down", Uint("<count>", 2, 255), "3"),
	)
}

func build() *Node {
	severity := Enum(
		E("emergency", "System is unusable"),
		E("alert", "Action must be taken immediately"),
		E("critical", "Critical conditions"),
		E("error", "Error conditions"),
		E("warning", "Warning conditions"),
		E("notice", "Normal but significant condition"),
		E("info", "Informational messages"),
		E("debug", "Debug messages"),
		E("any", "All severities"),
	)
	facility := Enum(
		E("any", "All facilities"),
		E("kernel", "Kernel messages"),
		E("daemon", "Switch daemon messages"),
		E("authorization", "Login and authorization"),
		E("change-log", "Configuration changes"),
		E("interactive-commands", "Commands entered in the CLI"),
		E("local0", "Local facility 0"), E("local1", "Local facility 1"),
		E("local2", "Local facility 2"), E("local3", "Local facility 3"),
		E("local4", "Local facility 4"), E("local5", "Local facility 5"),
		E("local6", "Local facility 6"), E("local7", "Local facility 7"),
	)

	management := C("management", "Management IP interface of this member (management VRF)",
		g("mgmt-attach", V("vlan", "Attach the management IP to this VLAN (IRB-like)", VlanSingle)),
		g("mgmt-attach", V("interface", "Dedicated, non-switched management port", PhysInterface)),
		LL("address", "Static addresses (IPv4 and/or IPv6)", IPPrefix),
		F("dhcp", "Obtain the IPv4 address via DHCP"),
		LL("gateway", "Default gateway, at most one per address family", IP),
	)

	system := C("system", "System parameters",
		V("host-name", "Name of the stack/system", Hostname),
		V("domain-name", "DNS domain name", Hostname),
		V("time-zone", "Time zone (e.g. Europe/Berlin)", String("<time-zone>", 64, `^[A-Za-z0-9_+/-]+$`)),
		LL("name-server", "DNS servers", IP),
		C("ntp", "Network time protocol",
			L("server", "NTP server", Host,
				F("prefer", "Prefer this server"),
			),
		),
		C("syslog", "System logging",
			L("host", "Remote syslog server", Host,
				VD("port", "Destination port", Uint("<port>", 1, 65535), "514 (udp/tcp), 6514 (tls)"),
				VD("transport", "Transport protocol", Enum(
					E("udp", "RFC 5426 UDP"),
					E("tcp", "RFC 6587 TCP"),
					E("tls", "RFC 5425 TLS"),
				), "udp"),
				VD("facility", "Facility filter", facility, "any"),
				VD("severity", "Minimum severity to send", severity, "info"),
				V("ca-certificate", "PEM file used to verify the TLS server", String("<path>", 255, `^/`)),
			),
			VD("local-buffer-size", "Lines kept for 'show log'", Uint("<lines>", 100, 100000), "5000"),
		),
		C("login", "Local user accounts",
			V("message", "Login banner", Text),
			L("user", "User account", Username,
				V("uid", "Numeric user id", Uint("<uid>", 1000, 64000)),
				VD("class", "Permission class", Enum(
					E("super-user", "All permissions, including 'start shell'"),
					E("operator", "Operational commands and configuration"),
					E("read-only", "Operational show commands only"),
				), "read-only"),
				V("full-name", "Full name", Text),
				C("authentication", "Authentication methods",
					V("encrypted-password", "Crypt(3) password hash ($6$/$y$)", String("<hash>", 255, `^\$[0-9a-z]+\$`)),
					LL("ssh-key", "SSH public key (\"ssh-ed25519 AAAA... comment\")",
						String("<public-key>", 16384, `^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-[a-z0-9-]+@openssh\.com) [A-Za-z0-9+/=]+( .*)?$`)),
				),
			),
		),
		C("services", "System services",
			P("ssh", "SSH access to the CLI (present: switchd manages the SSH server configuration)",
				VD("port", "Listening port", Uint("<port>", 1, 65535), "22"),
				VD("root-login", "Root login policy", Enum(
					E("deny", "Do not allow root login"),
					E("allow", "Allow root login"),
					E("key-only", "Allow root login with keys only"),
				), "deny"),
			),
			C("web-management", "Web interface and REST API",
				VD("port", "HTTPS port", Uint("<port>", 1, 65535), "443"),
				V("certificate", "PEM certificate file (self-signed if unset)", String("<path>", 255, `^/`)),
				V("key", "PEM private key file", String("<path>", 255, `^/`)),
				F("disable", "Disable the web interface"),
			),
		),
		C("commit", "Commit behaviour",
			C("confirmation", "Automatic rollback of unconfirmed commits",
				VD("mode", "Whether every commit must be confirmed", Enum(
					E("required", "Every commit must be confirmed or it is rolled back"),
					E("optional", "Only 'commit confirmed' requires confirmation"),
				), "required"),
				VD("timeout", "Minutes until an unconfirmed commit is rolled back", Uint("<minutes>", 1, 60), "10"),
			),
		),
		C("ports", "Console ports",
			F("no-auto-detect", "Do not start a CLI login on detected serial ports"),
			F("login-required", "Ask for user name and password on the local consoles (default: root without password)"),
			L("console", "Serial console port", TTY,
				VD("speed", "Baud rate", Enum(
					E("9600", ""), E("19200", ""), E("38400", ""), E("57600", ""), E("115200", ""),
				), "115200"),
				F("disable", "Do not start a login on this port"),
			),
		),
		C("offload", "Hardware acceleration",
			VD("mode", "Offload policy", Enum(
				E("auto", "Use hardware offload where available, fall back on errors"),
				E("disable", "Software forwarding only"),
			), "auto"),
			C("watchdog", "Runtime offload health monitoring",
				VD("interval", "Seconds between counter checks", Uint("<seconds>", 1, 300), "5"),
				VD("threshold", "Drops/errors per interval that trigger a software fallback", Uint("<count>", 1, 1000000), "100"),
				F("alarm-only", "Only raise alarms, never change offload settings"),
			),
		),
	)

	stack := C("virtual-chassis", "Stack members and stacking (like a Junos Virtual Chassis)",
		bfd("bfd", "BFD on stacking ports (IP-less)", "100"),
		L("member", "Stack member", MemberID,
			V("host-name", "Host name of this member", Hostname),
			VD("mastership-priority", "Priority for leader election (higher wins)", Uint("<priority>", 0, 255), "128"),
			VD("role", "Member role", Enum(
				E("switch", "Regular switching member"),
				E("witness", "Quorum-only member without data plane"),
			), "switch"),
			management,
			V("vtep-address", "Local VXLAN tunnel endpoint address", IP),
			C("underlay", "Layer 3 interface carrying VXLAN tunnels (default VRF)",
				g("ul-attach", V("vlan", "Attach the underlay IP to this VLAN (IRB-like)", VlanSingle)),
				g("ul-attach", V("interface", "Dedicated, non-switched underlay port", PhysInterface)),
				LL("address", "Underlay addresses", IPPrefix),
				LL("gateway", "Next hop towards remote VTEPs, at most one per address family", IP),
			),
		),
	)

	stormLevel := Uint("<pps>", 1, 100000000)
	// ifaceChildren builds the statements shared by interfaces and
	// interface-range (a fresh copy each time, nodes must not be shared).
	ifaceChildren := func() []*Node {
		return []*Node{
			V("description", "Interface description", Text),
			F("disable", "Administratively disable the interface"),
			VD("mtu", "Maximum frame size incl. Ethernet header, excl. FCS and VLAN tags", MTU, "1514"),
			C("ether-options", "Physical port options",
				V("802.3ad", "Make this port a member of an aggregated interface", AEInterface),
				g("flow", F("flow-control", "Enable pause frames (reduces drops under load)")),
				g("flow", F("no-flow-control", "Disable pause frames")),
			),
			C("aggregated-ether-options", "Aggregated interface options",
				P("lacp", "Link aggregation control protocol",
					g("lacp-mode", F("active", "Actively send LACPDUs")),
					g("lacp-mode", F("passive", "Only respond to LACPDUs")),
					VD("periodic", "LACPDU interval", Enum(E("fast", "Every second"), E("slow", "Every 30 seconds")), "fast"),
					VD("system-priority", "LACP system priority", Uint("<priority>", 1, 65535), "32768"),
				),
				VD("minimum-links", "Minimum active links for the bundle to be up", Uint("<links>", 1, 64), "1"),
				VD("hash-policy", "Load-balancing hash", Enum(
					E("layer2", "Source/destination MAC"),
					E("layer2+3", "MAC and IP addresses"),
					E("layer3+4", "IP addresses and ports"),
				), "layer3+4"),
				P("mclag", "Bundle spans the two members of an MC-LAG domain"),
			),
			C("storm-control", "Rate limit flooded traffic",
				V("broadcast", "Broadcast packets per second", stormLevel),
				V("multicast", "Multicast packets per second", stormLevel),
			),
			V("mac-limit", "Maximum learned MAC addresses", Uint("<count>", 1, 131072)),
			C("offload", "Per-interface hardware acceleration",
				F("disable", "Never offload this interface"),
			),
			V("native-vlan-id", "Untagged VLAN on a trunk port", VlanSingle),
			F("vlan-tagging", "Routed subinterfaces: each unit takes the frames with its vlan-id"),
			L("unit", "Logical unit", Uint("<unit>", 0, MaxUnit),
				V("description", "Unit description", Text),
				F("disable", "Administratively disable the unit"),
				V("vlan-id", "802.1Q tag of a routed subinterface (needs vlan-tagging)", VlanID),
				C("family", "Protocol family",
					P("ethernet-switching", "Layer 2 switching (unit 0 only)",
						V("interface-mode", "Port mode", Enum(
							E("access", "Untagged member of one VLAN"),
							E("trunk", "Tagged member of several VLANs"),
						)),
						C("vlan", "VLAN membership",
							LL("members", "VLAN names or ids (ranges like 10-20 allowed)", VlanRef),
						),
					),
					P("inet", "IPv4 (routed interface)",
						LL("address", "Interface addresses", IPPrefix),
					),
					P("inet6", "IPv6 (routed interface)",
						LL("address", "Interface addresses", IPPrefix),
					),
				),
			),
		}
	}
	iface := L("interfaces", "Interface configuration", Interface, ifaceChildren()...)
	ifRange := L("interface-range", "Apply one configuration to many ports", Identifier,
		append([]*Node{
			LL("member", "Ports by pattern, e.g. 1/0/* or */1/[0-3]", IfPattern),
			L("member-range", "Contiguous ports on one card, e.g. 1/0/0 to 1/0/23", PhysInterface,
				V("to", "Last port of the range", PhysInterface),
			),
		}, ifaceChildren()...)...,
	)
	ifRange.MinAbbrev = len("interface-")

	vlans := L("vlans", "VLAN configuration", Identifier,
		V("vlan-id", "802.1Q VLAN id", VlanID),
		V("description", "VLAN description", Text),
		V("l3-interface", "VLAN IP interface (routing between VLANs)", IrbUnit),
		V("mtu", "Maximum frame size within this VLAN (same meaning as interface mtu)", MTU),
		C("vxlan", "Extend this VLAN over VXLAN",
			V("vni", "VXLAN network identifier", VNI),
		),
	)

	rstpIf := L("interface", "RSTP port settings", Interface,
		V("cost", "Port path cost", Uint("<cost>", 1, 200000000)),
		VD("priority", "Port priority", UintStep("<priority>", 0, 240, 16), "128"),
		F("edge", "Port connects to an end host (fast transition)"),
		F("no-root-port", "Root guard: never become root port"),
		V("mode", "Link type", Enum(E("point-to-point", "Full duplex link"), E("shared", "Shared medium"))),
		F("disable", "Do not run RSTP on this port"),
	)

	protocols := C("protocols", "Protocol configuration",
		P("rstp", "Rapid spanning tree (802.1w)",
			VD("bridge-priority", "Bridge priority", UintStep("<priority>", 0, 61440, 4096), "32768"),
			VD("hello-time", "Hello interval in seconds", Uint("<seconds>", 1, 10), "2"),
			VD("max-age", "Maximum BPDU age in seconds", Uint("<seconds>", 6, 40), "20"),
			VD("forward-delay", "Forward delay in seconds", Uint("<seconds>", 4, 30), "15"),
			rstpIf,
			F("disable", "Disable RSTP"),
		),
		C("layer2-control", "Layer 2 protocol protection",
			C("bpdu-block", "Shut down ports that receive BPDUs",
				LL("interface", "Protected interfaces", Interface),
				V("disable-timeout", "Seconds until a blocked port is re-enabled (never if unset)", Uint("<seconds>", 10, 86400)),
			),
		),
	)

	mclag := C("mclag", "Multi-chassis link aggregation",
		L("domain", "MC-LAG domain (a pair of stack members)", Uint("<domain-id>", 1, 255),
			LL("members", "The two stack members forming this domain", MemberID),
			V("peer-link", "Aggregated interface connecting the two peers", AEInterface),
			V("system-mac", "Shared LACP system MAC (derived if unset)", MAC),
			VD("system-priority", "Shared LACP system priority", Uint("<priority>", 1, 65535), "32768"),
			V("anycast-vtep", "Shared VTEP address of the pair", IP),
			bfd("heartbeat", "BFD heartbeat over the management network (split-brain detection)", "300"),
			bfd("peer-link-bfd", "Micro-BFD on every peer-link port (IP-less)", "100"),
			VD("delay-restore", "Seconds to wait after reboot before enabling MC-LAG ports", Uint("<seconds>", 0, 3600), "300"),
		),
	)

	switchOpts := C("switch-options", "Global switching options",
		VD("mac-table-aging-time", "MAC table aging time in seconds", Uint("<seconds>", 10, 1000000), "300"),
		C("vxlan", "VXLAN transport",
			VD("mode", "How remote MACs are learned", Enum(
				E("control-plane", "Distribute MACs between stack members (no flooding to learn)"),
				E("flood-and-learn", "Learn from data traffic"),
			), "control-plane"),
			VD("udp-port", "VXLAN UDP destination port", Uint("<port>", 1, 65535), "4789"),
			L("remote-vtep", "Static VTEP outside the stack", IP,
				LL("vni", "VNIs to extend to this VTEP", VNI),
			),
			F("encryption", "Encrypt VXLAN underlay traffic with WireGuard"),
		),
	)

	routing := C("routing-options", "Routing of the default instance",
		C("static", "Static routes",
			L("route", "Destination network", RoutePrefix,
				LL("next-hop", "Gateway addresses (several: ECMP)", IP),
				F("discard", "Drop matching traffic silently"),
			),
		),
	)
	fwd := C("forwarding-options", "Forwarding options",
		L("analyzer", "Port mirroring session", Identifier,
			C("input", "Traffic to mirror",
				C("ingress", "Traffic received",
					LL("interface", "Source interfaces", Interface),
					LL("vlan", "Source VLANs", VlanRef),
				),
				C("egress", "Traffic transmitted",
					LL("interface", "Source interfaces", Interface),
				),
			),
			C("output", "Mirror destination",
				V("interface", "Destination interface", Interface),
			),
		),
	)

	iface.Wrapped = true
	vlans.Wrapped = true

	return C("", "",
		system, stack, ifRange, iface, vlans, protocols, mclag, switchOpts, routing, fwd,
	)
}
