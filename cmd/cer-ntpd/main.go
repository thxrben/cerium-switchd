// Command cer-ntpd keeps the switch's clock (reference 5.1, 1.8, 1.9): on
// the master an NTP client queries the configured servers through the
// management instance; the other members take the master's time over the
// stacking protocol.
package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/ntp"
)

func main() { daemonkit.Main("cer-ntpd", setup) }

func setup(k *daemonkit.Kit) error {
	client := &ntp.Client{Clock: ntp.SystemClock{}, Log: k.Log}
	var mu sync.Mutex
	var cfg ntp.Config
	var role svc.Role
	master := func(r svc.Role) bool { return r.Master || r.MasterID == 0 || r.MasterID == r.Member }
	apply := func() {
		mu.Lock()
		c, r := cfg, role
		mu.Unlock()
		client.SetVRF(c.VRF)
		if master(r) {
			client.Configure(c.Servers)
		} else {
			client.Configure(nil)
		}
	}
	k.OnConfig(func(raw json.RawMessage) {
		var c ntp.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		mu.Lock()
		cfg = c
		mu.Unlock()
		apply()
	})
	k.OnRole(func(r svc.Role) {
		mu.Lock()
		changed := master(r) != master(role)
		role = r
		mu.Unlock()
		if changed {
			apply()
		}
	})
	// The master's time for the other members.
	k.HandleStack("time", func(context.Context, int, json.RawMessage) (any, error) {
		return time.Now().UnixNano(), nil
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return client.Status(), nil
	})
	f := &ntp.Follower{Clock: ntp.SystemClock{}, Log: k.Log, Query: func(ctx context.Context) (time.Time, bool, error) {
		mu.Lock()
		r := role
		mu.Unlock()
		if master(r) {
			return time.Time{}, false, nil
		}
		var ns int64
		if err := k.StackCall(ctx, r.MasterID, "time", nil, &ns); err != nil {
			return time.Time{}, true, err
		}
		return time.Unix(0, ns), true, nil
	}}
	go f.Run(k.Ctx)
	go func() {
		<-k.Ctx.Done()
		client.Stop()
	}()
	return nil
}
