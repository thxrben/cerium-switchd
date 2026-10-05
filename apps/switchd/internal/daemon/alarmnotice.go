package daemon

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/thxrben/cerium-switchd/lib/platform/alarms"
)

// memberNotice makes a notice name the member it comes from (reference
// 3.5: the sessions of the whole stack see it).
func memberNotice(member int, text string) string {
	prefix := fmt.Sprintf("member %d: ", member)
	if strings.HasPrefix(text, prefix) {
		return text
	}
	return prefix + text
}

// announceAlarms logs every new, changed and cleared alarm of this member
// and announces it to the stack's CLI sessions, always with the member.
// The notices go out in order from a goroutine of their own: relaying one
// to the master may take seconds, and a raiser (a device watcher, the
// supervisor) must not wait for it.
func announceAlarms(al *alarms.Set, member int, log *slog.Logger, notify func(string)) {
	queue := make(chan string, 64)
	go func() {
		for text := range queue {
			notify(text)
		}
	}()
	send := func(text string) {
		select {
		case queue <- text:
		default: // a burst beyond the queue: the log has it
		}
	}
	al.OnChange(func(a alarms.Alarm, raised bool) {
		if raised {
			log.Error("ALARM: "+a.Text, "member", member, "class", a.Class, "alarm", a.ID)
		} else {
			log.Warn("alarm cleared: "+a.Text, "member", member, "class", a.Class, "alarm", a.ID)
		}
		send(alarmNotice(member, a, raised))
	})
}

func alarmNotice(member int, a alarms.Alarm, raised bool) string {
	if raised {
		return fmt.Sprintf("member %d: ALARM (%s): %s", member, a.Class, a.Text)
	}
	return fmt.Sprintf("member %d: alarm cleared (%s): %s", member, a.Class, a.Text)
}
