package netdev

import (
	"encoding/hex"
	"testing"
)

// A reply of Linux 7.2 (RTM_NEWMDB, after the netlink header): bridge
// ifindex 2 with two of its own (irb) memberships, port 3 in 239.1.1.1
// (permanent, VLAN 1) and port 3 as permanent multicast router.
const mdbReply = "0000000002000000900001002c000100280001000200000000000100ff02000000000000000000000000006a86dd0000080001008f6500002c000100280001000200000000000100ff0200000000000000000001ff13111186dd0000080001008f65000034000100300001000300000001000100ef01010100000000000000000000000008000000080001000000000005000500040000002c00020028000100030000000800010000000000050002000200000008000300000000000800040000000000"

func TestParseMDB(t *testing.T) {
	b, _ := hex.DecodeString(mdbReply)
	br, es, rs, err := parseMDB(b)
	if err != nil {
		t.Fatal(err)
	}
	if br != 2 || len(es) != 3 || len(rs) != 1 {
		t.Fatalf("bridge %d entries %+v routers %+v", br, es, rs)
	}
	if e := es[2]; e.ifindex != 3 || e.group != "239.1.1.1" || !e.permanent || e.vid != 1 {
		t.Fatalf("entry %+v", e)
	}
	if es[0].group != "ff02::6a" || es[0].permanent || es[0].expires != 259.99 {
		t.Fatalf("irb entry %+v", es[0])
	}
	if r := rs[0]; r.ifindex != 3 || !r.permanent {
		t.Fatalf("router %+v", r)
	}
}
