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

	fs := flag.NewFlagSet("switchd", flag.ExitOnError)
	stateDir := fs.String("state-dir", "/var/lib/switchd", "directory for persistent state")
	socket := fs.String("socket", swcli.DefaultSocket, "CLI socket")
	dryRun := fs.Bool("dry-run", false, "do not change the kernel; log what would be applied")
	debug := fs.Bool("debug", false, "debug logging")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version.Version)
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
