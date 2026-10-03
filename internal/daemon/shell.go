package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/rpcserver"
	"github.com/thxrben/cerium-switchd/internal/rshell"
	"github.com/thxrben/cerium-switchd/internal/stack"
)

// shells serves "start shell" on another member (reference 1.8): swcli
// asks its own switchd on the shell socket, which passes the stream to the
// member over the stacking protocol; that member runs the shell on a pty.
// Only super-users (and root) get a shell.
type shells struct {
	member    int
	vc        *stack.Manager
	authorize rpcserver.Authorizer
	log       *slog.Logger
}

// run listens on the shell socket next to the CLI socket and on the
// stacking protocol until ctx is done.
func (s *shells) run(ctx context.Context, cliSocket string) error {
	l, err := listen(filepath.Join(filepath.Dir(cliSocket), rshell.SocketName))
	if err != nil {
		return err
	}
	var ml net.Listener
	if s.vc != nil && s.vc.Mesh() != nil {
		ml = s.vc.Mesh().Listen("shell")
	}
	go func() {
		<-ctx.Done()
		l.Close()
		if ml != nil {
			ml.Close()
		}
	}()
	if ml != nil {
		go func() {
			for {
				nc, err := ml.Accept()
				if err != nil {
					return
				}
				go s.serveMember(nc)
			}
		}()
	}
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil
		}
		go s.serveLocal(c)
	}
}

// serveLocal handles swcli's request: it checks the user and connects the
// stream with the member's shell.
func (s *shells) serveLocal(c *net.UnixConn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fail := func(format string, args ...any) {
		_ = rshell.WriteLine(c, rshell.Status{Err: fmt.Sprintf(format, args...)})
	}
	uid, err := peerUID(c)
	if err != nil {
		return
	}
	name := strconv.Itoa(uid)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	class, err := s.authorize(uid, name)
	if err != nil {
		fail("%v", err)
		return
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req rshell.Request
	if err := rshell.ReadLine(r, &req); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	if class != commit.SuperUser {
		fail("permission denied: start shell")
		return
	}
	if req.Member == s.member {
		fail("the shell of this member is started by swcli itself")
		return
	}
	nc, err := s.vc.Mesh().Dial(req.Member, "shell", 5*time.Second)
	if err != nil {
		fail("member %d cannot be reached: %v", req.Member, err)
		return
	}
	defer nc.Close()
	if err := rshell.WriteLine(nc, rshell.Hello{User: name, Class: class.String(), From: s.member, Rows: req.Rows, Cols: req.Cols, Term: req.Term}); err != nil {
		fail("member %d: %v", req.Member, err)
		return
	}
	// The member's status line and the frames that follow go through as
	// they are.
	go func() {
		_, _ = io.Copy(c, nc)
		c.CloseWrite()
	}()
	_, _ = io.Copy(nc, r)
}

// serveMember starts a shell for a user of another member.
func (s *shells) serveMember(nc net.Conn) {
	defer nc.Close()
	r := bufio.NewReader(nc)
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var h rshell.Hello
	if err := rshell.ReadLine(r, &h); err != nil {
		return
	}
	nc.SetReadDeadline(time.Time{})
	if commit.ParseClass(h.Class) != commit.SuperUser || h.User == "" {
		_ = rshell.WriteLine(nc, rshell.Status{Err: "permission denied: start shell"})
		return
	}
	if _, err := user.Lookup(h.User); err != nil {
		_ = rshell.WriteLine(nc, rshell.Status{Err: fmt.Sprintf("user %s has no account on member %d", h.User, s.member)})
		return
	}
	if err := rshell.WriteLine(nc, rshell.Status{}); err != nil {
		return
	}
	s.log.Info("shell started for a user of another member", "facility", "authorization", "user", h.User, "member", h.From)
	term := h.Term
	if term == "" {
		term = "xterm"
	}
	cmd := exec.Command("/usr/sbin/runuser", "-s", "/bin/bash", "-l", h.User)
	cmd.Env = []string{"TERM=" + term, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "SWITCHD_SHELL=cli"}
	if err := rshell.Serve(nc, r, cmd, h.Rows, h.Cols); err != nil {
		s.log.Warn("shell", "user", h.User, "member", h.From, "err", err)
	}
	s.log.Info("shell ended", "facility", "authorization", "user", h.User, "member", h.From)
}

// peerUID returns the uid of the process on the other end of c.
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Uid), nil
}
