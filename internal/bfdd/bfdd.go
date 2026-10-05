// Package bfdd is BFD for the routing protocols (reference 5.12, 1.9):
// each protocol daemon (a client) sets the sessions it wants, as a whole
// list, and follows their states on a topic. Sessions shared by several
// clients run once with the fastest timers. cer-bfdd runs it.
package bfdd

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/bfd"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// Daemon keeps the clients' sessions on a BFD server.
type Daemon struct {
	sv  *bfd.Server
	ep  *ipc.Endpoint
	log *slog.Logger

	mu       sync.Mutex
	byClient map[string]map[bfd.Key]SessionSpec
}

// New returns a daemon on sv publishing on ep.
func New(sv *bfd.Server, ep *ipc.Endpoint, log *slog.Logger) *Daemon {
	return &Daemon{sv: sv, ep: ep, log: log, byClient: map[string]map[bfd.Key]SessionSpec{}}
}

func config(s SessionSpec) (bfd.Config, error) {
	iv := time.Duration(max(s.IntervalMs, 1)) * time.Millisecond
	c := bfd.Config{MinTx: iv, MinRx: iv, Multiplier: uint8(min(max(s.Multiplier, 1), 255))}
	switch s.AuthType {
	case "":
	case "keyed-md5":
		c.Auth = &bfd.Auth{Type: bfd.AuthKeyedMD5, KeyID: uint8(s.AuthKeyID), Key: []byte(s.AuthKey)}
	case "keyed-sha-1":
		c.Auth = &bfd.Auth{Type: bfd.AuthKeyedSHA1, KeyID: uint8(s.AuthKeyID), Key: []byte(s.AuthKey)}
	default:
		return c, fmt.Errorf("bfd: authentication %q not supported", s.AuthType)
	}
	return c, nil
}

// Set replaces a client's sessions: new ones start, changed ones take the
// new timers (a poll sequence, no flap), gone ones end for this client.
func (d *Daemon) Set(s Set) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.byClient[s.Client]
	want := map[bfd.Key]SessionSpec{}
	for _, sp := range s.Sessions {
		want[sp.Key] = sp
	}
	for k := range old {
		if _, ok := want[k]; !ok {
			d.sv.Remove(k, s.Client)
			if !d.usedLocked(k, s.Client) {
				d.ep.Publish(TopicSessions, k.String(), nil)
			}
		}
	}
	var errs []error
	for k, sp := range want {
		if prev, ok := old[k]; ok && prev == sp {
			continue
		}
		cfg, err := config(sp)
		if err != nil {
			errs = append(errs, err)
			delete(want, k)
			continue
		}
		key := k
		if err := d.sv.Add(k, sp.Interface, bfd.Client{Name: s.Client, OnChange: func(bool) { d.publish(key) }}, cfg); err != nil {
			errs = append(errs, err)
			delete(want, k)
			continue
		}
		d.publish(k)
	}
	d.byClient[s.Client] = want
	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}
	return nil
}

// usedLocked: another client still uses the session.
func (d *Daemon) usedLocked(k bfd.Key, except string) bool {
	for c, ss := range d.byClient {
		if _, ok := ss[k]; ok && c != except {
			return true
		}
	}
	return false
}

// publish reports a session's state.
func (d *Daemon) publish(k bfd.Key) {
	for _, st := range d.sv.Sessions() {
		if st.Key == k {
			d.ep.Publish(TopicSessions, k.String(), State{Up: st.State == bfd.Up, State: st.State.String(), Diag: st.Diag.String()})
			return
		}
	}
}
