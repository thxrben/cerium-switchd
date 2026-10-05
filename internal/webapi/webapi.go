// Package webapi is the REST API over HTTPS (system services
// web-management, reference 5.1): it runs on the master inside the
// management instance: software, configuration sessions and commands
// (cli.go).
package webapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/internal/commit"
)

// Config is what the server runs with.
type Config struct {
	Port     int
	CertFile string // "" (with KeyFile ""): a temporary self-signed certificate
	KeyFile  string
	// VRF is the management instance's device ("": the default table,
	// tests only).
	VRF string
	// Addr is the address to listen on ("": every address of the VRF).
	Addr        string
	HostName    string
	Names       []net.IP // the management addresses, for the certificate
	UploadLimit int64
}

// User is an authenticated client.
type User struct {
	Name      string
	SuperUser bool
	Class     commit.Class // its class (4.3): the CLI checks every command with it
}

// Software is what the software endpoints do (the daemon implements it).
type Software interface {
	// Status is GET /api/v1/software.
	Status() (any, error)
	// Upload receives a bundle of size bytes (-1: unknown) from body; it
	// returns the manifest. ErrTooLarge and ErrInvalid select the status.
	Upload(body io.Reader, size, limit int64, user string) (any, error)
	// Install starts an update from the uploaded bundle (ErrConflict: one
	// runs or nothing was uploaded).
	Install(req InstallRequest, user string) error
}

// InstallRequest is the body of POST /api/v1/software/install.
type InstallRequest struct {
	SHA256     string `json:"sha256,omitempty"`
	Member     int    `json:"member,omitempty"`
	NoValidate bool   `json:"no_validate,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

// Errors of Software that select a status.
var (
	ErrTooLarge = errors.New("too large")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// Info is show system services web-management.
type Info struct {
	Running     bool      `json:"running"`
	Port        int       `json:"port,omitempty"`
	VRF         string    `json:"vrf,omitempty"`
	Certificate string    `json:"certificate,omitempty"` // the file, or "temporary self-signed"
	Generated   time.Time `json:"generated,omitzero"`
	Fingerprint string    `json:"fingerprint,omitempty"` // SHA-256 of the certificate
	Pin         string    `json:"pin,omitempty"`         // sha256//<base64 of the public key's SHA-256>
	Error       string    `json:"error,omitempty"`
}

// Server is the REST API.
type Server struct {
	Log *slog.Logger
	// Auth checks a user's credentials (ok false: wrong).
	Auth     func(user, password string) (User, bool)
	Software Software
	sess     sessions
	events   hub

	mu       sync.Mutex
	newShell func(User) Shell // SetShells
	state    StateSource      // SetState
	health   Health
	cfg      Config
	running  bool
	srv      *http.Server
	info     Info
	tmpCert  *tls.Certificate // the self-signed one, kept while the server runs
	fails    map[string][]time.Time
}

// Sync runs the server with cfg, or stops it (cfg nil); it restarts only
// when cfg changed.
func (s *Server) Sync(cfg *Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg == nil {
		s.stopLocked()
		s.info = Info{}
		return
	}
	if s.running && sameConfig(s.cfg, *cfg) {
		s.cfg.UploadLimit = cfg.UploadLimit
		return
	}
	s.stopLocked()
	s.cfg = *cfg
	if err := s.startLocked(); err != nil {
		s.Log.Error("web-management: not started", "err", err)
		s.info = Info{Error: err.Error()}
	}
}

func sameConfig(a, b Config) bool {
	return a.Port == b.Port && a.CertFile == b.CertFile && a.KeyFile == b.KeyFile && a.VRF == b.VRF &&
		a.Addr == b.Addr && a.HostName == b.HostName && fmt.Sprint(a.Names) == fmt.Sprint(b.Names)
}

func (s *Server) stopLocked() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.srv.Shutdown(ctx)
		cancel()
		s.srv = nil
		s.Log.Info("web-management: stopped")
	}
	s.running = false
}

func (s *Server) startLocked() error {
	cert, info, err := s.certificate()
	if err != nil {
		return err
	}
	lc := net.ListenConfig{}
	if s.cfg.VRF != "" {
		vrf := s.cfg.VRF
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) { serr = syscall.BindToDevice(int(fd), vrf) }); err != nil {
				return err
			}
			return serr
		}
	}
	ln, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort(s.cfg.Addr, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.routes(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelDebug),
	}
	s.srv, s.running = srv, true
	info.Running, info.Port, info.VRF = true, s.cfg.Port, s.cfg.VRF
	s.info = info
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error("web-management: server ended", "err", err)
		}
	}()
	s.Log.Info("web-management: REST API runs", "port", s.cfg.Port, "vrf", s.cfg.VRF,
		"certificate", info.Certificate, "fingerprint", info.Fingerprint, "pin", info.Pin)
	return nil
}

// certificate loads the configured certificate or makes a temporary
// self-signed one (held only in memory).
func (s *Server) certificate() (tls.Certificate, Info, error) {
	var info Info
	if s.cfg.CertFile != "" {
		c, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
		if err != nil {
			return c, info, fmt.Errorf("certificate: %w", err)
		}
		info.Certificate = s.cfg.CertFile
		return c, fingerprint(c, info), nil
	}
	c, err := SelfSigned(s.cfg.HostName, s.cfg.Names, time.Now())
	if err != nil {
		return c, info, err
	}
	info.Certificate, info.Generated = "temporary self-signed", time.Now()
	return c, fingerprint(c, info), nil
}

func fingerprint(c tls.Certificate, info Info) Info {
	if len(c.Certificate) == 0 {
		return info
	}
	sum := sha256.Sum256(c.Certificate[0])
	var parts []string
	for _, b := range sum {
		parts = append(parts, strings.ToUpper(hex.EncodeToString([]byte{b})))
	}
	info.Fingerprint = strings.Join(parts, ":")
	if x, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
		pk := sha256.Sum256(x.RawSubjectPublicKeyInfo)
		info.Pin = "sha256//" + base64.StdEncoding.EncodeToString(pk[:])
	}
	return info
}

// SelfSigned makes a self-signed certificate (ECDSA P-256, a year) for the
// host name and addresses.
func SelfSigned(host string, ips []net.IP, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if host == "" {
		host = "cerOS"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"cerOS (temporary self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Info is the server's state.
func (s *Server) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// ---- requests ----

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/software", s.auth(false, func(w http.ResponseWriter, r *http.Request, _ User) {
		st, err := s.Software.Status()
		if err != nil {
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		reply(w, http.StatusOK, st)
	}))
	mux.HandleFunc("PUT /api/v1/software/upload", s.auth(true, s.upload))
	mux.HandleFunc("POST /api/v1/software/install", s.auth(true, s.install))
	{
		mux.HandleFunc("POST /api/v1/cli", s.auth(false, s.cliRun))
		mux.HandleFunc("GET /api/v1/config", s.auth(false, s.configGet))
		mux.HandleFunc("GET /api/v1/config/revisions", s.auth(false, func(w http.ResponseWriter, r *http.Request, u User) {
			s.oneCommand(w, r, u, "show system commit", false)
		}))
		mux.HandleFunc("POST /api/v1/config/confirm", s.authClass(commit.Operator, s.confirm))
		mux.HandleFunc("POST /api/v1/config/sessions", s.auth(true, s.newSession))
		mux.HandleFunc("POST /api/v1/config/sessions/{id}/load", s.auth(true, s.withSession(s.sessionLoad)))
		mux.HandleFunc("POST /api/v1/config/sessions/{id}/commands", s.auth(true, s.withSession(s.sessionCommands)))
		mux.HandleFunc("GET /api/v1/config/sessions/{id}/compare", s.auth(true, s.withSession(s.sessionCompare)))
		mux.HandleFunc("POST /api/v1/config/sessions/{id}/check", s.auth(true, s.withSession(s.sessionCheck)))
		mux.HandleFunc("POST /api/v1/config/sessions/{id}/commit", s.auth(true, s.withSession(s.sessionCommit)))
		mux.HandleFunc("DELETE /api/v1/config/sessions/{id}", s.auth(true, s.withSession(s.sessionDelete)))
	}
	mux.HandleFunc("GET /api/v1/state", s.auth(false, s.stateList))
	mux.HandleFunc("GET /api/v1/state/{name}", s.auth(false, s.stateGet))
	mux.HandleFunc("GET /api/v1/events", s.auth(false, s.eventStream))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.auth(false, s.metrics))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, errors.New("no such endpoint"))
	})
	return mux
}

// authClass is auth for a class (at least need).
func (s *Server) authClass(need commit.Class, h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return s.auth(false, func(w http.ResponseWriter, r *http.Request, u User) {
		if !classOK(u, need) {
			fail(w, http.StatusForbidden, fmt.Errorf("this needs the %s class", need))
			return
		}
		h(w, r, u)
	})
}

// maxFails is how many failed logins one address may have per minute.
const maxFails = 10

// auth checks the credentials (and super-user for changes).
func (s *Server) auth(change bool, h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if s.failures(host, false) >= maxFails {
			fail(w, http.StatusTooManyRequests, errors.New("too many failed logins; try again in a minute"))
			return
		}
		name, pw, ok := r.BasicAuth()
		var u User
		if ok {
			u, ok = s.Auth(name, pw)
		}
		if !ok {
			s.failures(host, true)
			s.Log.Warn("web-management: login failed", "user", name, "from", host)
			w.Header().Set("WWW-Authenticate", `Basic realm="cerOS"`)
			fail(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		if change && !u.SuperUser {
			fail(w, http.StatusForbidden, errors.New("this needs the super-user class"))
			return
		}
		h(w, r, u)
	}
}

// failures counts the failed logins of host in the last minute (add: one
// more).
func (s *Server) failures(host string, add bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails == nil {
		s.fails = map[string][]time.Time{}
	}
	now := time.Now()
	var keep []time.Time
	for _, t := range s.fails[host] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if add {
		keep = append(keep, now)
	}
	if len(keep) == 0 {
		delete(s.fails, host)
	} else {
		s.fails[host] = keep
	}
	return len(keep)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, u User) {
	s.mu.Lock()
	limit := s.cfg.UploadLimit
	s.mu.Unlock()
	if r.ContentLength > limit {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the bundle has %d bytes, upload-limit is %d", r.ContentLength, limit))
		return
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	m, err := s.Software.Upload(body, r.ContentLength, limit, u.Name)
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the bundle exceeds upload-limit (%d bytes)", limit))
	case errors.Is(err, ErrTooLarge):
		fail(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, ErrInvalid):
		fail(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, ErrConflict):
		fail(w, http.StatusConflict, err)
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
	default:
		reply(w, http.StatusOK, m)
	}
}

func (s *Server) install(w http.ResponseWriter, r *http.Request, u User) {
	var req InstallRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			fail(w, http.StatusBadRequest, fmt.Errorf("bad request body: %v", err))
			return
		}
	}
	switch err := s.Software.Install(req, u.Name); {
	case errors.Is(err, ErrConflict):
		fail(w, http.StatusConflict, err)
	case err != nil:
		fail(w, http.StatusBadRequest, err)
	default:
		reply(w, http.StatusAccepted, map[string]string{"status": "started; progress in GET /api/v1/software"})
	}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, map[string]string{"error": err.Error()})
}
