package cli

import (
	"testing"

	"github.com/thxrben/cerium-switchd/internal/commit"
	"github.com/thxrben/cerium-switchd/internal/inventory"
)

type envOps struct {
	fakeOps
	ss []EnvSensor
}

func (f *envOps) Environment() ([]EnvSensor, error) { return f.ss, nil }

func TestShowEnvironment(t *testing.T) {
	ts := newTester(t, newEngine(t), "alice", commit.SuperUser)
	ops := &envOps{}
	ts.sh.env.Ops = ops
	contains(t, ts.ok("show chassis environment"), "No sensors")
	ops.ss = []EnvSensor{
		{Member: 2, Sensor: inventory.Sensor{Class: "Fans", Chip: "nct6775", Label: "fan2", Value: 0, Min: 500, Status: "Critical"}},
		{Member: 1, Sensor: inventory.Sensor{Class: "Temp", Chip: "coretemp", Label: "Package id 0", Value: 45, Status: "OK"}},
	}
	out := ts.ok("show chassis environment")
	contains(t, out, "Class    Item", "Temp     Member 1 coretemp/Package id 0", "OK        45 degrees C / 113 degrees F",
		"Fans     Member 2 nct6775/fan2", "Critical  Not spinning")
}
