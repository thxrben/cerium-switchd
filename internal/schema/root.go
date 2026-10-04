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

	system := C("system", "System parameters",
		V("host-name", "Name of the stack/system", Hostname),
		V("management-instance", "Routing instance for management (cme, services on the master)", InstanceRef),
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
		C("root-authentication", "Password and SSH keys of root",
			V("encrypted-password", "Crypt(3) password hash ($6$/$y$)", String("<hash>", 255, `^\$[0-9a-z]+\$`)),
			LL("ssh-key", "SSH public key (\"ssh-ed25519 AAAA... comment\")",
				String("<public-key>", 16384, `^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-[a-z0-9-]+@openssh\.com) [A-Za-z0-9+/=]+( .*)?$`)),
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
			C("web-management", "REST API over HTTPS (web interface later)",
				VD("port", "HTTPS port", Uint("<port>", 1, 65535), "443"),
				V("certificate", "PEM certificate file (self-signed if unset)", String("<path>", 255, `^/`)),
				V("key", "PEM private key file", String("<path>", 255, `^/`)),
				VD("upload-limit", "Largest software bundle an upload may have", Size(1<<20, 64<<30), "1g"),
				F("disable", "Disable the REST API"),
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
		C("archival", "Copies of the configuration on servers",
			C("configuration", "Configuration archival",
				F("transfer-on-commit", "Send a copy after every commit"),
				V("transfer-interval", "Send a copy every n minutes", Uint("<minutes>", 15, 2880)),
				L("archive-sites", "Where the copies go (tried in order)", String("<url>", 1024, ""),
					V("password", "Login password", String("<password>", 128, "")),
				),
			),
		),
		C("memory", "Fixed memory for tables that must always fit (slots)",
			L("allocation", "Slots of one purpose", named("<purpose>", Enum(
				E("bgp-ipv4", "IPv4 prefixes learned by BGP"),
				E("bgp-ipv6", "IPv6 prefixes learned by BGP"),
				E("bgp-paths", "Further BGP paths to a prefix"),
				E("ospf", "OSPF and OSPFv3 routes"),
				E("arp", "IPv4 neighbours"),
				E("ndp", "IPv6 neighbours"),
				E("mac", "MAC addresses"),
				E("multicast", "IGMP and MLD snooping memberships"),
			)), Grouped("amount",
				V("percent", "Share of the slots", Uint("<percent>", 1, 100)),
				V("slots", "Number of slots", Uint("<count>", 1, 1048576)),
			)...),
			VD("update-size", "Largest software bundle held in memory", Size(64<<20, 16<<30), "512m"),
			VD("management-reserve", "Memory kept for logins and system services", Size(128<<20, 4<<30), "384m"),
		),
		C("timeouts", "How long cerOS waits for disks, the kernel and update steps",
			VD("disk-operation", "One file operation on a disk", Uint("<seconds>", 1, 600), "10"),
			VD("kernel-call", "One netlink, ioctl or sysfs call", Uint("<seconds>", 1, 120), "5"),
			VD("slot-write", "Writing and syncing 4 MiB of a software slot", Uint("<seconds>", 10, 3600), "60"),
			VD("software-transfer", "Copying a bundle to a member", Uint("<seconds>", 60, 7200), "600"),
			VD("software-install", "Installing a bundle on one member", Uint("<seconds>", 60, 7200), "900"),
			VD("member-update", "Waiting for an updated member to come back", Uint("<seconds>", 60, 7200), "600"),
			VD("health-check", "A new version's time to report healthy", Uint("<seconds>", 60, 3600), "300"),
			VD("config-check", "The new version's configuration check", Uint("<seconds>", 10, 1800), "120"),
		),
	)

	stackMACsecMode := Enum(
		E("auto", "Encrypt where both ends offload MACsec to the NIC"),
		E("on", "Encrypt, in software where a NIC cannot offload"),
		E("off", "Never encrypt"),
	)
	stack := C("virtual-chassis", "Stack members and stacking (like a Junos Virtual Chassis)",
		bfd("bfd", "BFD on stacking ports (IP-less)", "100"),
		C("macsec", "MACsec on the stacking links",
			VD("mode", "Default for every stacking port", stackMACsecMode, "auto"),
			L("interface", "Setting of one stacking port", Interface,
				VD("mode", "MACsec on this stacking port", stackMACsecMode, "auto"),
			),
		),
		L("member", "Stack member", MemberID,
			V("host-name", "Host name of this member", Hostname),
			VD("mastership-priority", "Priority for leader election (higher wins)", Uint("<priority>", 0, 255), "128"),
			VD("role", "Member role", Enum(
				E("switch", "Regular switching member"),
				E("witness", "Quorum-only member without data plane"),
			), "switch"),
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
			),
			C("storm-control", "Rate limit flooded traffic",
				V("broadcast", "Broadcast packets per second", stormLevel),
				V("multicast", "Multicast packets per second", stormLevel),
			),
			V("mac-limit", "Maximum learned MAC addresses", Uint("<count>", MinMACLimit, MaxMACLimit)),
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
						L("address", "Interface address", IPPrefix,
							V("member", "Only on this member (irb units)", MemberID),
						),
						F("dhcp", "Obtain the IPv4 address via DHCP"),
					),
					P("inet6", "IPv6 (routed interface)",
						L("address", "Interface address", IPPrefix,
							V("member", "Only on this member (irb units)", MemberID),
						),
					),
				),
			),
		}
	}
	iface := L("interfaces", "Interface configuration", Interface,
		append(ifaceChildren(), F("management", "Management port: carries only cme (the stack's management address, on the master)"))...)
	ifRange := L("interface-range", "Apply one configuration to many ports", Identifier,
		append([]*Node{
			LL("member", "Ports by pattern, e.g. 1/0/* or */1/[0-3]", IfPattern),
			L("member-range", "Contiguous ports on one card, e.g. 1/0/0 to 1/0/23", PhysInterface,
				V("to", "Last port of the range", PhysInterface),
			),
		}, ifaceChildren()...)...,
	)
	ifRange.MinAbbrev = len("interface-")

	vlans := L("vlans", "VLAN configuration", VlanName,
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

	snooping := func(name, help, versions, def string) *Node {
		lo, hi := uint64(2), uint64(3)
		if versions == "1|2" {
			lo, hi = 1, 2
		}
		return P(name, help,
			F("disable", "Snooping off in every VLAN"),
			L("vlan", "Per-VLAN settings (all: every VLAN)", VlanRefAll,
				F("disable", "Snooping off in this VLAN"),
				F("querier", "Send general queries in this VLAN"),
				VD("version", "Version of the queries", Uint("<version>", lo, hi), def),
			),
			L("interface", "Per-port settings", Interface,
				F("immediate-leave", "A leave removes the port at once"),
				F("multicast-router-interface", "Always send all group traffic here"),
			),
		)
	}
	protocols := C("protocols", "Protocol configuration",
		P("lldp", "Link layer discovery protocol (802.1AB); the stack is one system",
			F("disable", "Stop LLDP on every port"),
			L("interface", "Ports LLDP runs on (default: all)", LLDPInterface,
				F("disable", "No LLDP on this port"),
			),
			VD("advertisement-interval", "Seconds between LLDPDUs", Uint("<seconds>", 5, 32768), "30"),
			VD("hold-multiplier", "Time to live in advertisement intervals", Uint("<multiplier>", 2, 10), "4"),
		),
		P("rstp", "Rapid spanning tree (802.1w)",
			VD("bridge-priority", "Bridge priority", UintStep("<priority>", 0, 61440, 4096), "32768"),
			VD("hello-time", "Hello interval in seconds", Uint("<seconds>", 1, 10), "2"),
			VD("max-age", "Maximum BPDU age in seconds", Uint("<seconds>", 6, 40), "20"),
			VD("forward-delay", "Forward delay in seconds", Uint("<seconds>", 4, 30), "15"),
			rstpIf,
			F("disable", "Disable RSTP"),
		),
		snooping("igmp-snooping", "IGMP snooping (on by default in every VLAN)", "2|3", "2"),
		snooping("mld-snooping", "MLD snooping (on by default in every VLAN)", "1|2", "1"),
		ospfNode("ospf", "OSPF version 2 (IPv4)", true), ospfNode("ospf3", "OSPFv3 (IPv6)", false), bgpNode(),
		C("layer2-control", "Layer 2 protocol protection",
			C("bpdu-block", "Shut down ports that receive BPDUs",
				LL("interface", "Protected interfaces", Interface),
				V("disable-timeout", "Seconds until a blocked port is re-enabled (never if unset)", Uint("<seconds>", 10, 86400)),
			),
		),
	)

	mclag := C("mclag", "Multi-chassis link aggregation (bundles with ports on two members)",
		VD("delay-restore", "Seconds to wait after a boot before MC-LAG legs join their bundles", Uint("<seconds>", 0, 3600), "300"),
	)

	switchOpts := C("switch-options", "Global switching options",
		VD("mac-table-aging-time", "MAC table aging time in seconds", Uint("<seconds>", MinMACAging, MaxMACAging), "300"),
		C("vxlan", "VXLAN to VTEPs outside the stack (the stack is one VTEP)",
			V("source-address", "The stack's VTEP address (on every member)", IPv4),
			VD("udp-port", "VXLAN UDP destination port", Uint("<port>", 1, 65535), "4789"),
			L("remote-vtep", "Remote VTEP", IP,
				LL("vni", "VNIs to extend to this VTEP", VNI),
			),
		),
	)

	routing := RoutingOptions("Routing of the default instance")
	instances := L("routing-instances", "Separate routing tables (VRFs); system management-instance names the management instance", InstanceName,
		V("description", "Instance description", Text),
		VD("instance-type", "Instance type", Enum(E("virtual-router", "Separate routing table")), "virtual-router"),
		LL("interface", "Routed units in this instance (irb.10, 1/0/5.0)", UnitName),
		RoutingOptions("Routing of this instance"),
		C("protocols", "Routing protocols of this instance", RoutingProtocols()...),
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
	instances.Wrapped = true

	hex := func(name string, max int) *Type { return String(name, max, `^([0-9a-fA-F]{2})+$`) }
	security := C("security", "Security",
		C("macsec", "MACsec (IEEE 802.1AE) on ports, keys by MKA",
			L("connectivity-association", "A connectivity association (CAK and settings)", Identifier,
				VD("security-mode", "How the keys are agreed", Enum(
					E("static-cak", "Pre-shared CAK, MKA derives the session keys"),
				), "static-cak"),
				VD("cipher-suite", "Encryption", Enum(
					E("gcm-aes-128", "GCM-AES-128"),
					E("gcm-aes-256", "GCM-AES-256"),
				), "gcm-aes-128"),
				C("pre-shared-key", "The connectivity association key and its name",
					V("ckn", "Key name (hex)", hex("<hex>", 64)),
					V("cak", "Key (hex: 32 digits for 128-bit, 64 for 256-bit suites)", hex("<hex>", 64)),
				),
				C("mka", "MACsec Key Agreement",
					VD("key-server-priority", "Key server priority (lower is preferred)", Uint("<priority>", 0, 255), "16"),
				),
				C("replay-protect", "Replay protection",
					VD("replay-window-size", "Frames that may arrive out of order", Uint("<packets>", 0, 65535), "0"),
				),
			),
			L("interfaces", "Ports secured with MACsec", Interface,
				V("connectivity-association", "The connectivity association", Identifier),
			),
		),
	)

	return C("", "",
		system, stack, ifRange, iface, vlans, protocols, mclag, switchOpts, routing, instances, fwd, PolicyOptions(), security,
	)
}

// named is t under another name.
func named(name string, t *Type) *Type {
	t.Name = name
	return t
}
