// Command cer-bfdd runs BFD for the routing protocols (reference 5.12,
// 1.9): each protocol daemon sets its sessions (method bfd.set) and
// follows their states (topic bfd-sessions). It runs with real-time
// scheduling, so busy CPUs (software forwarding) do not delay its packets.
package main

import (
	"context"
	"encoding/json"

	"github.com/thxrben/cerium-switchd/internal/bfdd"
	"github.com/thxrben/cerium-switchd/internal/daemonkit"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/bfd"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

func main() { daemonkit.Main("cer-bfdd", setup) }

func setup(k *daemonkit.Kit) error {
	udp := &bfd.UDP{}
	sv := bfd.NewServer(udp, k.Log)
	udp.Input = sv.Input
	d := bfdd.New(sv, k.Endpoint, k.Log)
	k.Endpoint.Handle(bfdd.MethodSet, func(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var s bfdd.Set
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return nil, d.Set(s)
	})
	k.Endpoint.Handle(svc.MethodStatus, func(context.Context, *ipc.Conn, json.RawMessage) (any, error) {
		return sv.Sessions(), nil
	})
	go sv.Run()
	go func() {
		<-k.Ctx.Done()
		sv.Stop()
	}()
	return nil
}
