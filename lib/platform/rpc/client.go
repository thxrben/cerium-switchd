package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Handler performs the client-side parts of a command: questions, reading
// terminal input and file access (with the user's own permissions).
type Handler interface {
	Ask(prompt string, echo bool) (string, error)
	ReadText(prompt string) (string, error)
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
	// Notify shows an asynchronous message; it may be called at any time
	// from another goroutine. The client's prompt and banner are already
	// updated when it is called.
	Notify(text string)
	// Disconnected is called (from another goroutine) when the connection
	// to switchd breaks, but not after Close.
	Disconnected(c *Client)
}

// Client is a connection to switchd.
type Client struct {
	conn    net.Conn
	wmu     sync.Mutex
	enc     *json.Encoder
	msgs    chan Msg
	h       Handler
	done    chan struct{}
	closing atomic.Bool

	smu            sync.Mutex
	prompt, banner string
	// Class is the permission class of the session (from the hello).
	Class string
}

// ErrOffline is returned by an offline client.
var ErrOffline = errors.New("switchd is not available")

// Offline returns a client without a connection, showing prompt. Every
// request fails with ErrOffline.
func Offline(prompt string) *Client {
	c := &Client{done: make(chan struct{}), prompt: prompt}
	c.closing.Store(true)
	close(c.done)
	return c
}

// State returns the prompt and the banner for the next line.
func (c *Client) State() (prompt, banner string) {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.prompt, c.banner
}

func (c *Client) setState(m Msg) {
	c.smu.Lock()
	c.prompt, c.banner = m.Prompt, m.Banner
	c.smu.Unlock()
}

// Closed reports whether the connection is gone.
func (c *Client) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// helloTimeout bounds the wait for switchd's greeting (a hung switchd).
var helloTimeout = 10 * time.Second

// ErrRejected is wrapped by Dial when switchd refuses the login.
var ErrRejected = errors.New("login rejected")

// Dial connects to the socket at path.
func Dial(path string, h Handler) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, enc: json.NewEncoder(conn), msgs: make(chan Msg, 16), h: h, done: make(chan struct{})}
	c.closing.Store(true) // no Disconnected before the hello
	go c.read()
	var m Msg
	var ok bool
	select {
	case m, ok = <-c.msgs:
	case <-time.After(helloTimeout):
		c.Close()
		return nil, errors.New("switchd does not respond")
	}
	switch {
	case !ok:
		conn.Close()
		return nil, errors.New("connection closed by switchd")
	case m.T == "hello":
		c.setState(m)
		c.Class = m.Name
		c.closing.Store(false)
		if c.Closed() { // lost right after the hello
			return nil, errors.New("connection closed by switchd")
		}
		return c, nil
	default:
		c.Close()
		return nil, fmt.Errorf("%w: %s", ErrRejected, strings.TrimSpace(strings.TrimPrefix(m.Text, "error: ")))
	}
}

func (c *Client) read() {
	defer func() {
		close(c.done)
		close(c.msgs)
		if !c.closing.Load() {
			c.h.Disconnected(c)
		}
	}()
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64<<10), MaxMsg)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			return
		}
		if m.T == "notify" {
			if m.Prompt != "" {
				c.setState(m)
			}
			c.h.Notify(m.Text)
			continue
		}
		if m.Exit {
			c.closing.Store(true) // switchd ends the session: not a lost connection
		}
		c.msgs <- m
	}
}

// Close ends the session.
func (c *Client) Close() error {
	c.closing.Store(true)
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Abort drops the connection as if switchd had gone away (Disconnected is
// called), e.g. when switchd does not react to an interrupt.
func (c *Client) Abort() {
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *Client) send(m Msg) error {
	if c.conn == nil {
		return ErrOffline
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(m)
}

// Interrupt cancels the running command (Ctrl-C).
func (c *Client) Interrupt() { _ = c.send(Msg{T: "interrupt"}) }

// next waits for the next non-notification message.
func (c *Client) next() (Msg, error) {
	m, ok := <-c.msgs
	if !ok {
		return m, io.ErrUnexpectedEOF
	}
	return m, nil
}

// Exec runs a command line and returns the final "done" message.
func (c *Client) Exec(line string) (Msg, error) {
	if err := c.send(Msg{T: "exec", Line: line}); err != nil {
		return Msg{}, err
	}
	for {
		m, err := c.next()
		if err != nil {
			return m, err
		}
		switch m.T {
		case "done":
			c.setState(m)
			return m, nil
		case "ask":
			a, err := c.h.Ask(m.Prompt, m.Echo)
			err = c.reply("answer", a, nil, err)
			if err != nil {
				return m, err
			}
		case "print":
			// Output of a running command (e.g. a software update).
			if p, ok := c.h.(interface{ Print(string) }); ok {
				p.Print(m.Text)
			}
		case "readtext":
			a, err := c.h.ReadText(m.Prompt)
			if err := c.reply("answer", a, nil, err); err != nil {
				return m, err
			}
		case "readfile":
			data, err := c.h.ReadFile(m.Name)
			if err := c.reply("file", "", data, err); err != nil {
				return m, err
			}
		case "writefile":
			err := c.h.WriteFile(m.Name, m.Data)
			if err := c.reply("file", "", nil, err); err != nil {
				return m, err
			}
		}
	}
}

func (c *Client) reply(t, text string, data []byte, err error) error {
	m := Msg{T: t, Text: text, Data: data}
	if err != nil {
		m.Err = err.Error()
	}
	return c.send(m)
}

// Complete returns completions for line (the text before the cursor).
func (c *Client) Complete(line string) ([]Item, error) {
	m, err := c.query("complete", line)
	return m.Items, err
}

// Help returns the "?" output for line.
func (c *Client) Help(line string) (string, error) {
	m, err := c.query("help", line)
	return m.Text, err
}

func (c *Client) query(t, line string) (Msg, error) {
	if err := c.send(Msg{T: t, Line: line}); err != nil {
		return Msg{}, err
	}
	for {
		m, err := c.next()
		if err != nil || m.T == "completions" {
			return m, err
		}
	}
}
