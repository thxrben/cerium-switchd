package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/alarms"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/internal/version"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
)

// service is switchd's side of the service protocol with the cer- daemons
// (reference 1.9, package svc): it publishes their configuration and this
// member's role, relays their calls to other members over the stacking
// protocol and their notices to the CLI sessions.
type service struct {
	ep     *ipc.Endpoint
	member int
	log    *slog.Logger
	// ctl is the stack control (nil: standalone).
	ctl *stackCtl
	// notify reaches every CLI session of the stack.
	notify func(string)
	// role computes the current role.
	role func() svc.Role
	// alarms are this member's (the daemons raise theirs through
	// MethodAlarm).
	alarms *alarms.Set

	mu sync.Mutex
	// stack: stacking-protocol method -> daemon serving it.
	stack map[string]string
	// mirrors: topics of daemons switchd follows, by daemon.
	mirrors map[string][]*mirror
}

// mirror keeps a copy of a daemon's topic over its restarts: each new
// connection delivers the full state, which replaces the copy at its sync.
type mirror struct {
	topic string
	// onChange receives the whole topic after every change.
	onChange func(map[string]json.RawMessage)

	mu     sync.Mutex
	state  map[string]json.RawMessage
	synced bool
	ready  chan struct{} // closed at the first sync
}

// Ready is closed once the daemon delivered its topic the first time.
func (m *mirror) Ready() <-chan struct{} { return m.ready }

// State returns the current copy.
func (m *mirror) State() map[string]json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.state)
}

func (m *mirror) subscribe(c *ipc.Conn) {
	fresh := map[string]json.RawMessage{}
	syncing := true
	c.Subscribe(m.topic, "", func(ev ipc.Event) {
		m.mu.Lock()
		if ev.Sync {
			if syncing {
				m.state, syncing = fresh, false
			}
			if !m.synced {
				m.synced = true
				close(m.ready)
			}
		} else if syncing {
			if ev.Deleted {
				delete(fresh, ev.Key)
			} else {
				fresh[ev.Key] = ev.Value
			}
			m.mu.Unlock()
			return
		} else if ev.Deleted {
			delete(m.state, ev.Key)
		} else {
			m.state[ev.Key] = ev.Value
		}
		st := maps.Clone(m.state)
		m.mu.Unlock()
		if m.onChange != nil {
			m.onChange(st)
		}
	})
}

// follow mirrors a topic of a daemon (register before it connects).
func (s *service) follow(daemon, topic string, onChange func(map[string]json.RawMessage)) *mirror {
	m := &mirror{topic: topic, onChange: onChange, state: map[string]json.RawMessage{}, ready: make(chan struct{})}
	s.mu.Lock()
	s.mirrors[daemon] = append(s.mirrors[daemon], m)
	s.mu.Unlock()
	if c := s.ep.ConnTo(daemon); c != nil {
		m.subscribe(c)
	}
	return m
}

func newService(member int, ctl *stackCtl, notify func(string), role func() svc.Role, log *slog.Logger) *service {
	s := &service{ep: ipc.NewEndpoint(svc.Switchd, version.Version, log), member: member, log: log, ctl: ctl,
		notify: notify, role: role, stack: map[string]string{}, mirrors: map[string][]*mirror{}, alarms: &alarms.Set{}}
	s.ep.Handle(svc.MethodNote, func(_ context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
		var n svc.Notice
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		s.notify(memberNotice(s.member, n.Text))
		return nil, nil
	})
	s.ep.Handle(svc.MethodAlarm, func(_ context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
		var a svc.Alarm
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		id := c.Peer().Name + "/" + a.ID
		if a.Clear {
			s.alarms.Clear(id)
		} else {
			s.alarms.Raise(id, a.Class, a.Text)
		}
		return nil, nil
	})
	s.ep.Handle(svc.MethodStack, s.stackCall)
	s.ep.OnConnect = s.connected
	return s
}

// start serves the socket until ctx ends and keeps the role current.
func (s *service) start(ctx context.Context, dir string) error {
	if dir == "" {
		dir = svc.SocketDir
	}
	if err := hwio.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	l, err := ipc.Listen(svc.Socket(dir, svc.Switchd))
	if err != nil {
		return err
	}
	go s.ep.Serve(ctx, l)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			s.ep.Publish(svc.TopicRole, "", s.role())
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// setConfig publishes a daemon's configuration (nil: none).
func (s *service) setConfig(daemon string, v any) {
	if err := s.ep.Publish(svc.TopicConfig, daemon, v); err != nil {
		s.log.Error("daemon configuration", "daemon", daemon, "err", err)
	}
}

// call calls a method of a running daemon.
func (s *service) call(ctx context.Context, daemon, method string, req, resp any) error {
	c := s.ep.ConnTo(daemon)
	if c == nil {
		return fmt.Errorf("%s is not running on member %d", daemon, s.member)
	}
	return c.Call(ctx, method, req, resp)
}

// alarmResync is how long a reconnected daemon has to raise its alarms
// again before the old ones end.
const alarmResync = 10 * time.Second

// connected registers the stacking-protocol methods a daemon serves.
func (s *service) connected(c *ipc.Conn) {
	var meta svc.Meta
	json.Unmarshal(c.Peer().Meta, &meta)
	s.log.Debug("daemon connected", "daemon", c.Peer().Name, "version", c.Peer().Version, "stack_methods", meta.Stack)
	if c.Peer().Version != version.Version {
		s.log.Warn("daemon runs another version than switchd", "daemon", c.Peer().Name, "version", c.Peer().Version, "switchd", version.Version)
	}
	daemon := c.Peer().Name
	// A (re)started daemon raises again what still holds; the rest ends
	// (not at once: a held alarm would be announced cleared and raised).
	since := time.Now()
	time.AfterFunc(alarmResync, func() { s.alarms.ClearStale(daemon+"/", since) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.mirrors[daemon] {
		m.subscribe(c)
	}
	if len(meta.Stack) == 0 || s.ctl == nil {
		return
	}
	for _, m := range meta.Stack {
		if s.stack[m] == daemon {
			continue // registered at an earlier connect
		}
		s.stack[m] = daemon
		method := m
		s.ctl.node.Handle(method, func(from int, req json.RawMessage) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out json.RawMessage
			err := s.call(ctx, daemon, svc.StackPrefix+method, svc.StackIn{From: from, Data: req}, &out)
			return out, err
		})
	}
}

// stackCall relays a daemon's call to another member.
func (s *service) stackCall(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var sc svc.StackCall
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, err
	}
	if s.ctl == nil {
		return nil, fmt.Errorf("member %d is not in a virtual chassis", s.member)
	}
	timeout := time.Duration(sc.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > time.Minute {
		timeout = 5 * time.Second
	}
	data := sc.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return s.ctl.node.Call(sc.Member, sc.Method, data, timeout)
}

// roleOf computes this member's role.
func roleOf(member int, ctl *stackCtl, cfg func() *model.Config, hostName func() string, stackID string) svc.Role {
	r := svc.Role{Member: member, Master: true, MasterID: member, StackID: stackID}
	if hostName != nil {
		r.HostName = hostName()
	}
	if c := cfg(); c != nil {
		r.Members = c.SwitchMembers()
		r.ChassisName = c.ChassisName(member)
	}
	if ctl != nil {
		r.Master = ctl.node.IsMaster()
		r.MasterID = ctl.node.Master()
		r.Reachable = ctl.node.Mesh.Reachable()
		slices.Sort(r.Reachable)
		r.Draining = ctl.node.Mesh.Draining()
		slices.Sort(r.Draining)
	}
	return r
}
