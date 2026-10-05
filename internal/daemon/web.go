package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"io"
	"net"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/internal/webapi"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// webConfig is the REST API's configuration on this member (nil: it does
// not run here; reference 5.1 web-management).
func webConfig(cfg *model.Config, master bool) *webapi.Config {
	w := cfg.System.Web
	mi := cfg.System.MgmtInstance
	if !w.Enabled || !master || mi == "" {
		return nil
	}
	c := &webapi.Config{Port: w.Port, CertFile: w.CertFile, KeyFile: w.KeyFile, VRF: mi,
		HostName: cfg.System.HostName, UploadLimit: int64(w.UploadLimit)}
	for _, u := range cfg.L3 {
		if u.Instance != mi {
			continue
		}
		for _, a := range u.Addrs {
			c.Names = append(c.Names, net.IP(a.Addr().AsSlice()))
		}
	}
	return c
}

// webAuth checks a REST API login against the configuration: a user with
// an encrypted-password, or root when root-login allows passwords.
func webAuth(cfg *model.Config, name, password string) (webapi.User, bool) {
	if cfg == nil {
		return webapi.User{}, false
	}
	u := cfg.System.Users[name]
	if name == "root" {
		u = nil
		if cfg.System.Root != nil && cfg.System.SSH.RootLogin == "allow" {
			u = cfg.System.Root
		}
	}
	if u == nil || u.PasswordHash == "" {
		return webapi.User{}, false
	}
	got, err := access.SHA512Crypt(password, u.PasswordHash)
	if err != nil || subtle.ConstantTimeCompare([]byte(got), []byte(u.PasswordHash)) != 1 {
		return webapi.User{}, false
	}
	cl := commit.ParseClass(u.Class)
	return webapi.User{Name: name, SuperUser: cl == commit.SuperUser, Class: cl}, true
}

// Upload is the bundle uploaded through the REST API.
type Upload struct {
	Version string    `json:"version"`
	Built   string    `json:"built"`
	Arch    string    `json:"arch"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	User    string    `json:"user"`
	Time    time.Time `json:"time"`
}

// webSoftware is the REST API's software endpoints (reference 5.1).
type webSoftware struct {
	u *updater

	mu     sync.Mutex
	upload *Upload
}

func (w *webSoftware) uploaded() *Upload {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.upload == nil {
		return nil
	}
	if _, err := hwio.Stat(w.u.pkgPath("upload")); err != nil {
		w.upload = nil // installed, replaced or swept
		return nil
	}
	c := *w.upload
	return &c
}

func (w *webSoftware) Status() (any, error) {
	st, err := w.u.Status()
	if err != nil {
		return nil, err
	}
	return struct {
		cli.SoftwareStatus
		Upload *Upload `json:"upload,omitempty"`
	}{st, w.uploaded()}, nil
}

func (w *webSoftware) Upload(body io.Reader, size, limit int64, user string) (any, error) {
	if w.u.busy() {
		return nil, fmt.Errorf("%w: an update is running; its bundle cannot be replaced until it is done", webapi.ErrConflict)
	}
	room, err := w.u.store.prepare(uint64(max(size, 0)))
	if errors.Is(err, software.ErrTooLarge) {
		return nil, fmt.Errorf("%w: %v", webapi.ErrTooLarge, err)
	}
	if err != nil {
		return nil, err
	}
	room = min(room, uint64(limit))
	part := w.u.pkgPath("upload") + ".part"
	defer hwio.Remove(part)
	f, err := hwio.Create(part)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, int64(room)+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if uint64(n) > room {
		return nil, fmt.Errorf("%w: the bundle is larger than the room in memory (%s)", webapi.ErrTooLarge, mib(room))
	}
	m, err := verifyBundle(part)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webapi.ErrInvalid, err)
	}
	if m.Arch != software.Arch() {
		return nil, fmt.Errorf("%w: the bundle is for %s, this switch is %s", webapi.ErrInvalid, m.Arch, software.Arch())
	}
	if err := hwio.Rename(part, w.u.pkgPath("upload")); err != nil {
		return nil, err
	}
	w.u.store.touch(w.u.pkgPath("upload"))
	up := &Upload{Version: m.Version, Built: m.Built, Arch: m.Arch, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)),
		User: user, Time: time.Now()}
	w.mu.Lock()
	w.upload = up
	w.mu.Unlock()
	w.u.log.Warn("software: bundle uploaded (REST API)", "facility", "change-log", "user", user, "version", m.Version, "size", n)
	return up, nil
}

func (w *webSoftware) Install(req webapi.InstallRequest, user string) error {
	if w.uploaded() == nil {
		return fmt.Errorf("%w: no bundle was uploaded", webapi.ErrConflict)
	}
	if w.u.busy() {
		return fmt.Errorf("%w: an update is running (GET /api/v1/software)", webapi.ErrConflict)
	}
	w.u.log.Warn("software: update started (REST API)", "facility", "change-log", "user", user)
	return w.u.Start(cli.SoftwareRequest{Source: "upload", SHA256: req.SHA256, Member: req.Member,
		NoValidate: req.NoValidate, Force: req.Force})
}

// WebManagement is show system services web-management.
func (o *ops) WebManagement() (cli.WebStatus, error) {
	var st cli.WebStatus
	if cfg := o.model(); cfg != nil {
		st.Configured = cfg.System.Web.Enabled
	}
	if o.webServer != nil {
		i := o.webServer.Info()
		st.Running, st.Port, st.VRF, st.Certificate, st.Generated = i.Running, i.Port, i.VRF, i.Certificate, i.Generated
		st.Fingerprint, st.Pin, st.Error = i.Fingerprint, i.Pin, i.Error
		st.Member = o.member
	}
	if o.web != nil {
		if up := o.web.uploaded(); up != nil {
			st.Upload.Version, st.Upload.SHA256, st.Upload.User, st.Upload.Size, st.Upload.Time = up.Version, up.SHA256, up.User, up.Size, up.Time
		}
	}
	return st, nil
}
