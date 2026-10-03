// Command cer-rstpd runs RSTP with the stack as one bridge (reference 5.5,
// 1.9): on every member the ports' facts, BPDU sockets and kernel port
// states; on the owner (the lowest member reached) the state machines of
// all ports. A restart is hitless: the kernel keeps the port states and
// the owner continues from its last copy.
package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/stp"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

func main() { daemonkit.Main("cer-rstpd", setup) }

// kitStack is stp.Stack over switchd's relay.
type kitStack struct{ k *daemonkit.Kit }

func (s kitStack) Reachable() []int {
	r, _ := s.k.Role()
	return r.Reachable
}

func (s kitStack) Draining() []int {
	r, _ := s.k.Role()
	return r.Draining
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
	// An LACP bundle's leg carries nothing while held or negotiating: its
	// state comes from cer-lacpd.
	var mu sync.Mutex
	legs := map[string]bool{}
	lacp := k.Endpoint.Dial(k.Ctx, svc.Socket(dir, "cer-lacpd"))
	lacp.Subscribe(svc.TopicLACPLegs, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var v bool
		json.Unmarshal(ev.Value, &v)
		mu.Lock()
		if ev.Deleted {
			delete(legs, ev.Key)
		} else {
			legs[ev.Key] = v
		}
		mu.Unlock()
	})
	ctl := stp.New(k.Member, kitStack{k}, k.StateDir, k.Log)
	ctl.Legs = func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(legs))
		for b, v := range legs {
			out[b] = v
		}
		return out
	}
	k.OnConfig(func(raw json.RawMessage) {
		var c stp.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		ctl.SetConfig(&c)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return ctl.Status()
	})
	go ctl.Run(k.Ctx)
	return nil
}
