// Command cer-bgpd runs BGP for every routing instance (reference 5.14,
// 5.8): the protocol on the master, its routes in cer-ribd on every member
// (the master replicates them over the stacking protocol).
package main

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/thxrben/cerium-switchd/internal/bfdd"
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

// bfdClient calls cer-bfdd on this member.
type bfdClient struct{ c *ipc.Client }

func (b bfdClient) Set(ctx context.Context, s bfdd.Set) error {
	return b.c.Call(ctx, bfdd.MethodSet, s, nil)
}

func setup(k *daemonkit.Kit) error {
	rc := ribClient{k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-ribd"))}
	d := bgpd.New(bgpd.LinuxNet{}, rc, k.Log)
	d.Member, d.StackCall = k.Member, k.StackCall
	d.Full = func(purpose string, full bool) {
		id := "cer-bgpd/memory " + purpose
		if full {
			k.Alarm(id, "Major", "memory slots of "+purpose+" are full: further routes are not stored (system memory)")
		} else {
			k.ClearAlarm(id)
		}
	}
	// Sessions to routed ports of other members (reference 5.8).
	k.HandleStack(bgpd.StackRelayOpen, func(_ context.Context, from int, raw json.RawMessage) (any, error) {
		var o bgpd.RelayOpen
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		return nil, d.RelayOpened(from, o)
	})
	k.HandleStack(bgpd.StackRelayDial, func(ctx context.Context, from int, raw json.RawMessage) (any, error) {
		var q bgpd.RelayDial
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.RelayDialed(ctx, from, q)
	})
	k.HandleStack(bgpd.StackRelayData, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var m bgpd.RelayData
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return nil, d.RelayedData(m)
	})
	k.HandleStack(bgpd.StackBFDState, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var st bgpd.RelayBFDState
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, err
		}
		d.RelayedBFDState(st)
		return nil, nil
	})
	k.HandleStack(bgpd.StackRelayClose, func(_ context.Context, _ int, raw json.RawMessage) (any, error) {
		var m bgpd.RelayClose
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		d.RelayedClose(m)
		return nil, nil
	})
	// BFD for the neighbours (cer-bfdd runs while BFD is configured; the
	// sessions are set again whenever it (re)starts).
	bc := k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-bfdd"))
	d.BFD = bfdClient{bc}
	bc.OnConnect(func(*ipc.Conn) { d.ResendBFD() })
	bc.Subscribe(bfdd.TopicSessions, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var st bfdd.State
		if !ev.Deleted && json.Unmarshal(ev.Value, &st) != nil {
			return
		}
		d.BFDChanged(ev.Key, st.Up, ev.Deleted)
	})
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
		d.SetRole(r.Master, r.MasterID)
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
