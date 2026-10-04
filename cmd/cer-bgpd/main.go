// Command cer-bgpd runs BGP for every routing instance (reference 5.14,
// 5.8): the protocol on the master, its routes in cer-ribd on every member
// (the master replicates them over the stacking protocol).
package main

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bgpd"
	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func main() {
	daemonkit.Main("cer-bgpd", setup)
}

// ribClient calls cer-ribd on this member.
type ribClient struct{ c *ipc.Client }

func (r ribClient) SetRoutes(ctx context.Context, sr ribd.SetRoutes) error {
	return r.c.Call(ctx, svc.MethodRoutesSet, sr, nil)
}

func (r ribClient) Active(ctx context.Context, instance string) ([]rib.Entry, error) {
	var out []rib.Entry
	q := rib.Query{Active: true, Tables: []rib.Table{{Instance: instance}, {Instance: instance, V6: true}}}
	err := r.c.Call(ctx, svc.MethodRoutes, q, &out)
	return out, err
}

func setup(k *daemonkit.Kit) error {
	rc := ribClient{k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-ribd"))}
	d := bgpd.New(bgpd.LinuxNet{}, rc, k.Log)
	// The master's routes for the other members.
	d.Replicate = func(sr ribd.SetRoutes) {
		r, ok := k.Role()
		if !ok || !r.Master {
			return
		}
		for _, m := range r.Reachable {
			ctx, cancel := context.WithTimeout(k.Ctx, 5*time.Second)
			if err := k.StackCall(ctx, m, bgpd.StackRoutes, sr, nil); err != nil {
				k.Log.Debug("bgp routes to member", "member", m, "err", err)
			}
			cancel()
		}
	}
	k.HandleStack(bgpd.StackRoutes, func(ctx context.Context, from int, raw json.RawMessage) (any, error) {
		if r, ok := k.Role(); ok && (r.Master || r.MasterID != from) {
			return nil, nil // only the master's routes count
		}
		var sr ribd.SetRoutes
		if err := json.Unmarshal(raw, &sr); err != nil {
			return nil, err
		}
		return nil, rc.SetRoutes(ctx, sr)
	})
	k.OnConfig(func(raw json.RawMessage) {
		var c bgpd.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		d.SetConfig(c)
	})
	var lastReachable []int
	k.OnRole(func(r svc.Role) {
		d.SetMaster(r.Master)
		if r.Master && !slices.Equal(r.Reachable, lastReachable) {
			lastReachable = slices.Clone(r.Reachable)
			d.Resend()
		}
	})
	k.Endpoint.Handle(bgpd.MethodStatus, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var inst *string
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &inst); err != nil {
				return nil, err
			}
		}
		return d.Status(inst), nil
	})
	k.Endpoint.Handle(bgpd.MethodAdj, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q bgpd.AdjRequest
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.Adj(q)
	})
	k.Endpoint.Handle(bgpd.MethodClear, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q bgpd.ClearRequest
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.Clear(q)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.Status(nil), nil
	})
	go d.Run(k.Ctx)
	return nil
}
