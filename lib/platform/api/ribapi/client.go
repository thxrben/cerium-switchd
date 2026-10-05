package ribapi

import (
	"context"

	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
	"github.com/thxrben/cerium-switchd/lib/rib"
)

// Client calls cer-ribd on this member (the routing protocols use it).
type Client struct{ C *ipc.Client }

// SetRoutes gives a protocol's routes of one source.
func (r Client) SetRoutes(ctx context.Context, sr SetRoutes) error {
	return r.C.Call(ctx, svc.MethodRoutesSet, sr, nil)
}

// Delta gives a protocol's changes by prefix.
func (r Client) Delta(ctx context.Context, d RoutesDelta) (DeltaReply, error) {
	var out DeltaReply
	err := r.C.Call(ctx, svc.MethodRoutesDelta, d, &out)
	return out, err
}

// Active returns the active routes of an instance (IPv4 and IPv6).
func (r Client) Active(ctx context.Context, instance string) ([]rib.Entry, error) {
	var out []rib.Entry
	q := rib.Query{Active: true, Tables: []rib.Table{{Instance: instance}, {Instance: instance, V6: true}}}
	err := r.C.Call(ctx, svc.MethodRoutes, q, &out)
	return out, err
}
