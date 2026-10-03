// Command cer-dhcpcd runs the DHCP clients of the interfaces with family
// inet dhcp (reference 5.3.2, 1.9). It only obtains and keeps the leases
// and reports them; switchd adds the addresses and the default route. A
// restart keeps the leases (saved in /run): the addresses stay.
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/dhcp"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

func main() { daemonkit.Main("cer-dhcpcd", setup) }

func setup(k *daemonkit.Kit) error {
	dir := k.SocketDir
	if dir == "" {
		dir = svc.SocketDir
	}
	var mu sync.Mutex
	host := ""
	changed := make(chan struct{}, 1)
	m := &dhcp.Manager{Log: k.Log, StateFile: filepath.Join(dir, "cer-dhcpcd.leases"),
		HostName: func() string { mu.Lock(); defer mu.Unlock(); return host },
		OnChange: func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}}
	k.OnConfig(func(raw json.RawMessage) {
		var c dhcp.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		mu.Lock()
		host = c.HostName
		mu.Unlock()
		m.Sync(c.Ifaces)
		publish(k, m)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return m.Bindings(), nil
	})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-k.Ctx.Done():
				// A restart keeps the leases; removing the statement (Sync)
				// releases them.
				m.Shutdown()
				return
			case <-changed:
			case <-t.C:
			}
			publish(k, m)
		}
	}()
	return nil
}

// publish reports the current leases (unchanged ones are not sent again).
func publish(k *daemonkit.Kit, m *dhcp.Manager) {
	out := map[string]any{}
	for dev, l := range m.Leases() {
		le := svc.Lease{Addr: l.Addr.String()}
		if l.Router.IsValid() {
			le.Router = l.Router.String()
		}
		out[dev] = le
	}
	k.Endpoint.Replace(svc.TopicLeases, out)
}
