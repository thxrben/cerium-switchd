package rpc

import (
	"net"

	"golang.org/x/sys/unix"
)

// DefaultSocket is where switchd listens for CLI sessions.
const DefaultSocket = "/run/switchd/cli.sock"

// PeerUID is the user id of the process on the other end of a unix
// socket (SO_PEERCRED: nothing the client sends identifies the user).
func PeerUID(c *net.UnixConn) (int, error) {
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
