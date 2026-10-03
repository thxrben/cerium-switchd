package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/internal/version"
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

	mu sync.Mutex
	// stack: stacking-protocol method -> daemon serving it.
	stack map[string]string
}

func newService(member int, ctl *stackCtl, notify func(string), role func() svc.Role, log *slog.Logger) *service {
	s := &service{ep: ipc.NewEndpoint(svc.Switchd, version.Version, log), member: member, log: log, ctl: ctl,
		notify: notify, role: role, stack: map[string]string{}}
	s.ep.Handle(svc.MethodNote, func(_ context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
		var n svc.Notice
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		s.notify(n.Text)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

// connected registers the stacking-protocol methods a daemon serves.
func (s *service) connected(c *ipc.Conn) {
	var meta svc.Meta
	json.Unmarshal(c.Peer().Meta, &meta)
	s.log.Debug("daemon connected", "daemon", c.Peer().Name, "version", c.Peer().Version, "stack_methods", meta.Stack)
	if c.Peer().Version != version.Version {
		s.log.Warn("daemon runs another version than switchd", "daemon", c.Peer().Name, "version", c.Peer().Version, "switchd", version.Version)
	}
	if len(meta.Stack) == 0 || s.ctl == nil {
		return
	}
	daemon := c.Peer().Name
	s.mu.Lock()
	defer s.mu.Unlock()
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
	}
	return r
}
