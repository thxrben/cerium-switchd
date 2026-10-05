// Command cer-bgpd runs BGP for every routing instance (reference 5.14,
// 5.8): the protocol on the master, its routes in cer-ribd on every member
// (the master replicates them over the stacking protocol).
package main

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/thxrben/cerium-switchd/apps/cer-bgpd/internal/bgpd"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bfdapi"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bgpapi"
	"github.com/thxrben/cerium-switchd/lib/platform/api/ribapi"
	"github.com/thxrben/cerium-switchd/lib/platform/daemonkit"
	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
)

func main() {
	daemonkit.Main("cer-bgpd", setup)
}

func setup(k *daemonkit.Kit) error {
	rc := ribapi.Client{C: k.Endpoint.Dial(k.Ctx, k.SocketOf("cer-ribd"))}
	d := bgpd.New(bgpd.LinuxNet{}, rc, k.Log)
	d.Member, d.StackCall = k.Member, k.StackCall
	// cer-ribd (re)connected: it may have restarted and lost the routes.
	rc.C.OnConnect(func(*ipc.Conn) { d.ResyncRIB() })
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
	d.BFD = bfdapi.Client{C: bc}
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
	// The master's routes for the other members (PLAN 15b: changes by
	// prefix; whole tables only to members of an older release).
	d.Members = func() []int {
		r, ok := k.Role()
		if !ok || !r.Master {
			return nil
		}
		var out []int
		for _, m := range r.Reachable {
			if m != k.Member {
				out = append(out, m)
			}
		}
		return out
	}
	d.MemberDelta = func(ctx context.Context, m int, dl ribapi.RoutesDelta) (ribapi.DeltaReply, error) {
		var out ribapi.DeltaReply
		err := k.StackCall(ctx, m, bgpd.StackRoutesDelta, dl, &out)
		return out, err
	}
	d.MemberSetRoutes = func(ctx context.Context, m int, sr ribapi.SetRoutes) error {
		return k.StackCall(ctx, m, bgpd.StackRoutes, sr, nil)
	}
	k.HandleStack(bgpd.StackRoutesDelta, func(ctx context.Context, from int, raw json.RawMessage) (any, error) {
		if r, ok := k.Role(); ok && (r.Master || r.MasterID != from) {
			return ribapi.DeltaReply{}, nil // only the master's routes count
		}
		var dl ribapi.RoutesDelta
		if err := json.Unmarshal(raw, &dl); err != nil {
			return nil, err
		}
		return rc.Delta(ctx, dl)
	})
	k.HandleStack(bgpd.StackRoutes, func(ctx context.Context, from int, raw json.RawMessage) (any, error) {
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
		var c bgpapi.Config
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
	k.Endpoint.Handle(bgpapi.MethodStatus, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var inst *string
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &inst); err != nil {
				return nil, err
			}
		}
		return d.Status(inst), nil
	})
	k.Endpoint.Handle(bgpapi.MethodCounts, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return d.Counts(), nil
	})
	k.Endpoint.Handle(bgpapi.MethodAdj, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q bgpapi.AdjRequest
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		return d.Adj(q)
	})
	k.Endpoint.Handle(bgpapi.MethodClear, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q bgpapi.ClearRequest
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
