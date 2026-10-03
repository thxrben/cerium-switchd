// Command cer-ospfd runs OSPF and OSPFv3 for every routing instance
// (reference 5.13, 5.8): the protocol on the master, its routes in
// cer-ribd on every member (the master replicates them over the stacking
// protocol).
package main

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/ospfd"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func main() {
	daemonkit.Main("cer-ospfd", setup)
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
	d := ospfd.New(ospfd.LinuxKernel{}, ospfd.LinuxNet{}, rc, k.Log)
	d.Member = k.Member
	d.StackCall = k.StackCall
	// The relay of routed interfaces of other members (reference 5.8).
	k.HandleStack(ospfd.StackRx, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var p ospfd.RelayPacket
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		d.ReceiveRelayed(p)
		return nil, nil
	})
	k.HandleStack(ospfd.StackTx, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var p ospfd.RelayPacket
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		d.SendRelayed(p)
		return nil, nil
	})
	k.HandleStack(ospfd.StackLink, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var l ospfd.RelayLink
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, err
		}
		d.LinkReported(l)
		return nil, nil
	})
	// The master's routes for the other members.
	d.Replicate = func(sr ribd.SetRoutes) {
		r, ok := k.Role()
		if !ok || !r.Master {
			return
		}
		for _, m := range r.Reachable {
			ctx, cancel := context.WithTimeout(k.Ctx, 5*time.Second)
			if err := k.StackCall(ctx, m, ospfd.StackRoutes, sr, nil); err != nil {
				k.Log.Debug("ospf routes to member", "member", m, "err", err)
			}
			cancel()
		}
	}
	k.HandleStack(ospfd.StackRoutes, func(ctx context.Context, from int, raw json.RawMessage) (any, error) {
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
		var c ospfd.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		d.SetConfig(c)
	})
	var lastReachable []int
	k.OnRole(func(r svc.Role) {
		d.SetRole(r.Master, r.MasterID)
		// A member that became reachable gets the routes at once.
		if r.Master && !slices.Equal(r.Reachable, lastReachable) {
			lastReachable = slices.Clone(r.Reachable)
			d.Resend()
		}
	})
	k.Endpoint.Handle(ospfd.MethodStatus, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q ospfd.StatusRequest
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &q); err != nil {
				return nil, err
			}
		}
		return d.Status(q)
	})
	k.Endpoint.Handle(ospfd.MethodClear, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q ospfd.ClearRequest
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.Clear(q)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.Status(ospfd.StatusRequest{})
	})
	// Stopping: the neighbours drop the adjacencies at once (hellos without
	// neighbours) instead of after the dead interval.
	k.OnShutdown(d.Shutdown)
	go d.Run(k.Ctx)
	return nil
}
