// Package sysexec runs system tools (nft, ip, bridge, systemctl …) with a
// deadline: at the deadline the tool and everything it started are killed
// and the caller gets an error. Waiting for the killed tool is bounded too
// (hwio), since a process that waits inside the kernel cannot die.
package sysexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Default is the deadline of a tool.
var Default = 10 * time.Second

// grace: how long a killed tool may take to go away.
const grace = 5 * time.Second

// Cmd is a tool to run.
type Cmd struct {
	Name string
	Args []string
	// Timeout: 0 is Default.
	Timeout time.Duration
	Stdin   io.Reader
	// Env: nil inherits.
	Env []string
	Dir string
}

// Command returns a Cmd.
func Command(name string, args ...string) *Cmd { return &Cmd{Name: name, Args: args} }

// WithTimeout sets the deadline.
func (c *Cmd) WithTimeout(d time.Duration) *Cmd { c.Timeout = d; return c }

// WithStdin sets the input.
func (c *Cmd) WithStdin(r io.Reader) *Cmd { c.Stdin = r; return c }

func (c *Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Run runs the tool; stdout and stderr are returned (stderr in the error
// as well when the tool fails).
func (c *Cmd) Run(ctx context.Context) (stdout, stderr []byte, err error) {
	d := c.Timeout
	if d <= 0 {
		d = Default
	}
	type out struct{ o, e []byte }
	r, err := hwio.DoCtx(ctx, "exec "+c.Name, c.String(), d+grace, func() (out, error) {
		cctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		cmd := exec.CommandContext(cctx, c.Name, c.Args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// The tool's process group: helpers it started go too.
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = grace / 2
		cmd.Stdin, cmd.Env, cmd.Dir = c.Stdin, c.Env, c.Dir
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = &hwio.Error{Resource: "exec " + c.Name, Op: c.String() + " (killed)", After: d}
		} else if err != nil {
			if msg := strings.TrimSpace(e.String()); msg != "" {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					err = fmt.Errorf("%s: %w: %s", c.Name, err, msg)
				}
			}
		}
		return out{o.Bytes(), e.Bytes()}, err
	})
	return r.o, r.e, err
}

// Output runs the tool and returns its standard output.
func (c *Cmd) Output(ctx context.Context) ([]byte, error) {
	o, _, err := c.Run(ctx)
	return o, err
}

// CombinedOutput runs the tool and returns standard output followed by
// standard error.
func (c *Cmd) CombinedOutput(ctx context.Context) ([]byte, error) {
	o, e, err := c.Run(ctx)
	return append(o, e...), err
}

// Output runs name with args (Default deadline) and returns its output.
func Output(name string, args ...string) ([]byte, error) {
	return Command(name, args...).Output(context.Background())
}

// CombinedOutput runs name with args (Default deadline).
func CombinedOutput(name string, args ...string) ([]byte, error) {
	return Command(name, args...).CombinedOutput(context.Background())
}
