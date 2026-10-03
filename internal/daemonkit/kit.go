// Package daemonkit is the common part of the cer- daemons (reference 1.9):
// flags, journal logging, signals, systemd readiness and watchdog, the
// daemon's own socket and its connection to switchd with the config and
// role subscriptions.
//
// A daemon's main is
//
//	func main() { daemonkit.Main("cer-lldpd", setup) }
//
// where setup registers handlers and subscriptions on the Kit and starts
// the daemon's work; Main returns when the daemon is told to stop.
package daemonkit

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/internal/svc"
	"github.com/thxrben/cerium-switchd/internal/version"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/ipc"
	"github.com/thxrben/cerium-switchd/pkg/journal"
	"github.com/thxrben/cerium-switchd/pkg/nlx"
	"github.com/thxrben/cerium-switchd/pkg/sdnotify"
)

// Options are a kit's settings (flags in Main).
type Options struct {
	Name      string
	SocketDir string // "" = svc.SocketDir
	StateDir  string // persistent state (default /var/lib/switchd)
	Member    int
	Log       *slog.Logger
	// StopTimeout is the daemon's budget to finish when told to stop
	// (switchd gives it; systemd kills it a little later).
	StopTimeout time.Duration
}

// Kit is a running daemon's connection to the rest of the switch.
type Kit struct {
	Options
	// Ctx is the daemon's life: it ends after the shutdown work (Shutdown),
	// so the daemon keeps working while it finishes.
	Ctx    context.Context
	cancel context.CancelFunc
	// Endpoint serves this daemon's calls and topics (to switchd and to
	// other daemons).
	Endpoint *ipc.Endpoint
	// Switchd is the connection to switchd.
	Switchd *ipc.Client
	// Live feeds the systemd watchdog: a loop of the daemon that beats
	// (Live.Beat) and then stops making progress gets the daemon
	// restarted.
	Live *sdnotify.Liveness

	mu       sync.Mutex
	shutdown []func(context.Context)
	stack    []string
	role     svc.Role
	roleOK   bool
	onRole   []func(svc.Role)
	// subscriptions to switchd registered before Start.
	pending []pendingSub
}

// New returns a kit; Start connects it.
func New(ctx context.Context, o Options) *Kit {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.StateDir == "" {
		o.StateDir = "/var/lib/switchd"
	}
	e := ipc.NewEndpoint(o.Name, version.Version, o.Log)
	kctx, cancel := context.WithCancel(ctx)
	return &Kit{Options: o, Ctx: kctx, cancel: cancel, Endpoint: e, Live: &sdnotify.Liveness{}}
}

// OnShutdown registers work to do when the daemon is told to stop (close
// sessions, tell peers, flush queues). The hooks run in parallel with a
// context that ends at the stop budget; the daemon exits when they return
// or the budget is spent (a hook that hangs does not keep it).
func (k *Kit) OnShutdown(f func(ctx context.Context)) {
	k.mu.Lock()
	k.shutdown = append(k.shutdown, f)
	k.mu.Unlock()
}

// Shutdown runs the shutdown hooks within the stop budget.
func (k *Kit) Shutdown() {
	budget := k.StopTimeout
	if budget <= 0 {
		budget = 3 * time.Second
	}
	// A little before systemd's kill, so the exit is clean.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	k.mu.Lock()
	hooks := slices.Clone(k.shutdown)
	k.mu.Unlock()
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, f := range hooks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f(ctx)
			}()
		}
		wg.Wait()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
		k.Log.Info(k.Name+" finished", "took", time.Since(start).Round(time.Millisecond))
	case <-ctx.Done():
		k.Log.Warn(k.Name+" did not finish its shutdown work in time; ending anyway", "budget", budget)
	}
}

// HandleStack serves a stacking-protocol method: calls of the same daemon
// on other members (switchd relays them). Register before Start.
func (k *Kit) HandleStack(method string, f func(ctx context.Context, from int, req json.RawMessage) (any, error)) {
	k.mu.Lock()
	k.stack = append(k.stack, method)
	k.mu.Unlock()
	k.Endpoint.Handle(svc.StackPrefix+method, func(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
		var in svc.StackIn
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		return f(ctx, in.From, in.Data)
	})
}

// Start opens the daemon's socket and connects to switchd.
func (k *Kit) Start() error {
	k.mu.Lock()
	meta, _ := json.Marshal(svc.Meta{Stack: k.stack})
	k.mu.Unlock()
	k.Endpoint.Meta = meta
	if err := hwio.MkdirAll(k.socketDir(), 0o755); err != nil {
		return err
	}
	l, err := ipc.Listen(svc.Socket(k.SocketDir, k.Name))
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	go k.Endpoint.Serve(k.Ctx, l)
	sw := k.Endpoint.Dial(k.Ctx, svc.Socket(k.SocketDir, svc.Switchd))
	k.mu.Lock()
	k.Switchd = sw
	k.mu.Unlock()
	k.mu.Lock()
	pending := k.pending
	k.pending = nil
	k.mu.Unlock()
	for _, p := range pending {
		k.Switchd.Subscribe(p.topic, p.key, p.f)
	}
	k.Switchd.Subscribe(svc.TopicRole, "", func(ev ipc.Event) {
		if ev.Sync || ev.Deleted {
			return
		}
		var r svc.Role
		if json.Unmarshal(ev.Value, &r) != nil {
			return
		}
		k.mu.Lock()
		k.role, k.roleOK = r, true
		fs := k.onRole
		k.mu.Unlock()
		for _, f := range fs {
			f(r)
		}
	})
	return nil
}

func (k *Kit) socketDir() string {
	if k.SocketDir == "" {
		return svc.SocketDir
	}
	return k.SocketDir
}

// OnConfig calls f with every new configuration of this daemon, in order
// (a restart of switchd that brings the same configuration calls nothing).
func (k *Kit) OnConfig(f func(raw json.RawMessage)) {
	var last string
	sub := func(ev ipc.Event) {
		if ev.Sync || ev.Deleted || string(ev.Value) == last {
			return
		}
		last = string(ev.Value)
		f(ev.Value)
	}
	k.Subscribe(svc.TopicConfig, k.Name, sub)
}

type pendingSub struct {
	topic, key string
	f          func(ipc.Event)
}

// Subscribe follows a topic of switchd (before or after Start).
func (k *Kit) Subscribe(topic, key string, f func(ipc.Event)) {
	k.mu.Lock()
	sw := k.Switchd
	if sw == nil {
		k.pending = append(k.pending, pendingSub{topic, key, f})
	}
	k.mu.Unlock()
	if sw != nil {
		sw.Subscribe(topic, key, f)
	}
}

// OnRole calls f with every new role (and at once if one is known).
func (k *Kit) OnRole(f func(svc.Role)) {
	k.mu.Lock()
	k.onRole = append(k.onRole, f)
	r, ok := k.role, k.roleOK
	k.mu.Unlock()
	if ok {
		f(r)
	}
}

// Role returns the current role (ok false: not known yet).
func (k *Kit) Role() (svc.Role, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.role, k.roleOK
}

// StackCall calls method of this daemon on another member.
func (k *Kit) StackCall(ctx context.Context, member int, method string, req, resp any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	timeout := 5 * time.Second
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	return k.Switchd.Call(ctx, svc.MethodStack, svc.StackCall{Member: member, Method: method, Data: raw,
		TimeoutMs: int(timeout / time.Millisecond)}, resp)
}

// Notify sends a message to every CLI session of the stack.
func (k *Kit) Notify(text string) {
	ctx, cancel := context.WithTimeout(k.Ctx, 5*time.Second)
	defer cancel()
	if err := k.Switchd.Call(ctx, svc.MethodNote, svc.Notice{Text: text}, nil); err != nil {
		k.Log.Warn("notice not delivered", "text", text, "err", err)
	}
}

// Main runs a daemon: flags, logging, setup, readiness and watchdog, until
// SIGTERM or SIGINT.
func Main(name string, setup func(k *Kit) error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	sockDir := fs.String("socket-dir", svc.SocketDir, "directory of the programs' sockets")
	stateDir := fs.String("state-dir", "/var/lib/switchd", "directory for persistent state")
	member := fs.Int("member", 0, "this switch's member id")
	stopTimeout := fs.Duration("stop-timeout", 3*time.Second, "time to finish when told to stop")
	debug := fs.Bool("debug", false, "debug logging")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version.Version, version.Date)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	fields := map[string]string{}
	if *member > 0 {
		fields["CEROS_MEMBER"] = strconv.Itoa(*member)
	}
	log := slog.New(journal.NewHandler(journal.Options{Identifier: name, Level: level, Fields: fields}))
	slog.SetDefault(log)
	nlx.SetSocketTimeout()
	sig, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	k := New(context.Background(), Options{Name: name, SocketDir: *sockDir, StateDir: *stateDir, Member: *member, Log: log, StopTimeout: *stopTimeout})
	if err := run(k, sig, setup); err != nil {
		log.Error(name+" failed", "err", err)
		os.Exit(1)
	}
}

func run(k *Kit, sig context.Context, setup func(*Kit) error) error {
	if err := setup(k); err != nil {
		return err
	}
	if err := k.Start(); err != nil {
		return err
	}
	sdnotify.Ready()
	k.Log.Info(k.Name+" started", "version", version.Version)
	go k.Live.Run(k.Ctx, func(late []string) {
		if len(late) > 0 {
			k.Log.Error(k.Name+": loops make no progress; the watchdog restarts it", "loops", strings.Join(late, ", "))
		}
	})
	// A device that stops answering: an alarm for the operators (the
	// call failed at its deadline; nothing restarts).
	hwio.WatchResources(func(c hwio.Call, raised bool) {
		if raised {
			k.Log.Error("ALARM: a device does not answer", "resource", c.Resource, "call", c.Op, "since", c.Since)
			k.Notify(fmt.Sprintf("%s: ALARM: %s does not answer (%s, since %s)", k.Name, c.Resource, c.Op, c.Since.Format("15:04:05")))
			return
		}
		k.Log.Warn("alarm cleared: the device answers again", "resource", c.Resource)
		k.Notify(fmt.Sprintf("%s: alarm cleared: %s answers again", k.Name, c.Resource))
	})
	<-sig.Done()
	sdnotify.Notify("STOPPING=1")
	k.Log.Info(k.Name + " stopping")
	k.Shutdown()
	k.cancel()
	return nil
}
