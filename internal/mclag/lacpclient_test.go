package mclag

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// MC-LAG's holds reach cer-lacpd as state (also after either side
// restarts), and cer-lacpd's legs reach MC-LAG.
func TestLACPClient(t *testing.T) {
	dir, err := os.MkdirTemp("", "mclag")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// cer-lacpd: serves legs, follows cer-mclagd's control topic.
	lacpd := ipc.NewEndpoint("cer-lacpd", "test", log)
	lacpd.Replace(svc.TopicLACPLegs, map[string]any{"ae1": false, "ae2": true})
	l, err := ipc.Listen(svc.Socket(dir, "cer-lacpd"))
	if err != nil {
		t.Fatal(err)
	}
	go lacpd.Serve(ctx, l)

	mclagd := ipc.NewEndpoint("cer-mclagd", "test", log)
	ml, err := ipc.Listen(svc.Socket(dir, "cer-mclagd"))
	if err != nil {
		t.Fatal(err)
	}
	go mclagd.Serve(ctx, ml)
	c := NewLACPClient(ctx, mclagd, dir)
	c.SetHold("ae1", true)
	c.SetPeerReady("ae2", 2)

	control := make(chan ipc.Event, 10)
	lacpd.Dial(ctx, svc.Socket(dir, "cer-mclagd")).Subscribe(svc.TopicLACPControl, "", func(ev ipc.Event) { control <- ev })
	next := func() ipc.Event {
		select {
		case ev := <-control:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no control event")
		}
		return ipc.Event{}
	}
	got := map[string]string{}
	for ev := next(); !ev.Sync; ev = next() {
		got[ev.Key] = string(ev.Value)
	}
	if got["ae1"] != `{"hold":true}` || got["ae2"] != `{"peer_ready":2}` {
		t.Fatalf("control %v", got)
	}
	c.SetHold("ae1", false) // nothing left for ae1: the key goes away
	if ev := next(); ev.Key != "ae1" || !ev.Deleted {
		t.Fatalf("release %+v", ev)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if l := c.Legs(); len(l) == 2 && l["ae2"] && !l["ae1"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legs %v", c.Legs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
