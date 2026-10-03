// Command cer-lldpd runs LLDP for the switch (reference 5.5, 1.9): it
// announces the stack as one system on the ports switchd configures and
// keeps the neighbours for the show lldp commands.
package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/lldp"
	"github.com/thxrben/cerium-switchd/pkg/netdev"
)

func main() { daemonkit.Main("cer-lldpd", setup) }

func setup(k *daemonkit.Kit) error {
	var mu sync.Mutex
	inBundle := map[string]bool{}
	agent := &lldp.Agent{Log: k.Log, Carrier: netdev.Carrier, Aggregated: func(linux string) bool {
		mu.Lock()
		defer mu.Unlock()
		if on, known := inBundle[linux]; known {
			return on
		}
		return netdev.Carrier(linux) // a static bundle: a port with a link
	}}
	k.OnConfig(func(raw json.RawMessage) {
		var c lldp.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		agent.Sync(c.System, c.Ports)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return agent.Snapshot(), nil
	})
	go agent.Run(k.Ctx)
	go func() {
		<-k.Ctx.Done()
		agent.Sync(lldp.System{}, nil) // ports closed; neighbours age out
	}()
	k.Subscribe(svc.TopicLACPPorts, "", func(ev ipc.Event) {
		if ev.Sync || ev.Deleted {
			return
		}
		var m map[string]bool
		if json.Unmarshal(ev.Value, &m) == nil {
			mu.Lock()
			inBundle = m
			mu.Unlock()
		}
	})
	return nil
}
