package schema

// The limits of the configuration language. The parser types and
// `show system limits` (docs/config-reference.md 3.5.1) use these constants,
// so what the page shows is what commit accepts.
const (
	MinMTU = 256 // frame size incl. the Ethernet header (1.3)
	MaxMTU = 16000

	DefaultMTU = 1514

	MinVLANID = 1
	MaxVLANID = 4094
	MaxVNI    = 16777214

	MaxAE = 4095 // ae0..ae4095

	MinMember = 1
	MaxMember = 16

	MinMACLimit = 1
	MaxMACLimit = 131072

	MinMACAging     = 10
	MaxMACAging     = 1000000
	DefaultMACAging = 300

	MaxDomain         = 255
	MembersPerDomain  = 2
	DomainsPerMember  = 1
	MaxVoters         = 7
	DefaultMemberPrio = 128
)
