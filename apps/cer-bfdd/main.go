// Command cer-bfdd runs BFD for the routing protocols (reference 5.12,
// 1.9): each protocol daemon sets its sessions (method bfd.set) and
// follows their states (topic bfd-sessions). It runs with real-time
// scheduling, so busy CPUs (software forwarding) do not delay its packets.
package main

import (
	"context"
	"encoding/json"

	"github.com/thxrben/cerium-switchd/apps/cer-bfdd/internal/bfdd"
	"github.com/thxrben/cerium-switchd/lib/bfd"
	"github.com/thxrben/cerium-switchd/lib/platform/api/bfdapi"
	"github.com/thxrben/cerium-switchd/lib/platform/daemonkit"
	"github.com/thxrben/cerium-switchd/lib/platform/ipc"
	"github.com/thxrben/cerium-switchd/lib/platform/svc"
)

func main() { daemonkit.Main("cer-bfdd", setup) }

func setup(k *daemonkit.Kit) error {
	udp := &bfd.UDP{}
	sv := bfd.NewServer(udp, k.Log)
	udp.Input = sv.Input
	d := bfdd.New(sv, k.Endpoint, k.Log)
	k.Endpoint.Handle(bfdapi.MethodSet, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var s bfdapi.Set
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return nil, d.Set(s)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return sv.Sessions(), nil
	})
	go sv.Run()
	// Every session ends with AdminDown: the neighbours do not count it as
	// a failure.
	k.OnShutdown(func(context.Context) { sv.Stop() })
	return nil
}
