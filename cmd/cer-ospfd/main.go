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

	"github.com/thxrben/cerium-switchd/internal/api/bfdapi"
	"github.com/thxrben/cerium-switchd/internal/api/ospfapi"
	"github.com/thxrben/cerium-switchd/internal/api/ribapi"
	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/ospfd"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func main() {
	daemonkit.Main("cer-ospfd", setup)
}

// ribClient calls cer-ribd on this member.
type ribClient struct{ c *ipc.Client }

func (r ribClient) SetRoutes(ctx context.Context, sr ribapi.SetRoutes) error {
	return r.c.Call(ctx, svc.MethodRoutesSet, sr, nil)
}

func (r ribClient) Active(ctx context.Context, instance string) ([]rib.Entry, error) {
	var out []rib.Entry
	q := rib.Query{Active: true, Tables: []rib.Table{{Instance: instance}, {Instance: instance, V6: true}}}
	err := r.c.Call(ctx, svc.MethodRoutes, q, &out)
	return out, err
}

// bfdClient calls cer-bfdd on this member.
type bfdClient struct{ c *ipc.Client }

func (b bfdClient) Set(ctx context.Context, s bfdapi.Set) error {
	return b.c.Call(ctx, bfdapi.MethodSet, s, nil)
}

func setup(k *daemonkit.Kit) error {
	rc := ribClient{k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-ribd"))}
	d := ospfd.New(ospfd.LinuxKernel{}, ospfd.LinuxNet{}, rc, k.Log)
	// Graceful restart (reference 5.13): the neighbours kept in /run; a
	// restart by the supervisor is announced to them.
	d.RestartFile, d.Planned = ospfd.RestartFile, k.PlannedRestart
	d.Full = func(purpose string, full bool) {
		id := "cer-ospfd/memory " + purpose
		if full {
			k.Alarm(id, "Major", "OSPF external database overflow (RFC 1765): the memory slots of "+purpose+" are full; own external routes are not announced (system memory)")
		} else {
			k.ClearAlarm(id)
		}
	}
	d.Member = k.Member
	d.StackCall = k.StackCall
	// BFD for the neighbours (cer-bfdd runs while BFD is configured; the
	// sessions are set again whenever it (re)starts).
	bc := k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-bfdd"))
	d.BFD = bfdClient{bc}
	bc.OnConnect(func(*ipc.Conn) { d.ResendBFD() })
	bc.Subscribe(bfdapi.TopicSessions, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var st bfdapi.State
		if !ev.Deleted && json.Unmarshal(ev.Value, &st) != nil {
			return
		}
		d.BFDChanged(ev.Key, st.Up, ev.Deleted)
	})
	k.HandleStack(ospfd.StackBFDSet, func(_ context.Context, from int, raw json.RawMessage) (any, error) {
		if r, ok := k.Role(); ok && (r.Master || r.MasterID != from) {
			return nil, nil // only the master's sessions count
		}
		var r ospfd.RelayBFD
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return d.SetRelayedBFD(from, r), nil
	})
	k.HandleStack(ospfd.StackBFDState, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var st ospfd.RelayBFDState
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, err
		}
		d.RelayedBFDState(st)
		return nil, nil
	})
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
	d.Replicate = func(sr ribapi.SetRoutes) {
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
		var sr ribapi.SetRoutes
		if err := json.Unmarshal(raw, &sr); err != nil {
			return nil, err
		}
		return nil, rc.SetRoutes(ctx, sr)
	})
	k.OnConfig(func(raw json.RawMessage) {
		var c ospfapi.Config
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
	k.Endpoint.Handle(ospfapi.MethodStatus, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q ospfapi.StatusRequest
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &q); err != nil {
				return nil, err
			}
		}
		return d.Status(q)
	})
	k.Endpoint.Handle(ospfapi.MethodClear, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q ospfapi.ClearRequest
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.Clear(q)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.Status(ospfapi.StatusRequest{})
	})
	// Stopping: the neighbours drop the adjacencies at once (hellos without
	// neighbours) instead of after the dead interval.
	k.OnShutdown(d.Shutdown)
	go d.Run(k.Ctx)
	return nil
}
