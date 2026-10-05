package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/config"
)

var tokenName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func apiTokenCommand() *command {
	return &command{name: "api-token", help: "REST API tokens for orchestrators", class: commit.SuperUser, sub: []*command{
		{name: "create", help: "Make a token: '<name> user <user>' (printed once; written into the candidate)", class: commit.SuperUser,
			run: (*Shell).apiTokenCreate, complete: words(Completion{Text: "<name>", Help: "Token name", Placeholder: true})},
	}}
}

// apiTokenCreate is "request system api-token create <name> user <user>"
// (reference 5.1 web-management): a random token, printed once; only its
// SHA-256 goes into the candidate configuration.
func (sh *Shell) apiTokenCreate(c *call) error {
	if len(c.args) != 3 || !prefixOf(c.args[1].Text, "user") {
		return &posError{pos: c.argPos(0), msg: "expecting <name> user <user>"}
	}
	name, user := c.args[0].Text, c.args[2].Text
	if !tokenName.MatchString(name) {
		return &posError{pos: c.argPos(0), msg: "a token name has letters, digits, '.', '_' and '-' (at most 64)"}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))
	lines := fmt.Sprintf("set system services web-management api-token %s user %s\nset system services web-management api-token %s hash %s\n",
		name, user, name, hex.EncodeToString(sum[:]))
	s, _, err := sh.env.Engine.Configure(sh.env.User, sh.env.Class, commit.Shared)
	if err != nil {
		return err
	}
	err = s.Modify(func(t *config.Tree) error { return config.ApplySetLines(t, lines) })
	s.Close()
	if err != nil {
		return err
	}
	sh.env.Log.Info("API token created (in the candidate)", "facility", "change-log", "user", sh.env.User, "token", name, "for", user)
	fmt.Fprintf(c.out, "Token %s for %s (shown only now; keep it secret):\n  %s\n", name, user, token)
	c.out.WriteString("Written into the candidate configuration: commit to activate it (it has the class of the user).\n")
	return nil
}
