package daemon

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"

	"github.com/thxrben/cerium-switchd/apps/switchd/internal/cli"
	"github.com/thxrben/cerium-switchd/apps/switchd/internal/webapi"
)

// apiState is the REST API's state documents, readiness and metrics
// (reference 5.1 web-management, PLAN 18.2): the structures behind the
// show commands, for the whole stack.
type apiState struct {
	ops   func() cli.Operational // the stack-wide ops (stackOps in a stack)
	ready func() string
}

type stateFunc func(o cli.Operational, q url.Values) (any, error)

var stateDocs = map[string]stateFunc{
	"interfaces":               func(o cli.Operational, _ url.Values) (any, error) { return o.Interfaces() },
	"ethernet-switching-table": func(o cli.Operational, _ url.Values) (any, error) { return o.MACTable() },
	"chassis-hardware":         func(o cli.Operational, _ url.Values) (any, error) { return o.Hardware() },
	"arp":                      func(o cli.Operational, _ url.Values) (any, error) { return o.Neighbors(false) },
	"ipv6-neighbors":           func(o cli.Operational, _ url.Values) (any, error) { return o.Neighbors(true) },
	"uptime":                   func(o cli.Operational, _ url.Values) (any, error) { return o.Uptime() },
	"ntp":                      func(o cli.Operational, _ url.Values) (any, error) { return o.NTP() },
	"offload":                  func(o cli.Operational, _ url.Values) (any, error) { return o.Offload() },
	"routes":                   func(o cli.Operational, q url.Values) (any, error) { return o.Routes(q.Get("instance")) },
	"virtual-chassis":          func(o cli.Operational, _ url.Values) (any, error) { return o.VirtualChassis() },
	"vc-mtu":                   func(o cli.Operational, _ url.Values) (any, error) { return o.StackMTU() },
	"lacp":                     func(o cli.Operational, _ url.Values) (any, error) { return o.LACP() },
	"lldp":                     func(o cli.Operational, _ url.Values) (any, error) { return o.LLDP() },
	"mclag":                    func(o cli.Operational, _ url.Values) (any, error) { return o.MCLAG() },
	"limits":                   func(o cli.Operational, _ url.Values) (any, error) { return o.Limits() },
	"alarms":                   optional(func(x cli.Alarms) (any, error) { return x.Alarms() }),
	"macsec":                   optional(func(x cli.MACsec) (any, error) { return x.MACsec() }),
	"bfd":                      optional(func(x cli.BFD) (any, error) { return x.BFDSessions() }),
	"environment":              optional(func(x cli.Environment) (any, error) { return x.Environment() }),
	"memory":                   optional(func(x cli.Memory) (any, error) { return x.Memory() }),
	"processes":                optional(func(x cli.Processes) (any, error) { return x.Processes() }),
}

// optional reads a document of an optional interface of the ops.
func optional[T any](f func(T) (any, error)) stateFunc {
	return func(o cli.Operational, _ url.Values) (any, error) {
		x, ok := o.(T)
		if !ok {
			return nil, fmt.Errorf("not available on this switch")
		}
		return f(x)
	}
}

func (a *apiState) StateNames() []string {
	out := make([]string, 0, len(stateDocs))
	for k := range stateDocs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (a *apiState) State(name string, q url.Values) (any, error) {
	f := stateDocs[name]
	if f == nil {
		return nil, webapi.ErrUnknown
	}
	return f(a.ops(), q)
}

func (a *apiState) Ready() string { return a.ready() }

// Metrics writes the Prometheus text format (what fails is left out).
func (a *apiState) Metrics(w io.Writer) error {
	o := a.ops()
	if al, ok := o.(cli.Alarms); ok {
		as, _ := al.Alarms()
		n := map[string]int{"Major": 0, "Minor": 0}
		for _, x := range as {
			n[x.Class]++
		}
		fmt.Fprintln(w, "# HELP ceros_alarms Active alarms by class.\n# TYPE ceros_alarms gauge")
		for _, c := range []string{"Major", "Minor"} {
			fmt.Fprintf(w, "ceros_alarms{class=%q} %d\n", c, n[c])
		}
	}
	if p, ok := o.(cli.Processes); ok {
		if ps, err := p.Processes(); err == nil {
			fmt.Fprintln(w, "# HELP ceros_daemon_up Whether a program of the switch runs.\n# TYPE ceros_daemon_up gauge")
			for _, x := range ps {
				up := 0
				if x.State == "running" {
					up = 1
				}
				fmt.Fprintf(w, "ceros_daemon_up{program=%q} %d\n", x.Program, up)
			}
		}
	}
	if ifs, err := o.Interfaces(); err == nil {
		sort.Slice(ifs, func(i, j int) bool { return ifs[i].Name < ifs[j].Name })
		fmt.Fprintln(w, "# HELP ceros_interface_up Whether an interface's link is up.\n# TYPE ceros_interface_up gauge")
		for _, x := range ifs {
			up := 0
			if x.OperUp {
				up = 1
			}
			fmt.Fprintf(w, "ceros_interface_up{interface=%q} %d\n", x.Name, up)
		}
		counters := []struct {
			name, help string
			get        func(cli.IfCounters) uint64
		}{
			{"ceros_interface_receive_bytes_total", "Bytes received.", func(c cli.IfCounters) uint64 { return c.RxBytes }},
			{"ceros_interface_transmit_bytes_total", "Bytes sent.", func(c cli.IfCounters) uint64 { return c.TxBytes }},
			{"ceros_interface_receive_packets_total", "Packets received.", func(c cli.IfCounters) uint64 { return c.RxPackets }},
			{"ceros_interface_transmit_packets_total", "Packets sent.", func(c cli.IfCounters) uint64 { return c.TxPackets }},
			{"ceros_interface_receive_errors_total", "Receive errors.", func(c cli.IfCounters) uint64 { return c.RxErrors }},
			{"ceros_interface_receive_drops_total", "Packets dropped on receive.", func(c cli.IfCounters) uint64 { return c.RxDropped }},
		}
		for _, c := range counters {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
			for _, x := range ifs {
				fmt.Fprintf(w, "%s{interface=%q} %d\n", c.name, x.Name, c.get(x.Counters))
			}
		}
	}
	if m, ok := o.(cli.Memory); ok {
		if st, err := m.Memory(); err == nil {
			fmt.Fprintln(w, "# HELP ceros_memory_entries Entries of a memory slot purpose.\n# TYPE ceros_memory_entries gauge")
			for _, p := range st.Purposes {
				if p.Used >= 0 {
					fmt.Fprintf(w, "ceros_memory_entries{purpose=%q} %d\n", p.Name, p.Used)
				}
			}
			fmt.Fprintln(w, "# HELP ceros_memory_capacity Guaranteed entries of a purpose (0: dynamic).\n# TYPE ceros_memory_capacity gauge")
			for _, p := range st.Purposes {
				fmt.Fprintf(w, "ceros_memory_capacity{purpose=%q} %d\n", p.Name, p.Capacity)
			}
		}
	}
	return nil
}
