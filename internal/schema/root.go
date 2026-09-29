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

	management := C("management", "Out-of-band management interface of this member",
		V("interface", "Linux interface used for management (kept outside the switch bridge)", LinuxIfName),
		g("mgmt-addr", V("address", "Static management address", IPPrefix)),
		g("mgmt-addr", F("dhcp", "Obtain management address via DHCP")),
		V("gateway", "Default gateway in the management VRF", IP),
		V("vlan", "Use an in-band VLAN instead of a dedicated interface", VlanID),
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
			C("ssh", "SSH access to the CLI",
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
				F("disable", "Only raise alarms, never change offload settings"),
			),
		),
	)

	stack := C("stack", "Stack (virtual chassis) members",
		L("member", "Stack member", MemberID,
			V("host-name", "Host name of this member", Hostname),
			VD("priority", "Priority for leader election (higher wins)", Uint("<priority>", 0, 255), "128"),
			VD("role", "Member role", Enum(
				E("switch", "Regular switching member"),
				E("witness", "Quorum-only member without data plane"),
			), "switch"),
			management,
			V("vtep-address", "Local VXLAN tunnel endpoint address", IP),
			C("underlay", "Layer 3 interface used for VXLAN transport",
				V("interface", "Linux interface", LinuxIfName),
				V("address", "Underlay address", IPPrefix),
				V("gateway", "Underlay gateway", IP),
			),
		),
	)

	stormLevel := Uint("<pps>", 1, 100000000)
	iface := L("interfaces", "Interface configuration", Interface,
		V("description", "Interface description", Text),
		F("disable", "Administratively disable the interface"),
		VD("mtu", "Maximum frame payload size (jumbo frames up to 16000)", MTU, "1500"),
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
			C("mclag", "Multi-chassis link aggregation",
				V("id", "MC-LAG id, identical on both peers", Uint("<id>", 1, 65535)),
			),
		),
		C("storm-control", "Rate limit flooded traffic",
			V("broadcast", "Broadcast packets per second", stormLevel),
			V("multicast", "Multicast packets per second", stormLevel),
			V("unknown-unicast", "Unknown unicast packets per second", stormLevel),
		),
		V("mac-limit", "Maximum learned MAC addresses", Uint("<count>", 1, 131072)),
		C("offload", "Per-interface hardware acceleration",
			F("disable", "Never offload this interface"),
		),
		V("native-vlan-id", "Untagged VLAN on a trunk port", VlanRef),
		L("unit", "Logical unit", Uint("<unit>", 0, 0),
			V("description", "Unit description", Text),
			C("family", "Protocol family",
				P("ethernet-switching", "Layer 2 switching",
					V("interface-mode", "Port mode", Enum(
						E("access", "Untagged member of one VLAN"),
						E("trunk", "Tagged member of several VLANs"),
					)),
					C("vlan", "VLAN membership",
						LL("members", "VLAN names or ids (ranges like 10-20 allowed)", VlanRef),
					),
				),
			),
		),
	)

	vlans := L("vlans", "VLAN configuration", Identifier,
		V("vlan-id", "802.1Q VLAN id", VlanID),
		V("description", "VLAN description", Text),
		V("mtu", "Maximum frame payload size within this VLAN", MTU),
		C("vxlan", "Extend this VLAN over VXLAN",
			V("vni", "VXLAN network identifier", VNI),
		),
	)

	rstpIf := L("interface", "RSTP port settings", Interface,
		V("cost", "Port path cost", Uint("<cost>", 1, 200000000)),
		VD("priority", "Port priority", UintStep("<priority>", 0, 240, 16), "128"),
		F("edge", "Port connects to an end host (fast transition)"),
		F("bpdu-guard", "Disable the port if a BPDU is received"),
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
	)

	mclag := C("mclag", "Multi-chassis link aggregation",
		L("domain", "MC-LAG domain (a pair of stack members)", Uint("<domain-id>", 1, 255),
			LL("members", "The two stack members forming this domain", MemberID),
			V("peer-link", "Aggregated interface connecting the two peers", AEInterface),
			V("system-mac", "Shared LACP system MAC (derived if unset)", MAC),
			VD("system-priority", "Shared LACP system priority", Uint("<priority>", 1, 65535), "32768"),
			V("anycast-vtep", "Shared VTEP address of the pair", IP),
			C("keepalive", "Peer liveness detection over the management network",
				VD("interval", "Milliseconds between keepalives", Uint("<ms>", 100, 10000), "1000"),
				VD("timeout", "Missed keepalives before the peer is declared dead", Uint("<count>", 2, 30), "3"),
			),
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
		system, stack, iface, vlans, protocols, mclag, switchOpts, fwd,
	)
}
