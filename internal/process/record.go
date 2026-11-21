package process

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Record is the durable identity of one worker slot. It survives a GoHorizon restart
// so the supervisor can recognise (and never duplicate) the process it already owns.
type Record struct {
	ApplicationID string `json:"application_id"`
	Environment   string `json:"environment"`
	SupervisorID  string `json:"supervisor_id"`

	UUID  string `json:"uuid"`
	Slot  int    `json:"slot"`
	Queue string `json:"queue"`
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	// StartTicks is the kernel start time of the pid, used instead of the pid alone
	// because Linux reuses pids (handoff §7.2).
	StartTicks uint64 `json:"start_ticks"`

	Adopted bool  `json:"adopted"`
	State   State `json:"state"`

	StartedAt    time.Time  `json:"started_at"`
	StoppedAt    *time.Time `json:"stopped_at,omitempty"`
	RestartCount int        `json:"restart_count"`

	// CrashLooping marks a worker whose recent exits were all premature. It is derived
	// for the snapshot and never persisted: the streak it describes is RestartCount.
	CrashLooping bool `json:"crash_looping,omitempty"`

	LastExitCode   int    `json:"last_exit_code"`
	LastExitReason string `json:"last_exit_reason"`

	Transitions []Transition `json:"transitions,omitempty"`
}

const maxTransitions = 40

// Transition applies the state change if it is legal and records why it happened.
func (r *Record) Transition(to State, reason string, at time.Time) error {
	if r.State == to {
		return nil
	}
	if !CanTransition(r.State, to) {
		return &ErrIllegalTransition{From: r.State, To: to}
	}
	r.Transitions = append(r.Transitions, Transition{From: r.State, To: to, Reason: reason, At: at})
	if len(r.Transitions) > maxTransitions {
		r.Transitions = r.Transitions[len(r.Transitions)-maxTransitions:]
	}
	r.State = to
	if to == StateStopped {
		stopped := at
		r.StoppedAt = &stopped
	}
	return nil
}

func newUUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; a timestamp keeps the identity unique
		// enough to avoid adopting the wrong process.
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func shortID(uuid string) string {
	if len(uuid) >= 8 {
		return uuid[:8]
	}
	return uuid
}
