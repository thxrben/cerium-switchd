package swcli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"golang.org/x/term"
)

// childEnv marks the CLI process started by the supervisor.
const childEnv = "SWCLI_CHILD"

// supervise runs the interactive CLI as a child process. If the child
// fails (a crash, not a normal exit), the terminal is restored and the
// user gets a Linux shell if allowed (root, or a session switchd reported
// as super-user); afterwards the CLI starts again. Others get the CLI
// restarted, up to three times per minute.
func supervise(args []string) int {
	self, err := os.Executable()
	if err != nil {
		os.Setenv(childEnv, "1")
		return Main(args)
	}
	fd := int(os.Stdin.Fd())
	saved, _ := term.GetState(fd)

	// Terminal signals reach the child; the supervisor only survives them.
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sig)
	var mu sync.Mutex
	hangup := false
	go func() {
		for s := range sig {
			if s == syscall.SIGHUP || s == syscall.SIGTERM {
				mu.Lock()
				hangup = true
				mu.Unlock()
			}
		}
	}()

	class := ""
	var crashes []time.Time
	for {
		r, w, err := os.Pipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "swcli: %v\n", err)
			return 1
		}
		cmd := exec.Command(self)
		cmd.Args = append([]string{"swcli-session"}, args...)
		errR, errW, err := os.Pipe()
		if err != nil {
			r.Close()
			w.Close()
			fmt.Fprintf(os.Stderr, "swcli: %v\n", err)
			return 1
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, errW
		cmd.Env = append(os.Environ(), childEnv+"=1")
		cmd.ExtraFiles = []*os.File{w}
		err = cmd.Start()
		w.Close()
		errW.Close()
		if err != nil {
			r.Close()
			errR.Close()
			fmt.Fprintf(os.Stderr, "swcli: %v\n", err)
			return 1
		}
		reports := make(chan []byte, 1)
		go func() { reports <- filterCrash(errR, os.Stderr); errR.Close() }()
		classes := make(chan string, 1)
		go func() {
			last := ""
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				last = sc.Text()
			}
			r.Close()
			classes <- last
		}()
		err = cmd.Wait()
		report := <-reports
		if c := <-classes; c != "" {
			class = c
		}
		mu.Lock()
		hup := hangup
		mu.Unlock()
		code, crashed := exitStatus(err)
		if !crashed || hup {
			return code
		}

		if saved != nil {
			_ = term.Restore(fd, saved)
		}
		os.Stdout.WriteString("\x1b[?2004l\r\n")
		fmt.Fprintf(os.Stderr, "*** The CLI stopped unexpectedly (%v) ***\n", describe(err))
		if len(report) > 0 {
			if f, ferr := hwio.CreateTemp("", "swcli-crash-*.txt"); ferr == nil {
				_, _ = f.Write(report)
				f.Close()
				fmt.Fprintf(os.Stderr, "The crash report is in %s; please pass it on to the developers.\n", f.Name())
			}
		}
		now := time.Now()
		crashes = append(crashes, now)
		for len(crashes) > 0 && now.Sub(crashes[0]) > time.Minute {
			crashes = crashes[1:]
		}
		if os.Getuid() == 0 || class == "super-user" {
			fmt.Fprintln(os.Stderr, "Starting a Linux shell; 'exit' returns to the CLI.")
			saneTerminal(fd)
			runBash()
			reclaimTerminal(fd)
			continue
		}
		if len(crashes) >= 3 {
			fmt.Fprintln(os.Stderr, "The CLI keeps failing; logging out.")
			return 1
		}
		fmt.Fprintln(os.Stderr, "Restarting the CLI.")
		time.Sleep(time.Second)
	}
}

// exitStatus classifies how the CLI process ended. A Go runtime failure
// (exit status 2) or a fatal signal is a crash; a hangup is not.
func exitStatus(err error) (code int, crashed bool) {
	if err == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1, true
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return 1, true
	}
	if ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGHUP, syscall.SIGTERM, syscall.SIGPIPE:
			return 128 + int(ws.Signal()), false
		}
		return 128 + int(ws.Signal()), true
	}
	return ws.ExitStatus(), ws.ExitStatus() == 2
}

// crashStarts are the first lines of a Go runtime crash report.
var crashStarts = []string{"panic: ", "fatal error: ", "unexpected fault address", "SIGSEGV", "SIGBUS",
	"SIGILL", "SIGFPE", "SIGABRT", "SIGQUIT", "SIGTRAP", "SIGSYS"}

// filterCrash copies the CLI's error output to w until a crash report
// starts; the report is returned instead of shown.
func filterCrash(r io.Reader, w io.Writer) []byte {
	br := bufio.NewReader(r)
	var report bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if report.Len() == 0 {
			for _, p := range crashStarts {
				if strings.HasPrefix(line, p) {
					report.WriteString(line)
					line = ""
					break
				}
			}
			_, _ = io.WriteString(w, line)
		} else {
			report.WriteString(line)
		}
		if err != nil {
			return report.Bytes()
		}
	}
}

func describe(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "signal " + ws.Signal().String()
		}
		return "internal error"
	}
	return err.Error()
}
