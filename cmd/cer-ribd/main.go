// Command cer-ribd is the member's routing table and the only program that
// installs routes (reference 5.8, 1.9): connected, static and DHCP routes
// from switchd, the routing protocols' routes from their daemons; the
// active routes by preference (with ECMP) go into the kernel, changing only
// what differs. A restart is hitless: nothing is removed before the
// sources of a route have reported.
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/names"
	"github.com/thxrben/cerium-switchd/internal/ribd"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/netdev"
	"github.com/thxrben/cerium-switchd/pkg/rib"
)

func main() { daemonkit.Main("cer-ribd", setup) }

func setup(k *daemonkit.Kit) error {
	s := ribd.New(func(want []netdev.Route, protos []int) (bool, []string, error) {
		return netdev.SyncRoutes(want, protos, names.StackTable)
	}, k.Log, time.Now())
	k.OnConfig(func(raw json.RawMessage) {
		var c ribd.Config
		if err := json.Unmarshal(raw, &c); err != nil {
			k.Log.Error("configuration", "err", err)
			return
		}
		s.SetConfig(&c)
	})
	k.Endpoint.Handle(svc.MethodRoutesSet, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var sr ribd.SetRoutes
		if err := json.Unmarshal(raw, &sr); err != nil {
			return nil, err
		}
		s.SetRoutes(sr)
		return nil, nil
	})
	k.Endpoint.Handle(svc.MethodRoutes, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var q rib.Query
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &q); err != nil {
				return nil, err
			}
		}
		return s.Lookup(q), nil
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return s.Lookup(rib.Query{Active: true}), nil
	})
	go s.Run(k.Ctx)
	return nil
}
