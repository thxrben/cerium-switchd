package rpcserver

import (
	"context"
	"io"
	"testing"

	"github.com/thxrben/cerium-switchd/internal/rpc"
)

type discard struct{ io.Reader }

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Close() error                { return nil }

// An answer that arrives after its question was given up (Ctrl-C) must
// not answer the next question.
func TestStaleAnswerDropped(t *testing.T) {
	c := newConn(discard{})
	c.replies <- rpc.Msg{T: "answer", Err: "interrupted"} // the late answer
	tm := &term{c: c, ctx: context.Background()}
	go func() {
		// The client answers the new question.
		for len(c.replies) != 0 {
		}
		c.replies <- rpc.Msg{T: "answer", Text: "yes"}
	}()
	a, err := tm.Ask("Continue? ", true)
	if err != nil || a != "yes" {
		t.Fatalf("got %q %v", a, err)
	}
}
