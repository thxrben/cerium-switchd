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
	"strings"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/api/stpapi"
	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/stp"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// MethodClearSTP is clear spanning-tree protocol-migration|statistics.
const MethodClearSTP = stpapi.MethodClear

func main() { daemonkit.Main("cer-rstpd", setup) }

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
	// The RSTP copy is for hitless restarts of this daemon while the system
	// runs: memory (/run), not the disk (written every second; on a USB
	// system stick that was the largest load on the disk).
	ctl := stp.New(k.Member, k.Stack(), k.SocketDir, k.Log)
	ctl.Alarm = func(text string) {
		go func() { // never under the controller's lock
			k.Notify(fmt.Sprintf("member %d: %s", k.Member, text))
			if msg, ok := strings.CutPrefix(text, "ALARM: "); ok {
				k.Alarm("rstp", alarms.Major, msg)
			} else {
				k.ClearAlarm("rstp")
			}
		}()
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
	var alarmed sync.Map // ports with a bpdu-block alarm
	guard.Publish = func(m map[string]stpapi.Blocked) {
		v := map[string]any{}
		for n, b := range m {
			v[n] = b
		}
		k.Endpoint.Replace(stpapi.TopicBPDUBlocked, v)
		go func() {
			for n, b := range m {
				if _, had := alarmed.LoadOrStore(n, true); !had {
					k.Alarm("bpdu-block "+n, alarms.Minor, fmt.Sprintf("%s received a BPDU from %s and is shut down (bpdu-block)", n, b.From))
				}
			}
			alarmed.Range(func(key, _ any) bool {
				if _, still := m[key.(string)]; !still {
					alarmed.Delete(key)
					k.ClearAlarm("bpdu-block " + key.(string))
				}
				return true
			})
		}()
	}
	guard.Alarm = func(text string) { go k.Notify(fmt.Sprintf("member %d: %s", k.Member, text)) }
	guard.Publish(guard.Blocked())
	k.Endpoint.Handle(stpapi.MethodClearBPDU, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
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
		var c stpapi.Config
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
		var c stpapi.ClearRequest
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return nil, ctl.Clear(c)
	})
	go ctl.Run(k.Ctx)
	return nil
}
