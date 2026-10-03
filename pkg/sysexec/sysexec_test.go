package sysexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

func TestRun(t *testing.T) {
	out, err := Command("/bin/sh", "-c", "cat; echo err >&2").WithStdin(strings.NewReader("in")).CombinedOutput(context.Background())
	if err != nil || string(out) != "inerr\n" {
		t.Fatalf("%q %v", out, err)
	}
	_, err = Command("/bin/sh", "-c", "echo bad >&2; exit 2").Output(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("failure: %v", err)
	}
}

// A tool that hangs, and a helper it started, are killed at the deadline.
func TestTimeout(t *testing.T) {
	start := time.Now()
	_, err := Command("/bin/sh", "-c", "sleep 30 & sleep 30").WithTimeout(100 * time.Millisecond).Output(context.Background())
	if !errors.Is(err, hwio.ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}
