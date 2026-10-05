//go:build linux

package rshell

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"golang.org/x/sys/unix"
)

// Client connects a terminal (in, out) with a remote shell on rw (after
// the request and status lines). resize delivers new window sizes. It
// returns the shell's exit status when it ends. in is read with poll, so
// no input is consumed after the shell ended.
func Client(rw io.ReadWriter, r *bufio.Reader, in *os.File, out io.Writer, resize <-chan [2]uint16) (int, error) {
	w := &writer{w: rw}
	stop := make(chan struct{})
	exited := make(chan struct{})
	// The input reader ends before Client returns: nothing typed after
	// the shell ended is consumed.
	defer func() { close(stop); <-exited }()
	go func() {
		defer close(exited)
		fd := int(in.Fd())
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			case sz := <-resize:
				if w.resize(sz[0], sz[1]) != nil {
					return
				}
				continue
			default:
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, 100)
			if err != nil && !errors.Is(err, unix.EINTR) {
				return
			}
			if n == 0 || fds[0].Revents == 0 {
				continue
			}
			select {
			case <-stop:
				return
			default:
			}
			k, err := unix.Read(fd, buf)
			if k <= 0 || err != nil {
				return
			}
			if w.frame(frameData, buf[:k]) != nil {
				return
			}
		}
	}()
	for {
		t, p, err := readFrame(r)
		if err != nil {
			return -1, ErrNoExit
		}
		switch t {
		case frameData:
			if _, err := out.Write(p); err != nil {
				return -1, err
			}
		case frameExit:
			if len(p) == 4 {
				return int(int32(binary.BigEndian.Uint32(p))), nil
			}
			return 0, nil
		}
	}
}

// Serve runs cmd on a new pty of size rows × cols and connects it with rw
// (after the hello line, whose reader is r) until the shell ends; then it
// sends the exit status. A broken connection hangs the shell up.
func Serve(rw io.ReadWriteCloser, r *bufio.Reader, cmd *exec.Cmd, rows, cols uint16) error {
	master, slave, err := openPTY()
	if err != nil {
		return err
	}
	defer master.Close()
	setSize(master, rows, cols)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	slave.Close()
	if err != nil {
		return err
	}
	w := &writer{w: rw}
	// Input: frames to the pty.
	go func() {
		for {
			t, p, err := readFrame(r)
			if err != nil {
				// The user is gone: hang up (SIGHUP to the session).
				_ = cmd.Process.Signal(syscall.SIGHUP)
				return
			}
			switch t {
			case frameData:
				if _, err := master.Write(p); err != nil {
					return
				}
			case frameResize:
				if len(p) == 4 {
					setSize(master, binary.BigEndian.Uint16(p[:2]), binary.BigEndian.Uint16(p[2:]))
				}
			}
		}
	}()
	// Output: the pty to frames, until the shell (and every process that
	// still holds the terminal) has closed it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 && w.frame(frameData, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	werr := cmd.Wait()
	select {
	case <-done:
	case <-time.After(time.Second): // background processes keep the pty open
	}
	status := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		status = ee.ExitCode()
	}
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(int32(status)))
	_ = w.frame(frameExit, p[:])
	return rw.Close()
}

func openPTY() (*os.File, *os.File, error) {
	m, err := hwio.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("unlocking the pty: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("pty number: %w", err)
	}
	s, err := hwio.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func setSize(f *os.File, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	_ = unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}
