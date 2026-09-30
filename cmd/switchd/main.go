// Command switchd is the switch daemon. The same binary is the CLI client:
// started as "swcli" or "cli" (e.g. through a symlink, or as login shell "-swcli")
// or as "switchd cli", it runs the client instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"mclag/internal/daemon"
	"mclag/internal/software"
	"mclag/internal/swcli"
	"mclag/internal/version"
)

func main() {
	name := strings.TrimPrefix(filepath.Base(os.Args[0]), "-")
	if name == "swcli" || name == "cli" || name == "swcli-session" {
		os.Exit(swcli.Main(os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		os.Exit(swcli.Main(os.Args[2:]))
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "check-config":
			os.Exit(checkConfig(os.Args[2:]))
		case "package":
			os.Exit(makePackage(os.Args[2:]))
		}
	}

	fs := flag.NewFlagSet("switchd", flag.ExitOnError)
	stateDir := fs.String("state-dir", "/var/lib/switchd", "directory for persistent state")
	socket := fs.String("socket", swcli.DefaultSocket, "CLI socket")
	dryRun := fs.Bool("dry-run", false, "do not change the kernel; log what would be applied")
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := daemon.Run(ctx, daemon.Options{StateDir: *stateDir, Socket: *socket, DryRun: *dryRun, Log: log}); err != nil {
		log.Error("switchd", "err", err)
		os.Exit(1)
	}
}

// checkConfig is "switchd check-config <file>": does this version accept a
// stored configuration (JSON)? Exit status 0: yes, 1: errors.
func checkConfig(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: switchd check-config <configuration.json>")
		return 2
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	issues, ok := daemon.CheckConfig(raw)
	fmt.Print(issues)
	if !ok {
		return 1
	}
	return 0
}

// makePackage is "switchd package -o <file> -version <v> [-built <time>]
// <arch>=<program> …": builds a software package (reference 3.6).
func makePackage(args []string) int {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	out := fs.String("o", "", "package file")
	ver := fs.String("version", version.Version, "version")
	built := fs.String("built", version.Date, "build time (RFC 3339)")
	fs.Parse(args)
	progs := map[string][]byte{}
	for _, a := range fs.Args() {
		arch, path, ok := strings.Cut(a, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "expecting <arch>=<program>:", a)
			return 2
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		progs[arch] = b
	}
	if *out == "" || len(progs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: switchd package -o <file> -version <v> <arch>=<program> …")
		return 2
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := software.Write(f, *ver, *built, progs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
