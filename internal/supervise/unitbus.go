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

// unitBus reads unit properties from systemd over D-Bus, one property at
// a time. systemctl show asks systemd for every property of a unit
// (GetAll) and filters afterwards: for a dozen units every second that
// costs PID 1 several percent of a CPU (drop-in directories searched for
// NeedDaemonReload, cgroup files, rlimits and scheduling of each unit).
// Get of the few properties the supervisor needs costs a fraction.
type unitBus struct {
	path string

	mu   sync.Mutex
	conn *dbus.Conn
}

const (
	ifaceUnit    = "org.freedesktop.systemd1.Unit"
	ifaceService = "org.freedesktop.systemd1.Service"
)

// unitProps are the properties read per unit, with their interface.
var unitProps = []struct{ iface, name string }{
	{ifaceUnit, "LoadState"},
	{ifaceUnit, "ActiveState"},
	{ifaceUnit, "SubState"},
	{ifaceUnit, "ActiveEnterTimestamp"},
	{ifaceService, "Result"},
	{ifaceService, "MainPID"},
	{ifaceService, "NRestarts"},
	{ifaceService, "ExecMainCode"},
	{ifaceService, "ExecMainStatus"},
	{ifaceService, "MemoryCurrent"},
	{ifaceService, "CPUUsageNSec"},
}

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

// Show reads the units' states. All requests are sent at once and
// answered in one go.
func (b *unitBus) Show(units []string) (map[string]UnitState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	conn, err := b.connect()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	type pending struct {
		unit, prop string
		call       *dbus.Call
	}
	ch := make(chan *dbus.Call, len(units)*len(unitProps))
	var calls []pending
	for _, u := range units {
		obj := conn.Object("org.freedesktop.systemd1", UnitPath(u))
		for _, p := range unitProps {
			c := obj.GoWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, ch, p.iface, p.name)
			calls = append(calls, pending{u, p.name, c})
		}
	}
	vals := map[string]map[string]any{}
	for range calls {
		select {
		case <-ch:
		case <-ctx.Done():
			b.drop()
			return nil, fmt.Errorf("systemd did not answer within %s", busTimeout)
		}
	}
	for _, c := range calls {
		if c.call.Err != nil {
			var de dbus.Error
			if errors.As(c.call.Err, &de) && (strings.HasSuffix(de.Name, ".UnknownProperty") || strings.HasSuffix(de.Name, ".UnknownInterface")) {
				continue // e.g. no Service interface: the unit is not loaded
			}
			if !conn.Connected() {
				b.drop()
			}
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
	res := map[string]UnitState{}
	for _, u := range units {
		res[u] = unitState(vals[u])
	}
	return res, nil
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
