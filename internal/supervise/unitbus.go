package supervise

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// SystemdPrivate is systemd's own D-Bus socket (no bus daemon needed).
const SystemdPrivate = "/run/systemd/private"

// unitBus reads unit states from systemd over D-Bus. systemctl show asks
// systemd for every property of a unit (GetAll) and filters afterwards:
// for a dozen units every second that costs PID 1 several percent of a CPU
// (drop-in directories searched for NeedDaemonReload, cgroup files, rlimits
// and scheduling of each unit). Here one ListUnitsByNames gives every
// unit's load and active state, and two Gets per unit (MainPID, NRestarts)
// catch restarts between two polls; the details are read only when one of
// these changed, memory and CPU every usageEvery.
type unitBus struct {
	path string
	now  func() time.Time // nil: time.Now

	mu    sync.Mutex
	conn  *dbus.Conn
	cache map[string]*cachedUnit
}

type cachedUnit struct {
	st    UnitState
	usage time.Time // memory and CPU read
}

const (
	ifaceUnit    = "org.freedesktop.systemd1.Unit"
	ifaceService = "org.freedesktop.systemd1.Service"
)

type prop struct{ iface, name string }

var (
	// fastProps are read every poll; detailProps when the state, the main
	// process or the restart count changed; usageProps every usageEvery.
	fastProps   = []prop{{ifaceService, "MainPID"}, {ifaceService, "NRestarts"}}
	detailProps = []prop{{ifaceUnit, "ActiveEnterTimestamp"}, {ifaceService, "Result"}, {ifaceService, "ExecMainCode"}, {ifaceService, "ExecMainStatus"}}
	usageProps  = []prop{{ifaceService, "MemoryCurrent"}, {ifaceService, "CPUUsageNSec"}}
)

// usageEvery is how often memory and CPU time are read (show system
// processes shows them; nothing acts on them).
const usageEvery = 10 * time.Second

// busTimeout bounds connecting and one round of reads.
const busTimeout = 5 * time.Second

func (b *unitBus) connect() (*dbus.Conn, error) {
	if b.conn != nil && b.conn.Connected() {
		return b.conn, nil
	}
	d := net.Dialer{Timeout: busTimeout}
	nc, err := d.Dial("unix", b.path)
	if err != nil {
		return nil, err
	}
	uc := nc.(*net.UnixConn)
	// Auth reads and writes without a deadline of its own.
	_ = uc.SetDeadline(time.Now().Add(busTimeout))
	conn, err := dbus.DialUnix(uc)
	if err != nil {
		uc.Close()
		return nil, err
	}
	// A private (peer-to-peer) connection: no Hello, no bus names.
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		conn.Close()
		return nil, err
	}
	_ = uc.SetDeadline(time.Time{})
	b.conn = conn
	return conn, nil
}

func (b *unitBus) drop() {
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
}

// Show reads the units' states.
func (b *unitBus) Show(units []string) (map[string]UnitState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	conn, err := b.connect()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	now := time.Now()
	if b.now != nil {
		now = b.now()
	}
	if b.cache == nil {
		b.cache = map[string]*cachedUnit{}
	}
	// Load, active and sub state of every unit in one call.
	var list [][]any
	call := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx,
		"org.freedesktop.systemd1.Manager.ListUnitsByNames", 0, units)
	if call.Err != nil {
		b.fail(conn)
		return nil, fmt.Errorf("ListUnitsByNames: %w", call.Err)
	}
	if err := call.Store(&list); err != nil {
		return nil, fmt.Errorf("ListUnitsByNames: %w", err)
	}
	base := map[string]UnitState{}
	for _, row := range list {
		if len(row) < 5 {
			continue
		}
		name, _ := row[0].(string)
		load, _ := row[2].(string)
		active, _ := row[3].(string)
		sub, _ := row[4].(string)
		base[name] = UnitState{Loaded: load == "loaded", Active: active, Sub: sub}
	}
	var loaded []string
	for _, u := range units {
		if base[u].Loaded {
			loaded = append(loaded, u)
		}
	}
	fast, err := b.gets(ctx, conn, loaded, func(string) []prop { return fastProps })
	if err != nil {
		return nil, err
	}
	res := map[string]UnitState{}
	var more []string
	want := map[string][]prop{}
	for _, u := range units {
		st := base[u]
		v := fast[u]
		cur := unitState(v)
		st.PID, st.NRestarts = cur.PID, cur.NRestarts
		c := b.cache[u]
		changed := c == nil || c.st.Active != st.Active || c.st.Sub != st.Sub || c.st.PID != st.PID || c.st.NRestarts != st.NRestarts
		if st.Loaded && changed {
			want[u] = append(want[u], detailProps...)
		}
		if st.Loaded && (changed || now.Sub(c.usage) >= usageEvery) {
			want[u] = append(want[u], usageProps...)
		}
		if len(want[u]) > 0 {
			more = append(more, u)
		}
		res[u] = st
	}
	detail, err := b.gets(ctx, conn, more, func(u string) []prop { return want[u] })
	if err != nil {
		return nil, err
	}
	for _, u := range units {
		st := res[u]
		c := b.cache[u]
		if c == nil {
			c = &cachedUnit{}
			b.cache[u] = c
		}
		if v, ok := detail[u]; ok {
			d := unitState(v)
			if _, read := v["Result"]; read {
				st.Result, st.ExitCode, st.ExitStatus, st.Since = d.Result, d.ExitCode, d.ExitStatus, d.Since
			} else {
				st.Result, st.ExitCode, st.ExitStatus, st.Since = c.st.Result, c.st.ExitCode, c.st.ExitStatus, c.st.Since
			}
			if _, read := v["MemoryCurrent"]; read {
				st.Memory, st.CPU = d.Memory, d.CPU
				c.usage = now
			} else {
				st.Memory, st.CPU = c.st.Memory, c.st.CPU
			}
		} else {
			st.Result, st.ExitCode, st.ExitStatus, st.Since = c.st.Result, c.st.ExitCode, c.st.ExitStatus, c.st.Since
			st.Memory, st.CPU = c.st.Memory, c.st.CPU
		}
		if !st.Loaded {
			st = UnitState{Active: st.Active, Sub: st.Sub}
		}
		c.st = st
		res[u] = st
	}
	for u := range b.cache {
		if _, ok := res[u]; !ok {
			delete(b.cache, u) // no longer asked for
		}
	}
	return res, nil
}

func (b *unitBus) fail(conn *dbus.Conn) {
	if !conn.Connected() {
		b.drop()
	}
}

// gets reads properties of units, all requests sent at once.
func (b *unitBus) gets(ctx context.Context, conn *dbus.Conn, units []string, props func(string) []prop) (map[string]map[string]any, error) {
	type pending struct {
		unit, prop string
		call       *dbus.Call
	}
	var calls []pending
	n := 0
	for _, u := range units {
		n += len(props(u))
	}
	ch := make(chan *dbus.Call, n)
	for _, u := range units {
		obj := conn.Object("org.freedesktop.systemd1", UnitPath(u))
		for _, p := range props(u) {
			c := obj.GoWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, ch, p.iface, p.name)
			calls = append(calls, pending{u, p.name, c})
		}
	}
	for range calls {
		select {
		case <-ch:
		case <-ctx.Done():
			b.drop()
			return nil, fmt.Errorf("systemd did not answer within %s", busTimeout)
		}
	}
	vals := map[string]map[string]any{}
	for _, c := range calls {
		if c.call.Err != nil {
			var de dbus.Error
			if errors.As(c.call.Err, &de) && (strings.HasSuffix(de.Name, ".UnknownProperty") || strings.HasSuffix(de.Name, ".UnknownInterface")) {
				continue
			}
			b.fail(conn)
			return nil, fmt.Errorf("%s %s: %w", c.unit, c.prop, c.call.Err)
		}
		var v dbus.Variant
		if err := c.call.Store(&v); err != nil {
			return nil, fmt.Errorf("%s %s: %w", c.unit, c.prop, err)
		}
		if vals[c.unit] == nil {
			vals[c.unit] = map[string]any{}
		}
		vals[c.unit][c.prop] = v.Value()
	}
	return vals, nil
}

// unitState converts property values (D-Bus types) to a UnitState.
func unitState(v map[string]any) UnitState {
	str := func(k string) string { s, _ := v[k].(string); return s }
	num := func(k string) int {
		switch x := v[k].(type) {
		case uint32:
			return int(x)
		case int32:
			return int(x)
		}
		return 0
	}
	u64 := func(k string) uint64 {
		x, _ := v[k].(uint64)
		if x == math.MaxUint64 { // "not set"
			return 0
		}
		return x
	}
	u := UnitState{
		Loaded:     str("LoadState") == "loaded",
		Active:     str("ActiveState"),
		Sub:        str("SubState"),
		Result:     str("Result"),
		PID:        num("MainPID"),
		NRestarts:  num("NRestarts"),
		ExitCode:   num("ExecMainCode"),
		ExitStatus: num("ExecMainStatus"),
		Memory:     u64("MemoryCurrent"),
		CPU:        time.Duration(u64("CPUUsageNSec")),
	}
	if us := u64("ActiveEnterTimestamp"); us > 0 {
		u.Since = time.UnixMicro(int64(us)).Truncate(time.Second)
	}
	return u
}

// UnitPath is the D-Bus object path of a unit: every byte other than a
// letter or (not leading) digit becomes _xx.
func UnitPath(unit string) dbus.ObjectPath {
	var b strings.Builder
	b.WriteString("/org/freedesktop/systemd1/unit/")
	for i := 0; i < len(unit); i++ {
		c := unit[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "_%02x", c)
	}
	return dbus.ObjectPath(b.String())
}

// HardwareWatchdogDevice reports whether the machine has a watchdog device
// (/dev/watchdog0 or /dev/watchdog).
func HardwareWatchdogDevice() bool {
	for _, p := range []string{"/dev/watchdog0", "/dev/watchdog"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// DisableManagerWatchdog turns systemd's hardware watchdog off (runtime
// and reboot). Without a watchdog device systemd retries opening one on
// every pass of its main loop (about 100 times a second) while the
// watchdog is configured (docs/os-image.md §2).
func DisableManagerWatchdog(busPath string) error {
	b := &unitBus{path: busPath}
	b.mu.Lock()
	defer b.mu.Unlock()
	conn, err := b.connect()
	if err != nil {
		return err
	}
	defer b.drop()
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	obj := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	for _, prop := range []string{"RuntimeWatchdogUSec", "RebootWatchdogUSec"} {
		call := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
			"org.freedesktop.systemd1.Manager", prop, dbus.MakeVariant(uint64(0)))
		if call.Err != nil {
			return fmt.Errorf("%s: %w", prop, call.Err)
		}
	}
	return nil
}
