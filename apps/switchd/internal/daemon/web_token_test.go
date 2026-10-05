package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
)

func TestWebTokenAuth(t *testing.T) {
	sum := sha256.Sum256([]byte("s3cret-token"))
	cfg := &model.Config{System: model.System{
		Users: map[string]*model.User{"orch": {Name: "orch", Class: "operator"}},
		Web:   model.WebService{Tokens: map[string]model.APIToken{"ci": {User: "orch", Hash: hex.EncodeToString(sum[:])}}},
	}}
	u, ok := webTokenAuth(cfg, "s3cret-token")
	if !ok || u.Name != "orch" || u.Class != commit.Operator || u.SuperUser {
		t.Fatalf("user %+v ok %v", u, ok)
	}
	if _, ok := webTokenAuth(cfg, "wrong"); ok {
		t.Fatal("a wrong token was accepted")
	}
	delete(cfg.System.Users, "orch") // the user is gone: the token too
	if _, ok := webTokenAuth(cfg, "s3cret-token"); ok {
		t.Fatal("a token of a removed user was accepted")
	}
}
