package daemon

import (
	"net/netip"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/model"
)

func TestWebAuth(t *testing.T) {
	h, err := access.HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{System: model.System{
		Users: map[string]*model.User{
			"admin": {Name: "admin", Class: "super-user", PasswordHash: h},
			"op":    {Name: "op", Class: "operator", PasswordHash: h},
			"key":   {Name: "key", Class: "super-user"}, // keys only
		},
		Root: &model.User{Name: "root", Class: "super-user", PasswordHash: h},
		SSH:  model.SSHService{RootLogin: "deny"},
	}}
	if u, ok := webAuth(cfg, "admin", "secret"); !ok || !u.SuperUser {
		t.Fatalf("admin: %+v %v", u, ok)
	}
	if u, ok := webAuth(cfg, "op", "secret"); !ok || u.SuperUser {
		t.Fatalf("operator: %+v %v", u, ok)
	}
	for _, c := range [][2]string{{"admin", "wrong"}, {"key", ""}, {"nobody", "secret"}, {"root", "secret"}} {
		if _, ok := webAuth(cfg, c[0], c[1]); ok {
			t.Fatalf("%s logged in", c[0])
		}
	}
	cfg.System.SSH.RootLogin = "allow"
	if u, ok := webAuth(cfg, "root", "secret"); !ok || !u.SuperUser {
		t.Fatal("root with root-login allow")
	}
	if _, ok := webAuth(nil, "admin", "secret"); ok {
		t.Fatal("no configuration")
	}
}

func TestWebConfig(t *testing.T) {
	cfg := &model.Config{System: model.System{MgmtInstance: "mgmt", HostName: "sw",
		Web: model.WebService{Enabled: true, Port: 8443, UploadLimit: 1 << 30}},
		L3: map[string]*model.L3Unit{
			"cme.0":   {Name: "cme.0", Instance: "mgmt", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}},
			"1/0/1.0": {Name: "1/0/1.0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/24")}},
		}}
	if webConfig(cfg, false) != nil {
		t.Fatal("runs on a non-master")
	}
	c := webConfig(cfg, true)
	if c == nil || c.Port != 8443 || c.VRF != "mgmt" || len(c.Names) != 1 || c.Names[0].String() != "10.0.0.5" {
		t.Fatalf("%+v", c)
	}
	cfg.System.MgmtInstance = ""
	if webConfig(cfg, true) != nil {
		t.Fatal("runs without a management instance")
	}
}
