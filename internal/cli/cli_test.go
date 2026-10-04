package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/internal/access"
	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/diag"
	"github.com/thxrben/cerium-switchd/pkg/lacp"
	"github.com/thxrben/cerium-switchd/pkg/lldp"
)

type nopApplier struct{}

func (nopApplier) Apply(context.Context, *config.Tree, *config.Tree) []commit.MemberResult {
	return []commit.MemberResult{{Member: "member1"}}
}

// scriptTerm answers prompts from a queue and keeps files in memory.
type scriptTerm struct {
	answers []string
	text    string
	files   map[string]string
	asked   []string
}

func (t *scriptTerm) Ask(prompt string, echo bool) (string, error) {
	t.asked = append(t.asked, prompt)
	if len(t.answers) == 0 {
		return "", io.EOF
	}
	a := t.answers[0]
	t.answers = t.answers[1:]
	return a, nil
}

func (t *scriptTerm) ReadText(string) (string, error) { return t.text, nil }

func (t *scriptTerm) ReadFile(name string) ([]byte, error) {
	s, ok := t.files[name]
	if !ok {
		return nil, errors.New(name + ": no such file")
	}
	return []byte(s), nil
}

func (t *scriptTerm) WriteFile(name string, data []byte) error {
	t.files[name] = string(data)
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newEngine(t testing.TB) *commit.Engine {
	t.Helper()
	st, err := commit.OpenFileStore(t.TempDir(), 50)
	if err != nil {
		t.Fatal(err)
	}
	e, err := commit.New(commit.Options{Store: st, Applier: nopApplier{}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.Start(context.Background())
	t.Cleanup(e.Close)
	return e
}

type tester struct {
	t    *testing.T
	sh   *Shell
	term *scriptTerm
}

func newTester(t *testing.T, e *commit.Engine, user string, class commit.Class) *tester {
	sh := New(Env{Engine: e, User: user, Class: class, Version: "test", Log: quiet,
		HostName: func() string { return "sw1" },
		Ports:    func() []string { return []string{"1/0/0", "1/0/1", "1/0/2"} }})
	return &tester{t: t, sh: sh, term: &scriptTerm{files: map[string]string{}}}
}

// run executes a line and returns its output.
func (ts *tester) run(line string) string {
	ts.t.Helper()
	rep := ts.sh.Execute(context.Background(), line, ts.term)
	if strings.Contains(rep.Output, "internal error") {
		ts.t.Fatalf("%q: %s", line, rep.Output)
	}
	return rep.Output
}

// ok executes a line that must not print an error.
func (ts *tester) ok(line string) string {
	ts.t.Helper()
	out := ts.run(line)
	if strings.Contains(out, "error") || strings.Contains(out, "^\n") {
		ts.t.Fatalf("%q failed:\n%s", line, out)
	}
	return out
}

func contains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestOperationalMode(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	if p := ts.sh.Prompt(); p != "alice@sw1> " {
		t.Errorf("prompt %q", p)
	}
	contains(t, ts.ok("show version"), "cerOS (switchd) test")
	contains(t, ts.ok("sh ver"), "switchd") // abbreviations
	out := ts.run("show bogus")
	contains(t, out, strings.Repeat(" ", len("alice@sw1> show "))+"^\n", "syntax error")
	contains(t, ts.run("co"), "ambiguous: configure, confirm")
	contains(t, ts.run("show"), "missing argument")
	if rep := ts.sh.Execute(context.Background(), "exit", ts.term); !rep.Exit {
		t.Error("exit did not end the session")
	}
	contains(t, ts.run(`show "unterminated`), "unterminated")
	contains(t, ts.run("show version | frobnicate"), "expecting a pipe command")
	contains(t, ts.run("show version | display set"), "only valid for show commands")
}

func TestConfigureSetShowCommit(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	contains(t, ts.ok("configure"), "Entering configuration mode")
	if ts.sh.Prompt() != "alice@sw1# " || ts.sh.Banner() != "[edit]\n" {
		t.Errorf("prompt %q banner %q", ts.sh.Prompt(), ts.sh.Banner())
	}
	ts.ok("set system host-name core")
	ts.ok("set vlans users vlan-id 10")
	ts.ok("set interfaces 1/0/1 unit 0 family ethernet-switching interface-mode access")
	ts.ok("set interfaces 1/0/1 unit 0 family ethernet-switching vlan members users")
	contains(t, ts.ok("show system"), "host-name core;")
	contains(t, ts.ok("show | display set"), "set vlans users vlan-id 10\n", "set system host-name core\n")
	contains(t, ts.ok("show | compare"), "[edit]\n+   system {\n+       host-name core;")
	out := ts.ok("commit comment \"first one\"")
	contains(t, out, "member1: commit complete", "automatically rolled back in 10 minutes")
	if !strings.Contains(ts.sh.Banner(), "[commit pending confirmation: 10m left]") {
		t.Errorf("banner %q", ts.sh.Banner())
	}
	contains(t, ts.ok("commit"), "commit confirmed")
	if e.Pending() != nil {
		t.Error("still pending")
	}
	contains(t, ts.ok("run show system commit"), "0   ", "by alice", "first one")
	contains(t, ts.ok("exit"), "Exiting configuration mode")
	contains(t, ts.ok("show configuration interfaces 1/0/1 | display set"),
		"set interfaces 1/0/1 unit 0 family ethernet-switching vlan members users")
	contains(t, ts.ok("show configuration vlans"), "users {\n    vlan-id 10;\n}")
	contains(t, ts.ok("show configuration system host-name"), "host-name core;")
	contains(t, ts.ok("show configuration | display json"), `"host-name": "core"`)
}

func TestSetErrors(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	out := ts.run("set system hots-name x")
	contains(t, out, strings.Repeat(" ", len("alice@sw1# set system "))+"^\n", "syntax error")
	contains(t, ts.run("set system host-name"), "missing argument")
	contains(t, ts.run("set vlans v vlan-id 5000"), "^")
	contains(t, ts.run("set"), "missing statement")
	contains(t, ts.run("set system"), "missing argument")
	if ts.sh.sess.Changed() {
		t.Error("failed commands changed the candidate")
	}
}

func TestEditLevels(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	ts.ok("edit interfaces 1/0/3")
	if b := ts.sh.Banner(); b != "[edit interfaces 1/0/3]\n" {
		t.Errorf("banner %q", b)
	}
	ts.ok("set mtu 9014")
	ts.ok("edit unit 0 family ethernet-switching")
	ts.ok("set interface-mode trunk")
	contains(t, ts.ok("show"), "interface-mode trunk;")
	contains(t, ts.ok("show | display set relative"), "set interface-mode trunk\n")
	contains(t, ts.ok("show | display set"), "set interfaces 1/0/3 unit 0 family ethernet-switching interface-mode trunk\n")
	ts.ok("up 2")
	if b := ts.sh.Banner(); b != "[edit interfaces 1/0/3 unit 0]\n" {
		t.Errorf("after up 2: %q", b)
	}
	ts.ok("exit") // back to the previous level
	if b := ts.sh.Banner(); b != "[edit interfaces 1/0/3 unit 0 family ethernet-switching]\n" {
		t.Errorf("after exit: %q", b)
	}
	ts.ok("top")
	contains(t, ts.ok("show interfaces 1/0/3"), "mtu 9014;")
	contains(t, ts.run("edit system host-name"), "needs a container")
	contains(t, ts.run("edit system syslog host"), "missing")
	ts.ok("edit interfaces") // list level, like Junos
	if b := ts.sh.Banner(); b != "[edit interfaces]\n" {
		t.Errorf("banner %q", b)
	}
	contains(t, ts.ok("show"), "1/0/3 {")
	ts.ok("set 1/0/4 mtu 1600")
	ts.ok("top")
	contains(t, ts.ok("show interfaces 1/0/4"), "mtu 1600;")
	contains(t, ts.ok("up"), "already at the top")
	ts.ok("edit vlans v")
	ts.ok("exit configuration-mode")
	if ts.sh.InConfig() {
		t.Error("exit configuration-mode did not leave")
	}
}

func TestDeleteActivateCopyRename(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	ts.ok("set vlans a vlan-id 10")
	ts.ok("set system name-server [ 1.1.1.1 9.9.9.9 ]")
	ts.ok("delete system name-server 1.1.1.1")
	contains(t, ts.ok("show system"), "name-server 9.9.9.9;")
	contains(t, ts.ok("delete system ntp"), "warning: statement not found")
	ts.ok("deactivate vlans a")
	contains(t, ts.ok("show vlans"), "inactive: a {")
	contains(t, ts.ok("show | display set"), "deactivate vlans a")
	ts.ok("activate vlans a")
	ts.ok("copy vlans a to b")
	ts.ok("rename vlans b to c")
	contains(t, ts.ok("show vlans"), "c {\n    vlan-id 10;")
	contains(t, ts.run("copy vlans a to c"), "already exists")
	contains(t, ts.run("copy vlans a"), "expecting '<path> to <name>'")
	ts.ok("edit vlans")
	ts.term.answers = []string{"no"}
	ts.ok("delete")
	contains(t, ts.ok("show"), "vlan-id")
	ts.term.answers = []string{"yes"}
	ts.ok("delete")
	if out := ts.ok("show"); strings.Contains(out, "vlan-id") {
		t.Errorf("delete at level kept:\n%s", out)
	}
	ts.ok("top")
	contains(t, ts.ok("show"), "name-server 9.9.9.9;") // outside the level: untouched
}

func TestLoadSave(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	ts.term.text = "system { host-name loaded; }\n"
	contains(t, ts.ok("load merge terminal"), "load complete")
	ts.term.files["cfg.txt"] = "set vlans x vlan-id 7\n"
	ts.ok("load merge cfg.txt")
	contains(t, ts.ok("show"), "host-name loaded;", "vlan-id 7;")
	ts.term.files["bad.txt"] = "set vlans y vlan-id 8\nset nonsense\n"
	contains(t, ts.run("load merge bad.txt"), "load failed, candidate unchanged", "line 2")
	contains(t, ts.run("load merge missing.txt"), "no such file")
	contains(t, ts.ok("save out.conf"), "Wrote")
	if !strings.Contains(ts.term.files["out.conf"], "host-name loaded;") {
		t.Errorf("saved: %q", ts.term.files["out.conf"])
	}
	ts.ok("edit vlans")
	ts.term.text = "set z vlan-id 9"
	ts.ok("load set terminal")
	contains(t, ts.ok("show"), "z {")
	contains(t, ts.run("load override terminal"), "top level")
	ts.ok("top")
	ts.term.text = "system { host-name only; }"
	ts.ok("load override terminal")
	if out := ts.ok("show"); strings.Contains(out, "vlans") {
		t.Errorf("override kept vlans:\n%s", out)
	}
}

func TestCommitVariants(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.ok("configure")
	ts.ok("set vlans a vlan-id 10")
	ts.ok("set vlans b vlan-id 10")
	out := ts.run("commit check")
	contains(t, out, "error:", "configuration check-out failed")
	contains(t, ts.run("commit"), "check-out failed")
	ts.ok("delete vlans b")
	contains(t, ts.ok("commit check"), "configuration check succeeds")
	contains(t, ts.ok("commit confirmed 5 comment test"), "rolled back in 5 minutes")
	contains(t, ts.ok("confirm"), "commit confirmed")
	contains(t, ts.run("confirm"), "no commit pending")
	contains(t, ts.ok("commit"), "no changes")
	ts.ok("set system host-name x")
	contains(t, ts.ok("commit and-quit"), "Exiting configuration mode")
	if ts.sh.InConfig() {
		t.Error("and-quit stayed in configuration mode")
	}
	contains(t, ts.ok("confirm"), "commit confirmed")
	contains(t, ts.run("configure"), "Entering")
	contains(t, ts.run("commit confirmed 99"), "1..60")
	contains(t, ts.run("commit check and-quit"), "syntax error")
	ts.ok("rollback 1")
	contains(t, ts.ok("show | compare"), "-       host-name x;")
	contains(t, ts.ok("show | compare rollback 1"), "") // no crash
	contains(t, ts.run("rollback 99"), "no such revision")
}

func TestPrivateAndExclusive(t *testing.T) {
	e := newEngine(t)
	a := newTester(t, e, "alice", commit.SuperUser)
	b := newTester(t, e, "bob", commit.SuperUser)
	contains(t, a.ok("configure private"), "(private)")
	a.ok("set system host-name mine")
	contains(t, b.ok("configure exclusive"), "users currently editing the configuration: alice (private)")
	contains(t, a.run("commit"), "locked by bob")
	b.ok("exit")
	a.term.answers = []string{"no"}
	a.ok("exit")
	if !a.sh.InConfig() {
		t.Fatal("left private mode without confirmation")
	}
	a.term.answers = []string{"yes"}
	contains(t, a.ok("exit"), "Exiting")
	contains(t, a.ok("configure"), "Entering")
	contains(t, a.ok("status"), "alice (shared)")
	contains(t, a.run("update"), "only available")
}

func TestPermissions(t *testing.T) {
	e := newEngine(t)
	ro := newTester(t, e, "ro", commit.ReadOnly)
	contains(t, ro.run("configure"), "permission denied")
	if strings.Contains(ro.sh.Help(""), "configure") {
		t.Error("read-only user sees configure")
	}
	contains(t, ro.ok("show configuration"), "")
	op := newTester(t, e, "op", commit.Operator)
	op.ok("configure")
	op.ok("set system login user eve class super-user")
	contains(t, op.run("commit"), "permission denied: class operator")
}

func TestPipes(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	for _, l := range []string{"set vlans a vlan-id 10", "set vlans b vlan-id 11", "set vlans c vlan-id 12"} {
		ts.ok(l)
	}
	if out := ts.ok("show | display set | match vlan-id | except b"); out != "set vlans a vlan-id 10\nset vlans c vlan-id 12\n" {
		t.Errorf("match/except: %q", out)
	}
	if out := ts.ok("show | display set | count"); out != "Count: 3 lines\n" {
		t.Errorf("count: %q", out)
	}
	if out := ts.ok("show | display set | last 1"); out != "set vlans c vlan-id 12\n" {
		t.Errorf("last: %q", out)
	}
	if out := ts.ok("show | display set | find b"); !strings.HasPrefix(out, "set vlans b") {
		t.Errorf("find: %q", out)
	}
	rep := ts.sh.Execute(context.Background(), "show | no-more", ts.term)
	if !rep.NoMore {
		t.Error("no-more not signalled")
	}
	contains(t, ts.ok("show vlans a | display json"), `"vlan-id": "10"`)
	contains(t, ts.run("show | match ("), "invalid pattern")
	contains(t, ts.run("show | display xml"), "expecting 'set'")
	contains(t, ts.run("show | compare | display set"), "cannot be combined")
	contains(t, ts.run("show |"), "missing pipe command")
}

func completions(cs []Completion) string {
	var w []string
	for _, c := range cs {
		w = append(w, c.Text)
	}
	return strings.Join(w, " ")
}

func TestCompletion(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	check := func(line, want string) {
		t.Helper()
		if got := completions(ts.sh.Complete(line)); !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("Complete(%q) = %q, want %q", line, got, want)
		}
	}
	check("con", "configure")
	check("show ", "configuration")
	check("configure ", "private")
	ts.ok("configure")
	check("", "set")
	check("set sys", "system")
	check("set system ", "host-name")
	check("set system syslog host 1.2.3.4 transport ", "tls")
	check("set interfaces ", "1/0/1")
	check("set interfaces ", "irb")
	check("set interfaces ", "ae<N>")
	check("set interfaces i", "irb")
	contains(t, ts.sh.Help("set interfaces "), "> irb ", "VLAN IP interfaces", "ae<N>", "Interface (hardware)")
	ts.ok("set interfaces irb unit 10 family inet address 10.0.0.1/24")
	check("set vlans v l3-interface ", "irb.10")
	ts.ok("set vlans users vlan-id 10")
	check("set interfaces 1/0/0 unit 0 family ethernet-switching vlan members ", "users")
	check("set vlans ", "users")
	check("set protocols rstp ", "<[Enter]>")
	check("show | ", "display")
	check("show | display ", "set")
	check("commit ", "confirmed")
	check("commit confirmed ", "<minutes>")
	check("load ", "merge")
	check("load merge ", "terminal")
	check("rollback ", "0")
	check("run show ", "version")
	check("copy vlans users ", "to")
	if got := completions(ts.sh.Complete("set system host-name core ")); got != "<[Enter]>" {
		t.Errorf("completion after a complete leaf: %q", got)
	}
	h := ts.sh.Help("set system ")
	contains(t, h, "Possible completions:", "  host-name ", "> login ", "+ name-server ")
	contains(t, ts.sh.Help("set bogus "), "No valid completions")
}

func TestPanicRecovery(t *testing.T) {
	sh := New(Env{User: "x", Class: commit.SuperUser, Log: quiet}) // no engine: every engine call panics
	rep := sh.Execute(context.Background(), "show configuration", &scriptTerm{})
	if !strings.Contains(rep.Output, "internal error") {
		t.Fatalf("panic not reported: %q", rep.Output)
	}
	if cs := sh.Complete("show configuration "); cs != nil {
		t.Errorf("completion after panic: %v", cs)
	}
	// The shell still works afterwards.
	if rep := sh.Execute(context.Background(), "show version", &scriptTerm{}); !strings.Contains(rep.Output, "switchd") {
		t.Errorf("shell unusable after panic: %q", rep.Output)
	}
}

// FuzzShell feeds arbitrary lines to a configuration-mode shell: nothing
// may panic (an internal error counts as a failure).
func FuzzShell(f *testing.F) {
	for _, s := range []string{
		"set system host-name x", "show | display set | match a", "edit interfaces 1/0/1",
		"delete", "commit check", "load merge terminal", "set system name-server [ 1.1.1.1", "up 3",
		"copy vlans a to b", "rollback 0", "run show system commit", "show | compare rollback 2",
		"deactivate system", "exit", "configure private", `set system login message "a|b"`,
	} {
		f.Add(s)
	}
	e := newEngine(f)
	f.Fuzz(func(t *testing.T, line string) {
		sh := New(Env{Engine: e, User: "fuzz", Class: commit.SuperUser, Log: quiet})
		term := &scriptTerm{files: map[string]string{}, text: line, answers: []string{"yes"}}
		defer sh.Close()
		for _, l := range []string{"configure", line, line + " ", "show | display set"} {
			rep := sh.Execute(context.Background(), l, term)
			if strings.Contains(rep.Output, "internal error") {
				t.Fatalf("%q: %s", l, rep.Output)
			}
			_ = sh.Complete(l)
			_ = sh.Help(l)
		}
	})
}

type fakeOps struct {
	cleared []string
	power   []string
	memory  *MemoryStatus
}

func (f *fakeOps) Neighbors(ipv6 bool) ([]Neighbor, error) {
	if ipv6 {
		return []Neighbor{{MAC: "02:00:00:00:00:09", IP: "fe80::1", Interface: "mgmt0", Instance: "mgmt", State: "reachable"}}, nil
	}
	return []Neighbor{
		{MAC: "02:00:00:00:00:02", IP: "10.5.0.1", Interface: "1/2/0", Instance: "default", State: "reachable"},
		{MAC: "02:00:00:00:00:01", IP: "192.168.98.2", Interface: "mgmt0", Instance: "mgmt", State: "stale"},
	}, nil
}

func (f *fakeOps) Processes() ([]Process, error) {
	return []Process{
		{Program: "switchd", State: "running", PID: 812, Since: time.Now().Add(-3 * time.Hour), Memory: 48 << 20, CPU: 95 * time.Second, Scheduling: "nice -10"},
		{Program: "switchd-update", State: "restarting", Restarts: 2, LastFailure: "killed by signal SEGV", FailedAt: time.Now().Add(-90 * time.Second), Scheduling: "nice 0"},
	}, nil
}

func (f *fakeOps) RestartDaemon(name, user string) error {
	f.power = append(f.power, "restart "+name+" "+user)
	return nil
}

func (f *fakeOps) StopDaemon(name, user string) (string, error) {
	f.power = append(f.power, "stop "+name+" "+user)
	return "BGP", nil
}

func (f *fakeOps) StartDaemon(name, user string) error {
	f.power = append(f.power, "start "+name+" "+user)
	return nil
}

func (f *fakeOps) Uptime() (Uptime, error) {
	return Uptime{Booted: time.Now().Add(-26 * time.Hour), Started: time.Now().Add(-90 * time.Second), Load: [3]float64{0.5, 0.25, 0.125}}, nil
}

func (f *fakeOps) NTP() (NTPStatus, error) {
	return NTPStatus{Synced: true, LastAdjust: time.Now().Add(-90 * time.Second), Via: "mgmt_ceros", Servers: []NTPServerStatus{
		{Host: "10.0.0.1", Prefer: true, Addr: "10.0.0.1", Stratum: 2, Offset: 1500 * time.Microsecond, Delay: 800 * time.Microsecond, LastPoll: time.Now().Add(-30 * time.Second), Reach: true, Selected: true},
		{Host: "ntp.example.net", LastPoll: time.Now().Add(-30 * time.Second), Err: "no answer"},
		{Host: "10.0.0.9"}}}, nil
}

func (f *fakeOps) Power(action string, minutes int, user string) error {
	f.power = append(f.power, fmt.Sprintf("%s %d %s", action, minutes, user))
	return nil
}

func (f *fakeOps) Offload() ([]OffloadPort, error) {
	return []OffloadPort{{Name: "1/0/0", Linux: "enp1s0f0", Driver: "tg3", MaxSpeedMbps: 1000, Pause: "yes", TC: "-", VLANFilter: "-", Csum: "on", TSO: "on", GRO: "on"}}, nil
}

func (f *fakeOps) Routes(instance string) ([]Route, error) {
	if instance == "nope" {
		return nil, fmt.Errorf("routing instance nope does not exist on this member")
	}
	return []Route{{Dest: "::/0", Proto: "static", Metric: 20, Via: "fd00::1 via irb.10"},
		{Dest: "0.0.0.0/0", Proto: "static", Metric: 20, Via: "10.5.0.1 via 1/2/0"},
		{Dest: "10.5.0.0/16", Proto: "direct", Via: "1/2/0"}}, nil
}

func (f *fakeOps) LLDP() (LLDPStatus, error) {
	return LLDPStatus{Running: true,
		System: lldp.System{ChassisMAC: []byte{2, 1, 1, 1, 1, 1}, Name: "core", Desc: "cerOS t", Caps: lldp.CapBridge, Enabled: lldp.CapBridge, Interval: 30, Hold: 4},
		Ports:  []lldp.PortSpec{{Name: "1/0/1", PVID: 10, MaxFrame: 1514}, {Name: "2/0/1", Bundle: "ae1", MaxFrame: 9014}},
		Stats:  []lldp.Stats{{Port: "1/0/1", Sent: 5, Received: 3}},
		Neighbors: []lldp.Neighbor{
			{Port: "2/0/1", Chassis: "aa:bb:cc:00:00:01", PortID: "Gi0/1", PortDesc: "to core", SysName: "access-1", Caps: lldp.CapBridge, Expires: time.Now().Add(100 * time.Second)},
			{Port: "1/0/1", Chassis: "aa:bb:cc:00:00:02", PortID: "eth0", SysName: "server", Mgmt: []string{"10.0.0.5"}, Expires: time.Now().Add(90 * time.Second)},
		}}, nil
}

func (f *fakeOps) VirtualChassis() (VCStatus, error) {
	return VCStatus{StackID: "abc", Member: 1, HostName: "sw1", Ports: []VCPort{
		{Port: "0/1", Linux: "ens19", State: "up", Neighbor: "member 2 (sw2)", PeerPort: "0/1", UpSince: time.Now().Add(-time.Minute)},
		{Port: "0/2", Linux: "ens20", State: "up", Neighbor: "other stack", LastError: "x"},
	}, Control: true, Master: 2, Members: []int{1, 2, 3}, Voters: []int{1, 2, 3}, Reachable: []int{2}}, nil
}

func (f *fakeOps) SetVCPort(local string, add bool, user string) error {
	f.power = append(f.power, fmt.Sprintf("vc %s %v", local, add))
	return nil
}

func (f *fakeOps) AddVCMember(id int, user string) (string, error) { return "AAAA-BBBB", nil }
func (f *fakeOps) LACP() ([]lacp.BundleStatus, error) {
	up := lacp.Activity | lacp.Timeout | lacp.Aggregation | lacp.Sync | lacp.Collecting | lacp.Distributing
	sys := lacp.SystemID{Priority: 32768, MAC: [6]byte{2, 0x11, 0x22, 0x33, 0x44, 0x55}}
	psys := lacp.SystemID{Priority: 65535, MAC: [6]byte{2, 0, 0, 0, 0, 0x22}}
	return []lacp.BundleStatus{{Name: "ae1", PortNames: map[string]string{"ens21": "1/2/0", "ens22": "1/3/0"}, Ports: []lacp.PortStatus{
		{Name: "ens21", Actor: lacp.Info{System: sys, Key: 2, Port: 1152, State: up}, Partner: lacp.Info{System: psys, Key: 9, Port: 1, State: up},
			Rx: lacp.RxCurrent, Mux: lacp.MuxCollectingDistributing, Selected: true, Stats: lacp.Stats{RxPDUs: 30, TxPDUs: 31}},
		{Name: "ens22", Actor: lacp.Info{System: sys, Key: 2, Port: 1216, State: lacp.Activity | lacp.Timeout | lacp.Aggregation | lacp.Defaulted},
			Rx: lacp.RxDefaulted, Mux: lacp.MuxDetached, Stats: lacp.Stats{TxPDUs: 5, RxErrors: 1}},
	}}}, nil
}

func (f *fakeOps) MCLAG() ([]MCLAGStatus, error) {
	return []MCLAGStatus{
		{Pair: 34, Member: 1, Peer: 2, Primary: true, PeerReachable: true, Reach: 3, Members: 3,
			PeerKnown: true, PeerSeen: time.Now(), Bundles: []MCLAGBundle{
				{Name: "ae1", LocalUp: true, PeerUp: true, PeerKnown: true, SplitHorizon: true, Facts: "x", PeerFacts: "x"},
				{Name: "ae2", LocalUp: false, PeerUp: false, PeerKnown: true, Hold: "delay-restore (4m50s left)"},
			}},
		{Pair: 34, Member: 2, Peer: 1, PeerReachable: true, Reach: 3, Members: 3,
			PeerKnown: true, PeerSeen: time.Now(), Bundles: []MCLAGBundle{
				{Name: "ae1", LocalUp: true, PeerUp: true, PeerKnown: true, SplitHorizon: true, Facts: "x", PeerFacts: "x"},
				{Name: "ae2", LocalUp: false, PeerUp: false, PeerKnown: true},
			}},
	}, nil
}

func (f *fakeOps) VXLAN() ([]VXLANStatus, error) {
	return []VXLANStatus{{Member: 1, Ports: []VXLANPort{{VNI: 10010, VLAN: 10, Port: "swvx10010", Up: true, Remotes: []string{"10.200.1.10"}, RemoteMACs: 3}},
		Routes: []VTEPRoute{{VTEP: "10.200.1.10", Via: "10.99.0.0", Interface: "1/5/0"}}},
		{Member: 2, Routes: []VTEPRoute{{VTEP: "10.200.1.10", NoRoute: true}}}}, nil
}

func (f *fakeOps) Bottlenecks() ([]diag.Finding, error) {
	return []diag.Finding{{Severity: diag.Limit, Area: "PCIe", Subject: "card 1", Text: "the link carries 16 Gbit/s", Advice: "use another slot"}}, nil
}

func (f *fakeOps) Multicast() ([]McastStatus, error) {
	return []McastStatus{
		{Member: 1, Groups: []McastGroup{{VLAN: 10, Group: "239.1.1.1", Interface: "1/0/1", Expires: 250},
			{VLAN: 10, Group: "239.1.1.1", Interface: "vc-2", Expires: 250}, {VLAN: 10, Group: "ff0e::1", Interface: "1/0/1", Expires: 200}},
			Routers: []McastRouter{{VLAN: 10, Interface: "vc-2", Permanent: true}, {VLAN: 10, Interface: "1/0/2", Expires: 200}},
			VLANs:   []McastVLAN{{VLAN: 10, Snooping: true}}},
		{Member: 2, Groups: []McastGroup{{VLAN: 10, Group: "239.1.1.1", Interface: "ae1", Static: true}},
			VLANs: []McastVLAN{{VLAN: 10, Snooping: true}}},
	}, nil
}

func (f *fakeOps) Limits() (LimitsStatus, error) {
	return LimitsStatus{Member: 1, Ports: 9, StackPorts: 2, LowestMaxMTU: 9014, LowestMaxPort: "1/1/0", HighestMaxMTU: 16014, HighestMaxPort: "1/3/0",
		FastestMbps: 10000, FastestPort: "1/3/0", MACEntries: 12, Memory: f.memory}, nil
}

func (f *fakeOps) StackMTU() (StackMTUStatus, error) {
	return StackMTUStatus{Member: 1, DataMTU: 9014, Where: "vlans storage mtu", Stack: true, Ports: []StackMTUPort{
		{Port: "1/5/0", MTU: 9216, MaxMTU: 9216, PathMTU: 9202}, {Port: "1/5/1", MTU: 1514, MaxMTU: 16058},
		{Port: "1/5/2", MTU: 9000, MaxMTU: 9050}, {Port: "1/5/3", MTU: 9216, MaxMTU: 9216, PathMTU: 1500}}}, nil
}

func (f *fakeOps) SwitchMaster(to int, user string) error { return nil }
func (f *fakeOps) ForceMaster(user string) error          { return nil }
func (f *fakeOps) Cards() ([]CardStatus, error) {
	return []CardStatus{{Number: 0, Key: "pci:0000:01:00", Present: true, Driver: "tg3", Ports: 4, MovedFrom: -1}}, nil
}
func (f *fakeOps) CardInterfaces(cards ...int) []string         { return nil }
func (f *fakeOps) CardRenumber(from, to int, user string) error { return nil }
func (f *fakeOps) CardForget(card int, user string) error       { return nil }

func (f *fakeOps) VLANMTUDrops() (map[int]uint64, error) { return map[int]uint64{10: 7}, nil }

func (f *fakeOps) DHCPBindings() ([]DHCPBinding, error) {
	return []DHCPBinding{{Unit: "1/0/6.0", State: "bound", Address: "10.1.2.50/24", Server: "10.1.2.1", Router: "10.1.2.1",
		DNS: []string{"10.1.2.53"}, Lease: time.Hour, Renew: time.Now().Add(30 * time.Minute), Expires: time.Now().Add(time.Hour)}}, nil
}

func (f *fakeOps) SpanningTree() (STPStatus, error) {
	return STPStatus{Running: true, Owner: 1, BridgeID: "32768.02:aa:bb:cc:dd:ee", RootID: "4096.02:00:00:00:00:01", RootCost: 2000,
		RootPort: "ae1", HelloTime: 2, MaxAge: 20, ForwardDelay: 15, Changes: 3, Ports: []STPPort{
			{Name: "ae1", Role: "root", State: "forwarding", Cost: 2000, PortID: "128.3840", DesignatedBridge: "4096.02:00:00:00:00:01", DesignatedPort: "128.1", P2P: true, RSTP: true, Enabled: true, Rx: 10, Tx: 2},
			{Name: "1/0/3", Role: "designated", State: "forwarding", Cost: 20000, PortID: "128.3", DesignatedBridge: "32768.02:aa:bb:cc:dd:ee", DesignatedPort: "128.3", Edge: true, OperEdge: true, P2P: true, RSTP: true, Enabled: true},
			{Name: "2/0/4", Role: "alternate", State: "discarding", Cost: 20000, PortID: "128.260", DesignatedBridge: "8192.02:00:00:00:00:02", DesignatedPort: "128.7", P2P: true, Enabled: true, RootInconsistent: true},
		}}, nil
}

func (f *fakeOps) Maintenance(enter, force bool, user string) (string, error) {
	return fmt.Sprintf("maintenance enter=%v force=%v by %s\n", enter, force, user), nil
}
func (f *fakeOps) RemoveVCMember(id int, user string) error { return nil }
func (f *fakeOps) JoinVC(token, user string) (int, error)   { return 2, nil }

func (f *fakeOps) CancelPower(user string) error {
	f.power = append(f.power, "cancel "+user)
	return nil
}

func (f *fakeOps) Interfaces() ([]IfStatus, error) {
	return []IfStatus{
		{Name: "1/1/0", Linux: "ens19", Configured: true, Role: "access v10", AdminUp: true, OperUp: true, MTU: 1514,
			SpeedMbps: 10000, Description: "server", VLANs: []string{"v10 (10, untagged)"}, TaggedDrops: 7,
			Counters: IfCounters{RxPackets: 5, RxErrors: 1}},
		{Name: "1/2/0", Linux: "ens2", MTU: 1514},
	}, nil
}

func (f *fakeOps) Hardware() ([]HardwarePort, error) {
	return []HardwarePort{{Name: "1/1/0", Linux: "ens19", Bus: "0000:00:13.0", Driver: "virtio_net", MAC: "02:00:00:00:00:13"}}, nil
}

func (f *fakeOps) MACTable() ([]MACEntry, error) {
	return []MACEntry{
		{VLAN: 20, VLANName: "v20", MAC: "02:00:00:00:00:02", Interface: "1/2/0", Age: 3},
		{VLAN: 10, VLANName: "v10", MAC: "02:00:00:00:00:01", Interface: "1/1/0", Age: 1},
	}, nil
}

func (f *fakeOps) ClearMACTable(vlan int, iface string) (int, error) {
	f.cleared = append(f.cleared, fmt.Sprintf("%d/%s", vlan, iface))
	return 1, nil
}

func TestOperationalCommands(t *testing.T) {
	e := newEngine(t)
	ops := &fakeOps{}
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.sh.env.Ops = ops
	ts.ok("configure")
	ts.ok("set vlans v10 vlan-id 10")
	ts.ok("set vlans v20 vlan-id 20")
	ts.ok("set interfaces 1/1/0 unit 0 family ethernet-switching vlan members v10")
	ts.ok("set interfaces 1/2/0 unit 0 family ethernet-switching interface-mode trunk")
	ts.ok("set interfaces 1/2/0 unit 0 family ethernet-switching vlan members [ v10 v20 ]")
	ts.ok("commit")
	ts.ok("exit")

	out := ts.ok("show interfaces terse")
	contains(t, out, "1/1/0          up    up    1514   10G    access v10", "1/2/0          down  down", "(not configured)")
	contains(t, ts.run("show interfaces 1/1/0 extensive"), "Description: server", "VLANs: v10 (10, untagged)",
		"Input errors: 1", "Tagged frames dropped (access port): 7")
	contains(t, ts.run("show interfaces nope"), "not found")
	contains(t, ts.ok("show chassis hardware"), "1/1/0      ens19            0000:00:13.0   virtio_net   02:00:00:00:00:13")
	out = ts.ok("show ethernet-switching table")
	if i, j := strings.Index(out, "02:00:00:00:00:01"), strings.Index(out, "02:00:00:00:00:02"); i < 0 || j < i {
		t.Errorf("mac table not sorted by VLAN:\n%s", out)
	}
	contains(t, out, "2 entries")
	contains(t, ts.ok("show ethernet-switching table vlan v20"), "1 entries", "1/2/0")
	contains(t, ts.ok("show ethernet-switching table interface 1/1/0"), "1 entries")
	contains(t, ts.run("show ethernet-switching table vlan nope"), "unknown VLAN")
	contains(t, ts.ok("show vlans"), "v10            10", "1/1/0, 1/2/0*", "v20            20", "* = tagged")
	contains(t, ts.ok("clear ethernet-switching table vlan 10 interface 1/1/0"), "1 entries cleared")
	if len(ops.cleared) != 1 || ops.cleared[0] != "10/1/1/0" {
		t.Errorf("clear: %v", ops.cleared)
	}
	ro := newTester(t, e, "ro", commit.ReadOnly)
	ro.sh.env.Ops = ops
	contains(t, ro.run("clear ethernet-switching table"), "permission denied")
	contains(t, ro.ok("show vlans"), "v10")
	noOps := newTester(t, e, "x", commit.SuperUser)
	contains(t, noOps.run("show interfaces"), "not available")
	check := completions(ts.sh.Complete("show ethernet-switching table vlan "))
	if !strings.Contains(check, "v10") {
		t.Errorf("vlan completion: %s", check)
	}
}

type fakeLogs struct{}

func (fakeLogs) Recent() ([]LogLine, error) {
	return []LogLine{{Time: time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local), Facility: "change-log", Severity: "info", Text: "commit revision=2", App: "switchd", PID: 812},
		{Time: time.Date(2026, 9, 29, 12, 0, 1, 0, time.Local), Facility: "kernel", Severity: "warning", Text: "eth2: NIC Link is Down", App: "kernel", Host: "sw2"}}, nil
}

func (fakeLogs) Forwarders() ([]ForwarderStatus, error) {
	return []ForwarderStatus{{Target: "10.0.0.5:6514/tls", Filter: "any/info", Sent: 3, Queued: 2, Dropped: 1, LastError: "connection refused"}}, nil
}

func TestShowLog(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	contains(t, ts.run("show log"), "not available")
	ts.sh.env.Logs = fakeLogs{}
	contains(t, ts.ok("show log"), "2026-09-29 12:00:00 sw1 change-log.info: switchd[812]: commit revision=2",
		"2026-09-29 12:00:01 sw2 kernel.warning: kernel: eth2: NIC Link is Down")
	out := ts.run("show system syslog")
	contains(t, out, "10.0.0.5:6514/tls (any/info): not connected, sent 3, queued 2, dropped 1", "last error: connection refused")
	contains(t, ts.ok("show log | match revision | count"), "Count: 1 lines")
}

func TestPlainTextPassword(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ts.ok("configure")
	ts.term.answers = []string{"correct horse", "correct horse"}
	ts.ok("set system login user bob authentication plain-text-password")
	hash := ts.sh.sess.Candidate().Root.Leaf("system", "login", "user", "bob", "authentication", "encrypted-password")
	if !access.CheckPassword("correct horse", hash) {
		t.Fatalf("stored hash %q does not match", hash)
	}
	if out := ts.ok("show | display set"); strings.Contains(out, "correct horse") || strings.Contains(out, "plain-text") {
		t.Errorf("plain text leaked into the configuration:\n%s", out)
	}
	ts.term.answers = []string{"aaaaaaaa", "bbbbbbbb"}
	contains(t, ts.run("set system login user bob authentication plain-text-password"), "do not match")
	ts.term.answers = []string{"short", "short"}
	contains(t, ts.run("set system login user bob authentication plain-text-password"), "at least 8")
	if ts.sh.sess.Candidate().Root.Leaf("system", "login", "user", "bob", "authentication", "encrypted-password") != hash {
		t.Error("failed attempts changed the password")
	}
	contains(t, ts.run("set system host-name plain-text-password"), "only valid below")
	ts.ok("edit system login user carol authentication")
	ts.term.answers = []string{"12345678", "12345678"}
	ts.ok("set plain-text-password")
	if got := completions(ts.sh.Complete("set ")); !strings.Contains(got, "plain-text-password") {
		t.Errorf("completion at the authentication level: %s", got)
	}
	// root (system root-authentication).
	ts.ok("top")
	ts.term.answers = []string{"root pass 1", "root pass 1"}
	ts.ok("set system root-authentication plain-text-password")
	if h := ts.sh.sess.Candidate().Root.Leaf("system", "root-authentication", "encrypted-password"); !access.CheckPassword("root pass 1", h) {
		t.Fatalf("root hash %q does not match", h)
	}
	if got := completions(ts.sh.Complete("set system root-authentication ")); !strings.Contains(got, "plain-text-password") {
		t.Errorf("completion below root-authentication: %s", got)
	}
}

func TestStartShell(t *testing.T) {
	e := newEngine(t)
	su := newTester(t, e, "alice", commit.SuperUser)
	if rep := su.sh.Execute(context.Background(), "start shell", su.term); !rep.Shell || rep.Output != "" {
		t.Errorf("start shell: %+v", rep)
	}
	op := newTester(t, e, "op", commit.Operator)
	if rep := op.sh.Execute(context.Background(), "start shell", op.term); rep.Shell || !strings.Contains(rep.Output, "permission denied") {
		t.Errorf("operator start shell: %+v", rep)
	}
}

func TestShowSystemRollback(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.ok("configure")
	ts.ok("set system host-name first")
	ts.ok("commit comment \"one\"")
	ts.ok("set system host-name second")
	ts.ok("set vlans v10 vlan-id 10")
	ts.ok("commit")
	ts.ok("exit")
	out := ts.ok("show system rollback 1")
	contains(t, out, "## Revision 1: ", "by alice: one", "host-name first;")
	if strings.Contains(out, "v10") {
		t.Errorf("revision 1 shows later changes:\n%s", out)
	}
	contains(t, ts.ok("show system rollback 1 | display set"), "set system host-name first")
	if out := ts.ok("show system rollback 1 | display set"); strings.Contains(out, "##") {
		t.Errorf("display set must stay loadable:\n%s", out)
	}
	contains(t, ts.ok("show system rollback 1 compare 0"), "-   host-name first;", "+   host-name second;", "+   v10 {")
	contains(t, ts.ok("show system rollback 0 compare 1"), "+   host-name first;")
	contains(t, ts.run("show system rollback 99"), "revision 99 does not exist")
	contains(t, ts.run("show system rollback"), "missing argument")
	contains(t, ts.run("show system rollback 1 compare"), "syntax error")
	contains(t, ts.run("show system rollback x"), "expecting a revision number")
	if cs := ts.sh.Complete("show system rollback "); len(cs) < 3 || cs[0].Text != "0" {
		t.Errorf("completion: %v", cs)
	}
}

func TestSystemOperationalCommands(t *testing.T) {
	e := newEngine(t)
	ops := &fakeOps{}
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.sh.env.Ops = ops
	out := ts.ok("show arp no-resolve")
	contains(t, out, "02:00:00:00:00:02  10.5.0.1         1/2/0        default    reachable", "mgmt0        mgmt       stale", "Total entries: 2")
	if strings.Index(out, "10.5.0.1") > strings.Index(out, "192.168.98.2") {
		t.Errorf("not sorted by instance:\n%s", out)
	}
	contains(t, ts.ok("show ipv6 neighbors"), "fe80::1")
	out = ts.ok("show route")
	contains(t, out, "Routing instance default: 3 routes", "0.0.0.0/0                    static   20      10.5.0.1 via 1/2/0")
	if strings.Index(out, "::/0") < strings.Index(out, "10.5.0.0/16") {
		t.Errorf("IPv4 first:\n%s", out)
	}
	contains(t, ts.ok("show route instance mgmt_ceros"), "Routing instance mgmt_ceros")
	contains(t, ts.ok("show virtual-chassis"), "Virtual chassis abc, this switch is member 1",
		"1       sw1                  backup    128       voter      present",
		"2                            master    128       voter      present",
		"3                            linecard  128       voter      not present")
	ts.ok("request chassis routing-engine master switch member 2")
	contains(t, ts.run("request chassis routing-engine master switch member x"), "expecting a member id")
	contains(t, ts.ok("show virtual-chassis vc-port"), "0/1        ens19        up      -       member 2 (sw2)           0/1        00:01:00", "other stack")
	ts.ok("request virtual-chassis vc-port set pic-slot 0 port 3")
	if len(ops.power) != 1 || ops.power[0] != "vc 0/3 true" {
		t.Errorf("vc-port set: %v", ops.power)
	}
	ops.power = nil
	ts.ok("request virtual-chassis vc-port delete 1/1/0")
	if len(ops.power) != 1 || ops.power[0] != "vc 1/0 false" {
		t.Errorf("vc-port delete 1/1/0: %v", ops.power)
	}
	contains(t, ts.run("request virtual-chassis vc-port set 2/1/0"), "port of member 2; this switch is member 1")
	contains(t, ts.run("request virtual-chassis vc-port set ens19"), "expecting a port")
	ops.power = nil
	contains(t, ts.run("request virtual-chassis vc-port set pic-slot 0"), "syntax error")
	contains(t, ts.run("request virtual-chassis vc-port set pic-slot x port 1"), "expecting a card number")
	contains(t, ts.ok("request virtual-chassis member add 2"), "Join token for member 2", "request virtual-chassis join token AAAA-BBBB")
	contains(t, ts.run("request virtual-chassis member add 17"), "expecting a member id")
	ts.term.answers = []string{"yes"}
	contains(t, ts.ok("request virtual-chassis join token AAAA-BBBB"), "Joined as member 2")
	contains(t, ts.run("show route instance nope"), "does not exist")
	contains(t, ts.ok("show system offload"), "1/0/0      enp1s0f0     tg3         1G     yes   no        -    -     on   on   on")
	contains(t, ts.ok("show system ntp"), "Synchronized: yes, clock slewed 00:01:30 ago", "Queries leave through: mgmt_ceros",
		"* 10.0.0.1 (prefer)", "1.5ms", "ntp.example.net", "(no answer)", "not queried yet")
	contains(t, ts.ok("show system processes"), "Program         State         PID      Uptime     Restarts  Memory   CPU       Scheduling",
		"switchd         running       812      03:00:00   0         48.0M    1m35s     nice -10",
		"switchd-update  restarting    -        -          2", "Last failures:", "switchd-update: killed by signal SEGV (00:01:30 ago)")
	ops.power = nil
	contains(t, ts.ok("request daemon stop bgp"), "cer-bgpd stopped until the reboot or 'request daemon start bgp'")
	contains(t, ts.ok("request daemon start bgp"), "cer-bgpd may run again")
	contains(t, ts.run("request daemon stop update"), "error")
	ops.power = nil
	contains(t, ts.ok("request daemon restart update"), "switchd-update restarted")
	ops.power = nil
	contains(t, ts.ok("restart update"), "switchd-update restarted")
	if len(ops.power) != 1 || !strings.HasPrefix(ops.power[0], "restart update ") {
		t.Errorf("restart: %v", ops.power)
	}
	ops.power = nil
	contains(t, ts.ok("show system uptime"), "Current time: ", "System booted: ", "(1d 02:00 ago)", "switchd started: ", "(00:01:30 ago)", "Load averages: 0.50 0.25 0.12")

	ts.term.answers = []string{"no"}
	ts.ok("request system reboot")
	ts.term.answers = []string{"yes"}
	contains(t, ts.ok("request system reboot in 5"), "scheduled in 5 minutes")
	ts.term.answers = []string{"yes"}
	contains(t, ts.ok("request system power-off"), "Power off requested")
	contains(t, ts.ok("clear system reboot"), "cancelled")
	if strings.Join(ops.power, ",") != "reboot 5 alice,power-off 0 alice,cancel alice" {
		t.Errorf("power: %v", ops.power)
	}
	contains(t, ts.run("request system reboot in x"), "expecting minutes")
	op := newTester(t, e, "bob", commit.Operator)
	op.sh.env.Ops = ops
	contains(t, op.run("request system reboot"), "permission denied")
}

type fakeStack struct {
	mu    sync.Mutex
	calls []string
}

func (*fakeStack) Self() int      { return 1 }
func (*fakeStack) Members() []int { return []int{1, 2, 3} }
func (f *fakeStack) Exec(_ context.Context, member int, line string, confirmed bool) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%d %s %v", member, line, confirmed))
	f.mu.Unlock()
	if member == 3 {
		return "", errors.New("member 3 is not reachable")
	}
	return fmt.Sprintf("remote %d: %s\n", member, line), nil
}

// Operational commands on other stack members (reference 5.2).
// show lldp: one table for the stack, sorted by port; a target narrows it.
func TestShowLLDP(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.sh.env.Ops, ts.sh.env.Stack = &fakeOps{}, &fakeStack{}
	out := ts.ok("show lldp neighbors")
	contains(t, out, "1/0/1      aa:bb:cc:00:00:02", "2/0/1      aa:bb:cc:00:00:01", "2 neighbours")
	if strings.Index(out, "1/0/1") > strings.Index(out, "2/0/1") {
		t.Errorf("not sorted by port:\n%s", out)
	}
	if out := ts.ok("show lldp neighbors member 2"); strings.Contains(out, "1/0/1") {
		t.Errorf("member 2 shows 1/0/1:\n%s", out)
	}
	contains(t, ts.ok("show lldp neighbors interface 1/0/1"), "System name: server", "Management addresses: 10.0.0.5")
	contains(t, ts.ok("show lldp local-information"), "Chassis ID: 02:01:01:01:01:01", "System name: core", "time to live 120s", "2/0/1      2/0/1")
	contains(t, ts.ok("show lldp statistics"), "1/0/1               5          3")
}

func TestMemberTargets(t *testing.T) {
	e := newEngine(t)
	ops := &fakeOps{}
	st := &fakeStack{}
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.sh.env.Ops, ts.sh.env.Stack = ops, st

	out := ts.ok("show version member 2")
	contains(t, out, "member2:\n-----", "remote 2: show version\n")
	if strings.Contains(out, "member1:") {
		t.Errorf("local section for member 2:\n%s", out)
	}
	// Interface listings are one table of the whole stack (Ops returns
	// every member's rows); a target narrows the rows, nothing runs on
	// the other members.
	st.calls = nil
	out = ts.ok("show chassis hardware")
	contains(t, out, "1/1/0")
	if strings.Contains(out, "member1:") || strings.Contains(out, "remote") || len(st.calls) != 0 {
		t.Errorf("show chassis hardware in sections or run remotely (%v):\n%s", st.calls, out)
	}
	if out := ts.ok("show chassis hardware member 2"); strings.Contains(out, "1/1/0") || strings.Contains(out, "member2:") {
		t.Errorf("show chassis hardware member 2:\n%s", out)
	}
	contains(t, ts.ok("show interfaces terse local"), "Interface")
	out = ts.run(`show system uptime all-members | match "remote|member|error"`)
	contains(t, out, "member1:", "member2:", "remote 2: show system uptime", "member3:", "error: member 3 is not reachable")
	if strings.Index(out, "member1:") > strings.Index(out, "member2:") {
		t.Errorf("sections not in member order:\n%s", out)
	}
	// local and this member's id run here, without sections.
	for _, l := range []string{"show version local", "show version member 1"} {
		if out := ts.ok(l); strings.Contains(out, "member1:") || !strings.Contains(out, "test") {
			t.Errorf("%s:\n%s", l, out)
		}
	}
	contains(t, ts.run("show version member 7"), "expecting a member of this stack")
	// A VC port of another member: the request goes to that member.
	st.calls = nil
	contains(t, ts.ok("request virtual-chassis vc-port set 2/1/0"), "remote 2: request virtual-chassis vc-port set 2/1/0")
	contains(t, ts.run("request virtual-chassis vc-port set 5/1/0"), "member 5 is not in this virtual chassis")
	contains(t, ts.run("show version member"), "expecting a member id")

	// Reboot: one question for all targets, the others first, this one last.
	st.calls, ops.power = nil, nil
	ts.term.answers = []string{"yes"}
	ts.run("request system reboot all-members")
	if len(st.calls) != 2 || st.calls[0] != "2 request system reboot true" || st.calls[1] != "3 request system reboot true" {
		t.Errorf("remote reboots: %v", st.calls)
	}
	if len(ops.power) != 1 || !strings.Contains(ops.power[0], "reboot") {
		t.Errorf("local reboot: %v", ops.power)
	}
	st.calls, ops.power = nil, nil
	ts.term.answers = []string{"no"}
	ts.run("request system reboot member 2")
	if len(st.calls) != 0 {
		t.Errorf("rebooted without confirmation: %v", st.calls)
	}

	// Completion offers the targets and the member ids.
	names := func(cs []Completion) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Text)
		}
		return out
	}
	if got := names(ts.sh.Complete("show version ")); !slices.Contains(got, "all-members") || !slices.Contains(got, "member") {
		t.Errorf("show version completions: %v", got)
	}
	if got := names(ts.sh.Complete("show version member ")); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("member completions: %v", got)
	}
	if got := names(ts.sh.Complete("show interfaces 1/1/0 ")); !slices.Contains(got, "local") {
		t.Errorf("interface completions lack targets: %v", got)
	}
}

func TestShowLACP(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	out := ts.ok("show lacp interfaces")
	contains(t, out, "Aggregated interface: ae1", "Actor system: 32768,02:11:22:33:44:55, key 2",
		"      1/2/0           Actor    No    No   Yes  Yes  Yes   Yes     Fast    Active",
		"      1/3/0           Actor    No   Yes    No   No   No   Yes     Fast    Active",
		"Current   Fast periodic Collecting distributing  65535,02:00:00:00:00:22, key 9, port 1",
		"Defaulted", "Detached")
	contains(t, ts.ok("show lacp interfaces ae1"), "Aggregated interface: ae1")
	contains(t, ts.run("show lacp interfaces ae7"), "ae7: no LACP")
	contains(t, ts.run("show lacp interfaces 1/2/0"), "expecting an aggregated interface")
	contains(t, ts.ok("show lacp statistics interfaces"), "      1/2/0                30         31            0            0",
		"      1/3/0                 0          5            0            1")
}

func TestShowLimits(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	out := ts.ok("show system limits")
	contains(t, out, "Limits of member 1", "Applied (slots)        Supported",
		"  Configurable mtu:                -                      256..16000 (default 1514)",
		"  Largest mtu configured:          -                      1514 (default; hosts up to MTU 1500)",
		"  Hardware maximum of the ports:   -                      9014 (1/1/0) .. 16014 (1/3/0)",
		"  VLAN ids:                        -                      1..4094; 0 of 4094",
		"  MAC addresses learned now:       -                      12",
		"  Bundles (ae0..ae4095):           -                      0 of 4096",
		"  Members:                         -                      1 of 16",
		"  Stacking ports:                  -                      2",
		"  Fastest port:                    -                      10G (1/3/0)")
	// With memory slots: the Applied column and the tables.
	ts.sh.env.Ops = &fakeOps{memory: &MemoryStatus{Slotted: true, Update: 129, UpdateRoom: 512 << 20, NeighV4: 32768, NeighV6: 4096,
		Purposes: []MemoryPurpose{{Name: "bgp-ipv4", Capacity: 199700, AllSlots: 2000000, Used: 1000},
			{Name: "arp", Capacity: 32768, AllSlots: 8000000, Used: -1}, {Name: "mac", AllSlots: 9000000, Used: 12}}}}
	contains(t, ts.ok("show system limits"),
		"  BGP IPv4 routes:                 1000 of 199700         2000000 with every slot; 1000 now",
		"  ARP entries:                     32768                  8000000 with every slot",
		"  MAC addresses:                   -                      9000000 with every slot; 12 now",
		"  Software bundle:                 512 MB                 512 MB free in memory now")
}

func TestShowStackMTU(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	contains(t, ts.ok("show virtual-chassis mtu"),
		"Largest data mtu in the stack:  9014 (vlans storage mtu; hosts up to MTU 9000)",
		"Needed on the stacking links:   9072 (+58: tunnel 50, VLAN tags 8)",
		"The stacking ports allow data mtu up to 8992 (hosts up to MTU 8978)",
		"  1/5/0    9216    9216     9216      ok",
		"  1/5/1    1514    16058    -         too small (set to the maximum when switchd starts)",
		"  1/5/2    9000    9050     -         too small: the NIC carries at most 9050",
		"  1/5/3    9216    9216     1514      the cable carries only 1514: larger frames are lost")
}

func TestShowMCLAG(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	out := ts.ok("show mclag")
	contains(t, out, "MC-LAG pair: member 1 (primary), member 2 (secondary)",
		"Member 1: reaches its peer over the stack (stack members reached: 3 of 3)",
		"  ae1        up                   up                   1:on 2:on      -",
		"  ae2        down                 down                 1:off 2:off    1: delay-restore (4m50s left)")
	contains(t, ts.ok("show mclag consistency"), "  ae1: consistent")
}

func TestShowSnooping(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	out := ts.ok("show igmp snooping membership")
	contains(t, out, "1/0/1", "ae1", "static", "2 memberships")
	if strings.Contains(out, "vc-2") || strings.Contains(out, "ff0e::1") {
		t.Errorf("stack copies or IPv6 groups listed:\n%s", out)
	}
	contains(t, ts.ok("show mld snooping membership"), "ff0e::1", "1 memberships")
	contains(t, ts.ok("show igmp snooping vlans"), "seen on 1/0/2")
}

func TestShowBottlenecks(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.env.Ops = &fakeOps{}
	contains(t, ts.ok("show system bottlenecks"), "limit PCIe   card 1", "-> use another slot")
	su := newTester(t, e, "root", commit.SuperUser)
	su.sh.env.Ops = &fakeOps{}
	contains(t, su.ok("request system diagnose"), "card 1") // request: super-user (4.3)
}

func TestShowVXLAN(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.ok("configure")
	for _, l := range []string{"set vlans users vlan-id 10", "set vlans users vxlan vni 10010", "set switch-options vxlan source-address 10.255.0.1",
		"set switch-options vxlan remote-vtep 10.200.1.10 vni 10010", "commit", "exit"} {
		ts.ok(l)
	}
	ts.sh.env.Ops = &fakeOps{}
	contains(t, ts.ok("show vxlan"), "Stack VTEP 10.255.0.1", "10010     users          swvx10010    1       up", "10.200.1.10 (3)")
	contains(t, ts.ok("show vxlan remote-vtep"), "10.200.1.10      1       10.99.0.0, 1/5/0", "10.200.1.10      2       no route")
}

func TestShowRouteInstance(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.ok("configure")
	for _, l := range []string{"set interfaces 1/0/5 unit 0 family inet address 10.7.0.1/24", "set routing-instances data interface 1/0/5.0",
		"commit", "exit"} {
		ts.ok(l)
	}
	ts.sh.env.Ops = &fakeOps{}
	contains(t, ts.ok("show route instance"), "default ", "data             virtual-router", "1/0/5.0")
}

func TestPlainErrors(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.ReadOnly)
	ts.sh.SetPlainErrors()
	out := ts.run("show route nonsense")
	if strings.Contains(out, "^") || !strings.Contains(out, "error: syntax error") {
		t.Errorf("plain error: %q", out)
	}
}

func TestSTPWhyNotAPort(t *testing.T) {
	e := newEngine(t)
	ts := newTester(t, e, "alice", commit.SuperUser)
	ts.ok("configure")
	for _, l := range []string{"set interfaces 1/0/7", "set interfaces 1/0/8 ether-options 802.3ad ae1", "set interfaces ae1",
		"set protocols rstp", "commit", "exit"} {
		ts.ok(l)
	}
	ts.sh.env.Ops = &fakeOps{}
	contains(t, ts.run("show spanning-tree interface 1/0/7"), "not a switch port")
	contains(t, ts.run("show spanning-tree interface 1/0/8"), "member of ae1; RSTP runs on ae1")
}
