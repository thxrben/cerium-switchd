// Package rshell runs a Linux shell on another stack member ("start shell"
// in a session forwarded to the master, reference 1.8). swcli talks to its
// own switchd on the shell socket; switchd passes the stream on to the
// member over the stacking protocol, where the shell runs on a new pty.
//
// The stream starts with one JSON line (Request from swcli, Hello between
// the members), followed by frames in both directions: one type byte, a
// 32-bit big-endian length and the payload.
//
//	'd' data (terminal input, or shell output)
//	'w' window size: rows, columns (16 bits each)
//	'x' the shell ended: exit status (32 bits); the last frame
package rshell

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// SocketName is the shell socket, next to switchd's CLI socket.
const SocketName = "shell.sock"

// Frame types.
const (
	frameData   = 'd'
	frameResize = 'w'
	frameExit   = 'x'
)

// maxFrame bounds a frame's payload.
const maxFrame = 1 << 20

// Request is what swcli sends on the shell socket.
type Request struct {
	Member int    `json:"member"`
	Rows   uint16 `json:"rows"`
	Cols   uint16 `json:"cols"`
	Term   string `json:"term,omitempty"`
}

// Hello is what the member that starts the shell receives: the user as
// authenticated by the member the user is connected to.
type Hello struct {
	User  string `json:"user"`
	Class string `json:"class"`
	From  int    `json:"from"`
	Rows  uint16 `json:"rows"`
	Cols  uint16 `json:"cols"`
	Term  string `json:"term,omitempty"`
}

// Status is the first line the shell's member answers: "" or an error.
type Status struct {
	Err string `json:"err,omitempty"`
}

// writer serializes frames.
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *writer) frame(t byte, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var h [5]byte
	h[0] = t
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.w.Write(p)
	return err
}

func (w *writer) resize(rows, cols uint16) error {
	var p [4]byte
	binary.BigEndian.PutUint16(p[:2], rows)
	binary.BigEndian.PutUint16(p[2:], cols)
	return w.frame(frameResize, p[:])
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// WriteLine sends v as one JSON line.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadLine reads one JSON line into v.
func ReadLine(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// ErrNoExit: the stream ended without the shell's exit status.
var ErrNoExit = errors.New("the connection to the shell broke")
