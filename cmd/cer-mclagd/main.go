// Command cer-mclagd runs this member's side of its MC-LAG pair (reference
// 5.6, 1.9): leg states with the peer, MAC synchronisation, the split
// horizon and failover (holds). It talks to cer-lacpd directly and to the
// peer's cer-mclagd over the stacking protocol (relayed by switchd).
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/mclag"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

func main() { daemonkit.Main("cer-mclagd", setup) }

// kitStack is mclag.Stack over switchd's relay.
type kitStack struct{ k *daemonkit.Kit }

func (s kitStack) Reachable() []int {
	r, _ := s.k.Role()
	return r.Reachable
}

func (s kitStack) Call(member int, method string, req any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(s.k.Ctx, timeout)
	defer cancel()
	var out json.RawMessage
	err := s.k.StackCall(ctx, member, method, req, &out)
	return out, err
}

func (s kitStack) Handle(method string, h func(from int, req json.RawMessage) (any, error)) {
	s.k.HandleStack(method, func(_ context.Context, from int, req json.RawMessage) (any, error) { return h(from, req) })
}

func setup(k *daemonkit.Kit) error {
	dir := k.SocketDir
	if dir == "" {
		dir = svc.SocketDir
	}
	lacp := mclag.NewLACPClient(k.Ctx, k.Endpoint, dir)
	ctl := mclag.New(k.Member, lacp, kitStack{k}, k.Log)
	k.OnConfig(func(raw json.RawMessage) {
		var c mclag.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		ctl.SetConfig(&c)
	})
	k.Subscribe(svc.TopicMaintenance, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var on bool
		if !ev.Deleted {
			json.Unmarshal(ev.Value, &on)
		}
		ctl.SetMaintenance(on, time.Now())
	})
	// cer-lacpd calls these before a leg carries traffic and before a held
	// leg's last port leaves.
	k.Endpoint.Handle(svc.MethodBeforeJoin, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var b string
		json.Unmarshal(raw, &b)
		ctl.BeforeJoin(b)
		return nil, nil
	})
	k.Endpoint.Handle(svc.MethodBeforeLeave, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var b string
		json.Unmarshal(raw, &b)
		ctl.BeforeLeave(b)
		return nil, nil
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return ctl.Status()
	})
	k.Endpoint.Handle(svc.MethodLegsUp, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return ctl.LegsUp(), nil
	})
	k.Endpoint.Handle(svc.MethodDrainBlockers, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var draining []int
		json.Unmarshal(raw, &draining)
		return ctl.DrainBlockers(draining), nil
	})
	go ctl.Run(k.Ctx)
	return nil
}
