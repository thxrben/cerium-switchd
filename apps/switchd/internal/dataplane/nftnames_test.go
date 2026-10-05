package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// nft rules name protocols by number: names need /etc/protocols, which the
// cerOS image had not (every commit with a routed interface failed).
func TestNftProtocolsByNumber(t *testing.T) {
	re := regexp.MustCompile(`(l4proto|ip protocol|ip6 nexthdr)\s+(\{[^}]*[a-z][^}]*\}|[a-z][a-z0-9-]*)`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllString(string(raw), -1) {
			if f == "nftnames_test.go" {
				continue
			}
			t.Errorf("%s: %q names a protocol; use its number", f, m)
		}
	}
}
