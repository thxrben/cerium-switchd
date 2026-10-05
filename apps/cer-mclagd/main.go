// Command cer-mclagd runs this member's side of its MC-LAG pair (reference
// 5.6, 1.9): leg states with the peer, MAC synchronisation, the split
// horizon and failover (holds). It talks to cer-lacpd directly and to the
// peer's cer-mclagd over the stacking protocol (relayed by switchd).
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/thxrben/cerium-switchd/apps/cer-mclagd/internal/mclag"
	"github.com/thxrben/cerium-switchd/lib/platform/api/mclagapi"
	"github.com/thxrben/cerium-switchd/lib/platform/daemonkit"
	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
)

func main() { daemonkit.Main("cer-mclagd", setup) }

func setup(k *daemonkit.Kit) error {
	dir := k.SocketDir
	if dir == "" {
		dir = svc.SocketDir
	}
	lacp := mclag.NewLACPClient(k.Ctx, k.Endpoint, dir)
	ctl := mclag.New(k.Member, lacp, k.Stack(), k.Log)
	k.OnConfig(func(raw json.RawMessage) {
		var c mclagapi.Config
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
	// Stopping: the legs leave their bundles first (the peer takes over).
	k.OnShutdown(ctl.Drain)
	go ctl.Run(k.Ctx)
	return nil
}
