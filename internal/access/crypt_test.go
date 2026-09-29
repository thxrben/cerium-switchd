package access

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSHA512CryptVectors(t *testing.T) {
	// Test vectors from the SHA-crypt specification (Drepper).
	cases := []struct{ setting, pw, want string }{
		{"$6$saltstring", "Hello world!",
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"$6$rounds=10000$saltstringsaltstring", "Hello world!",
			"$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"$6$rounds=5000$toolongsaltstring", "This is just a test",
			"$6$rounds=5000$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
		{"$6$rounds=1400$anotherlongsaltstring", "a very much longer text to encrypt.  This one even stretches over morethan one line.",
			"$6$rounds=1400$anotherlongsalts$POfYwTEok97VWcjxIiSOjiykti.o/pQs.wPvMxQ6Fm7I6IoYN3CmLs66x9t0oSwbtEW7o7UmJEiDwGqd8p4ur1"},
		{"$6$rounds=10$roundstoolow", "the minimum number is still observed",
			"$6$rounds=1000$roundstoolow$kUMsbe306n21p9R.FRkW3IGn.S9NPN0x50YhH1xhLsPuWGsUSklZt58jaTfF4ZEQpyUNGc0dqbpBYYBaHHrsX."},
	}
	for _, c := range cases {
		got, err := SHA512Crypt(c.pw, c.setting)
		if err != nil || got != c.want {
			t.Errorf("SHA512Crypt(%q, %q) =\n%s\nwant\n%s (%v)", c.pw, c.setting, got, c.want, err)
		}
	}
}

func TestHashPasswordAgainstOpenSSL(t *testing.T) {
	h, err := HashPassword("s3cret pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$6$") || len(h) != 3+16+1+86 || !CheckPassword("s3cret pass", h) || CheckPassword("wrong", h) {
		t.Fatalf("hash %q", h)
	}
	salt := strings.Split(h, "$")[2]
	out, err := exec.Command("openssl", "passwd", "-6", "-salt", salt, "s3cret pass").Output()
	if err != nil {
		t.Skip("openssl not available")
	}
	if strings.TrimSpace(string(out)) != h {
		t.Errorf("openssl disagrees:\n%s\n%s", out, h)
	}
}
