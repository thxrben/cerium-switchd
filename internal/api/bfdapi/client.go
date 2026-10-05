package bfdapi

import (
	"context"

	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// Client calls cer-bfdd on this member (the routing protocols use it).
type Client struct{ C *ipc.Client }

// Set replaces the client's sessions.
func (b Client) Set(ctx context.Context, s Set) error {
	return b.C.Call(ctx, MethodSet, s, nil)
}
