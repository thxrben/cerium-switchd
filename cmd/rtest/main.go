// Command rtest runs switchd's routing protocols on plain Linux interfaces
// for the interoperability tests (test/interop): no data plane, no stack.
// It reads a small JSON description, runs the protocols, installs routes
// into the kernel of its network namespace and serves a status file.
//
//	rtest -c config.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/bfd"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// Config describes what rtest runs.
type Config struct {
	BFD []struct {
		Peer     string `json:"peer"`
		Local    string `json:"local"`
		Interval int    `json:"interval_ms"`
		Mult     int    `json:"multiplier"`
	} `json:"bfd"`
}

func main() {
	file := flag.String("c", "rtest.json", "configuration")
	status := flag.String("status", "/tmp/rtest-status.json", "status file, rewritten every second")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	raw, err := hwio.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	udp := &bfd.UDP{}
	sv := bfd.NewServer(udp, log)
	udp.Input = sv.Input
	go sv.Run()
	for _, b := range cfg.BFD {
		k := bfd.Key{Peer: netip.MustParseAddr(b.Peer)}
		if b.Local != "" {
			k.Local = netip.MustParseAddr(b.Local)
		}
		iv := time.Duration(b.Interval) * time.Millisecond
		if err := sv.Add(k, "", bfd.Client{Name: "test"}, bfd.Config{MinTx: iv, MinRx: iv, Multiplier: uint8(b.Mult)}); err != nil {
			log.Error("bfd", "err", err)
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(time.Second)
	for {
		select {
		case <-sig:
			sv.Stop()
			return
		case <-tick.C:
			st := map[string]any{"bfd": sv.Sessions()}
			b, _ := json.MarshalIndent(st, "", " ")
			hwio.WriteFile(*status+".tmp", b, 0o644)
			hwio.Rename(*status+".tmp", *status)
		}
	}
}
