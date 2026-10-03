package mclag

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// LACPClient is LACP as cer-lacpd serves it (reference 1.9): the legs and
// ready ports come from its topics; holds and the peer's ready ports are
// published as the control topic of this program's endpoint, which
// cer-lacpd follows (state, not calls: a restart of either side loses
// nothing).
type LACPClient struct {
	ep *ipc.Endpoint

	mu      sync.Mutex
	legs    map[string]bool
	ready   map[string]int
	control map[string]svc.LACPControl
}

// NewLACPClient connects to cer-lacpd's socket in dir.
func NewLACPClient(ctx context.Context, ep *ipc.Endpoint, dir string) *LACPClient {
	l := &LACPClient{ep: ep, legs: map[string]bool{}, ready: map[string]int{}, control: map[string]svc.LACPControl{}}
	// Until decided otherwise: nothing held (cer-lacpd waits for this).
	ep.Replace(svc.TopicLACPControl, map[string]any{})
	c := ep.Dial(ctx, svc.Socket(dir, "cer-lacpd"))
	c.Subscribe(svc.TopicLACPLegs, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var v bool
		json.Unmarshal(ev.Value, &v)
		l.mu.Lock()
		if ev.Deleted {
			delete(l.legs, ev.Key)
		} else {
			l.legs[ev.Key] = v
		}
		l.mu.Unlock()
	})
	c.Subscribe(svc.TopicLACPReady, "", func(ev ipc.Event) {
		if ev.Sync {
			return
		}
		var v int
		json.Unmarshal(ev.Value, &v)
		l.mu.Lock()
		if ev.Deleted {
			delete(l.ready, ev.Key)
		} else {
			l.ready[ev.Key] = v
		}
		l.mu.Unlock()
	})
	return l
}

func (l *LACPClient) Legs() map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]bool, len(l.legs))
	for k, v := range l.legs {
		out[k] = v
	}
	return out
}

func (l *LACPClient) Ready() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.ready))
	for k, v := range l.ready {
		out[k] = v
	}
	return out
}

func (l *LACPClient) set(bundle string, f func(*svc.LACPControl)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.control[bundle]
	f(&c)
	if c == (svc.LACPControl{}) {
		delete(l.control, bundle)
		l.ep.Publish(svc.TopicLACPControl, bundle, nil)
		return
	}
	l.control[bundle] = c
	l.ep.Publish(svc.TopicLACPControl, bundle, c)
}

func (l *LACPClient) SetHold(bundle string, hold bool) {
	l.set(bundle, func(c *svc.LACPControl) { c.Hold = hold })
}

func (l *LACPClient) SetPeerReady(bundle string, n int) {
	l.set(bundle, func(c *svc.LACPControl) { c.PeerReady = n })
}
