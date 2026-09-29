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
	// from another goroutine.
	Notify(text string)
}

// Client is a connection to switchd.
type Client struct {
	conn net.Conn
	wmu  sync.Mutex
	enc  *json.Encoder
	msgs chan Msg
	h    Handler
	// Prompt and Banner for the next line, updated after each command.
	Prompt, Banner string
}

// ErrRejected is wrapped by Dial when switchd refuses the login.
var ErrRejected = errors.New("login rejected")

// Dial connects to the socket at path.
func Dial(path string, h Handler) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, enc: json.NewEncoder(conn), msgs: make(chan Msg, 16), h: h}
	go c.read()
	m, ok := <-c.msgs
	switch {
	case !ok:
		conn.Close()
		return nil, errors.New("connection closed by switchd")
	case m.T == "hello":
		c.Prompt, c.Banner = m.Prompt, m.Banner
		return c, nil
	default:
		conn.Close()
		return nil, fmt.Errorf("%w: %s", ErrRejected, strings.TrimSpace(strings.TrimPrefix(m.Text, "error: ")))
	}
}

func (c *Client) read() {
	defer close(c.msgs)
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64<<10), MaxMsg)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			return
		}
		if m.T == "notify" {
			c.h.Notify(m.Text)
			continue
		}
		c.msgs <- m
	}
}

// Close ends the session.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) send(m Msg) error {
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
			c.Prompt, c.Banner = m.Prompt, m.Banner
			return m, nil
		case "ask":
			a, err := c.h.Ask(m.Prompt, m.Echo)
			err = c.reply("answer", a, nil, err)
			if err != nil {
				return m, err
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
