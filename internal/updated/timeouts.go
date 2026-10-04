package updated

import (
	"sync/atomic"
	"time"

	"github.com/thxrben/cerium-switchd/internal/config"
	"github.com/thxrben/cerium-switchd/internal/model"
	"github.com/thxrben/cerium-switchd/internal/software"
	"github.com/thxrben/cerium-switchd/pkg/hwio"
)

// The update daemon's timeouts come from system timeouts of the active
// configuration (reference 5.1): read when it starts and with every
// request, so a value raised for an update holds for the whole update,
// the health check after the reboot included.
var (
	installTimeout atomic.Int64
	checkTimeout   atomic.Int64
	healthTimeout  atomic.Int64
)

func init() { ApplyTimeouts(model.DefaultTimeouts) }

// ApplyTimeouts sets the timeouts of this process.
func ApplyTimeouts(t model.Timeouts) {
	hwio.SetDeadlines(t.DiskOperation, t.KernelCall)
	software.SetSlotIODeadline(t.SlotWrite)
	installTimeout.Store(int64(t.SoftwareInstall))
	checkTimeout.Store(int64(t.ConfigCheck))
	healthTimeout.Store(int64(t.HealthCheck))
}

// TimeoutsOf reads the timeouts of a configuration (JSON as stored); the
// defaults when it cannot be read.
func TimeoutsOf(raw []byte) model.Timeouts {
	t, err := config.FromJSON(raw)
	if err != nil {
		return model.DefaultTimeouts
	}
	cfg, _ := model.Build(t, nil) // only system timeouts are used
	if cfg == nil {
		return model.DefaultTimeouts
	}
	return cfg.System.Timeouts
}

// loadTimeouts applies the active configuration's timeouts.
func loadTimeouts(active func() ([]byte, error)) {
	if active == nil {
		return
	}
	if raw, err := active(); err == nil {
		ApplyTimeouts(TimeoutsOf(raw))
	}
}

func dur(v *atomic.Int64) time.Duration { return time.Duration(v.Load()) }
