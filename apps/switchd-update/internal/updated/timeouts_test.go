package updated

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thxrben/cerium-switchd/lib/conf/config"
	"github.com/thxrben/cerium-switchd/lib/conf/model"
	"github.com/thxrben/cerium-switchd/lib/software/software"
	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
)

// The update daemon takes system timeouts from the stored configuration;
// an unreadable one gives the defaults.
func TestTimeoutsFromConfig(t *testing.T) {
	tr, err := config.ParseSet("set system timeouts slot-write 300\nset system timeouts health-check 1200\nset system timeouts disk-operation 45\n")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(config.ToJSON(tr.Root))
	to := TimeoutsOf(raw)
	if to.SlotWrite != 5*time.Minute || to.HealthCheck != 20*time.Minute || to.SoftwareInstall != model.DefaultTimeouts.SoftwareInstall {
		t.Fatalf("timeouts %+v", to)
	}
	defer ApplyTimeouts(model.DefaultTimeouts)
	loadTimeouts(func() ([]byte, error) { return raw, nil })
	d := &Daemon{}
	if d.timeout() != 20*time.Minute || software.SlotIODeadline() != 5*time.Minute || hwio.FileDeadline() != 45*time.Second {
		t.Fatalf("applied: health %v slot %v disk %v", d.timeout(), software.SlotIODeadline(), hwio.FileDeadline())
	}
	if (&Daemon{Timeout: time.Minute}).timeout() != time.Minute {
		t.Fatal("the kernel command line's health timeout must win")
	}
	if TimeoutsOf([]byte("not json")) != model.DefaultTimeouts {
		t.Fatal("unreadable configuration: defaults")
	}
}
