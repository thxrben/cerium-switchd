// Command cer-rstpd runs RSTP with the stack as one bridge (reference 5.5,
// 1.9): on every member the ports' facts, BPDU sockets and kernel port
// states; on the owner (the lowest member reached) the state machines of
// all ports. A restart is hitless: the kernel keeps the port states and
// the owner continues from its last copy.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/stp"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// MethodClearSTP is clear spanning-tree protocol-migration|statistics.
const MethodClearSTP = stp.MethodClear

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
	ctl.Alarm = func(text string) {
		go k.Notify(fmt.Sprintf("member %d: %s", k.Member, text)) // never under the controller's lock
	}
	ctl.Legs = func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(legs))
		for b, v := range legs {
			out[b] = v
		}
		return out
	}
	// bpdu-block (reference 5.5): switchd follows the blocked ports.
	guard := stp.NewGuard(stp.LinuxGuardIO{}, stp.GuardStateFile(k.StateDir), k.Log)
	guard.Publish = func(m map[string]stp.Blocked) {
		v := map[string]any{}
		for n, b := range m {
			v[n] = b
		}
		k.Endpoint.Replace(stp.TopicBPDUBlocked, v)
	}
	guard.Alarm = func(text string) { go k.Notify(fmt.Sprintf("member %d: %s", k.Member, text)) }
	guard.Publish(guard.Blocked())
	k.Endpoint.Handle(stp.MethodClearBPDU, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return nil, err
		}
		return guard.Clear(name), nil
	})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-k.Ctx.Done():
				return
			case <-t.C:
				guard.Tick()
			}
		}
	}()
	k.OnConfig(func(raw json.RawMessage) {
		var c stp.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		guard.SetConfig(c.BPDUBlock, time.Duration(c.BPDUTimeout)*time.Second)
		ctl.SetConfig(&c)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return ctl.Status()
	})
	k.Endpoint.Handle(MethodClearSTP, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var c stp.ClearRequest
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return nil, ctl.Clear(c)
	})
	go ctl.Run(k.Ctx)
	return nil
}
