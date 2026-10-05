// Command switchd is the switch daemon (the CLI client is the program
// swcli, the update daemon switchd-update).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/thxrben/cerium-switchd/internal/daemon"
	"github.com/thxrben/cerium-switchd/internal/rpc"
	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/internal/version"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
	"github.com/thxrben/cerium-switchd/pkg/journal"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "check-config":
			os.Exit(checkConfig(os.Args[2:]))
		case "bundle":
			os.Exit(makeBundle(os.Args[2:]))
		case "keygen":
			os.Exit(keygen(os.Args[2:]))
		case "verify-bundle":
			os.Exit(verifyBundle(os.Args[2:]))
		}
	}

	fs := flag.NewFlagSet("switchd", flag.ExitOnError)
	stateDir := fs.String("state-dir", "/var/lib/switchd", "directory for persistent state")
	socket := fs.String("socket", rpc.DefaultSocket, "CLI socket")
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
	// The journal (with facility and severity for cer-syslogd, reference
	// 1.9); stderr when there is none.
	log := slog.New(journal.NewHandler(journal.Options{Identifier: "switchd", Level: level}))
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
	raw, err := hwio.ReadFile(args[0])
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

// makeBundle is "switchd bundle": signs a slot image into a bundle
// (docs/os-image.md §4.1). The verity values come from the image build.
func makeBundle(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	out := fs.String("o", "", "output bundle")
	img := fs.String("image", "", "slot image (squashfs + verity hash tree)")
	keyFile := fs.String("key", "", "signing key file (ceros-ed25519-private …)")
	m := software.BundleManifest{Arch: "amd64", Platform: "x86_64-efi", Compatible: "ceros-x86_64"}
	fs.StringVar(&m.Version, "version", version.Version, "version")
	fs.StringVar(&m.Built, "built", version.Date, "build time (RFC 3339)")
	fs.StringVar(&m.Arch, "arch", m.Arch, "architecture")
	fs.StringVar(&m.Platform, "platform", m.Platform, "platform")
	fs.StringVar(&m.Compatible, "compatible", m.Compatible, "compatible string")
	fs.StringVar(&m.RootHash, "roothash", "", "dm-verity root hash")
	fs.Int64Var(&m.HashOffset, "hash-offset", 0, "offset of the verity hash tree")
	fs.StringVar(&m.MinFrom, "min-from", "", "oldest version it updates from")
	fs.Parse(args)
	if *out == "" || *img == "" || *keyFile == "" {
		fmt.Fprintln(os.Stderr, "usage: switchd bundle -o <file> -image <rootfs.img> -key <key> -roothash <hex> -hash-offset <n> [-version <v>]")
		return 2
	}
	raw, err := hwio.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	key, err := software.ParsePrivateKey(string(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	f, err := hwio.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := software.WriteBundle(f, m, *img, key); err != nil {
		f.Close()
		hwio.Remove(*out)
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// keygen is "switchd keygen <name>": a new bundle signing key, written to
// <name>.key (keep it secret) and <name>.pub (goes into the image).
func keygen(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: switchd keygen <name>")
		return 2
	}
	priv, pub, err := software.GenerateKey(filepath.Base(args[0]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := hwio.Stat(args[0] + ".key"); err == nil {
		fmt.Fprintln(os.Stderr, args[0]+".key exists")
		return 1
	}
	if err := hwio.WriteFile(args[0]+".key", []byte(priv), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := hwio.WriteFile(args[0]+".pub", []byte(pub), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// verifyBundle is "switchd verify-bundle <bundle> <keys-dir>".
func verifyBundle(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: switchd verify-bundle <bundle> <keys-dir>")
		return 2
	}
	keys, err := software.LoadKeys(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	m, err := software.VerifyBundleFile(args[0], keys)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s %s (%s), %d bytes, root hash %s: signature and image verified\n", m.Version, m.Arch, m.Built, m.ImageSize, m.RootHash)
	return 0
}

// updateDaemon is "switchd update-daemon", the update daemon of earlier
// versions' units (now its own program, cmd/switchd-update).
