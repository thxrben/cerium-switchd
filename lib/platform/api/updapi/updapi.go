// Package updapi is the API of switchd-update: what switchd and the other programs
// use of it (configuration, requests, states, method and topic names).
package updapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/thxrben/cerium-switchd/lib/software/software"
)

const (
	KeysDir = "/usr/share/ceros/keys"
)

// DefaultSocket is where the daemon listens.
const DefaultSocket = "/run/switchd-update/sock"

// ErrRejected starts the error of a check whose configuration the new
// version rejects.
var ErrRejected = errors.New("the new version rejects the active configuration")

// Request is what switchd sends.
type Request struct {
	// install, rollback, check, healthy, started, maintenance-done, status
	Op string `json:"op"`
	// install: the bundle, received and verified by switchd (verified
	// again here).
	Bundle string `json:"bundle,omitempty"`
	// healthy, started: the version switchd runs.
	Version string `json:"version,omitempty"`
	// ExitMaintenance: the update drained the member; switchd leaves
	// maintenance mode once the new version is healthy.
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
	// Standalone: the switch is not a member of a virtual chassis (its
	// configuration backup may be put back after a rollback).
	Standalone bool `json:"standalone,omitempty"`
	// NoValidate: a configuration the new version rejects is only a
	// warning.
	NoValidate bool `json:"no_validate,omitempty"`
}

// Reply is the daemon's answer.
type Reply struct {
	Err     string              `json:"err,omitempty"`
	State   string              `json:"state,omitempty"`
	Version string              `json:"version,omitempty"` // install/rollback: the version booted next
	Note    string              `json:"note,omitempty"`    // the result of the last update
	Text    string              `json:"text,omitempty"`    // install: what the checks said
	Slots   []software.SlotInfo `json:"slots,omitempty"`
	// BootState: a problem with the boot state (missing, damaged,
	// unreadable); "" when it reads fine.
	BootState string `json:"boot_state,omitempty"`
	Active    string `json:"active,omitempty"`
	Update    *State `json:"update,omitempty"`
}

// State is the update in progress, kept on the configuration partition
// (/config/update/state.json).
type State struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	FromSlot string    `json:"from_slot"`
	Slot     string    `json:"slot"` // the slot written
	Since    time.Time `json:"since"`
	// Revision: the newest configuration revision when the update started.
	Revision   string `json:"revision,omitempty"`
	Standalone bool   `json:"standalone,omitempty"`
	// ExitMaintenance: see Request.
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
	// Booted: the new slot started (its daemon saw the update).
	Booted bool `json:"booted,omitempty"`
	// MaintenanceDone: switchd left the maintenance mode the update
	// entered (ExitMaintenance is then not acted upon again).
	MaintenanceDone bool `json:"maintenance_done,omitempty"`
	// Starts counts the new switchd's starts.
	Starts int `json:"starts,omitempty"`
	// Done: "" in progress, "ok", or "rolled back: <reason>".
	Done string `json:"done,omitempty"`
	// Recorded: the old slot recorded the rollback (it is final).
	Recorded bool `json:"recorded,omitempty"`
	// Rollback: a rollback (no image written).
	Rollback bool `json:"rollback,omitempty"`
}

// Call sends one request to the daemon at socket.
func Call(socket string, r Request) (Reply, error) {
	return CallTimeout(socket, r, 20*time.Second)
}

// CallTimeout is Call with a deadline (install writes a slot).
func CallTimeout(socket string, r Request, timeout time.Duration) (Reply, error) {
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return Reply{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	raw, _ := json.Marshal(r)
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return Reply{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return Reply{}, err
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return Reply{}, err
	}
	if rep.Err != "" {
		return rep, errors.New(rep.Err)
	}
	return rep, nil
}
