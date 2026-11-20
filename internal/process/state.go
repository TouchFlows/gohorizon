package process

import (
	"fmt"
	"time"
)

// State is the supervisor's view of one worker process (handoff §7.3).
type State string

const (
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateCrashed    State = "crashed"
	StateRestarting State = "restarting"
	StateUnhealthy  State = "unhealthy"
)

func (s State) Valid() bool {
	switch s {
	case StateStarting, StateRunning, StateStopping, StateStopped, StateCrashed, StateRestarting, StateUnhealthy:
		return true
	}
	return false
}

// Alive reports whether a worker in this state is expected to have a live process.
func (s State) Alive() bool {
	return s == StateStarting || s == StateRunning || s == StateStopping || s == StateUnhealthy
}

// allowedTransitions is the complete state machine. A transition outside this table
// is a programming error in the supervisor, not an operator problem, so it is
// rejected instead of logged and ignored.
var allowedTransitions = map[State]map[State]bool{
	StateStarting: {
		StateRunning:   true,
		StateUnhealthy: true,
		StateCrashed:   true,
		StateStopped:   true,
	},
	StateRunning: {
		StateStopping: true,
		// A worker can also exit on its own: --max-jobs, --max-time, memory limit,
		// queue:restart or queue empty. That is a recycle, not a crash.
		StateStopped:   true,
		StateCrashed:   true,
		StateUnhealthy: true,
	},
	StateStopping: {
		StateStopped: true,
		StateCrashed: true,
	},
	StateStopped: {
		StateStarting: true, // replacement process for the same slot
	},
	StateCrashed: {
		StateRestarting: true,
		StateStarting:   true,
		StateStopped:    true,
	},
	StateRestarting: {
		StateStarting: true,
		StateStopped:  true,
	},
	StateUnhealthy: {
		StateRunning:  true,
		StateStopping: true,
		StateCrashed:  true,
		StateStopped:  true,
	},
}

func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	return allowedTransitions[from][to]
}

// Transition records an explicit state change with its reason and timestamp.
type Transition struct {
	From   State     `json:"from"`
	To     State     `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// ErrIllegalTransition is returned instead of silently accepting an impossible state
// change, so a supervisor bug surfaces in tests rather than in the dashboard.
type ErrIllegalTransition struct {
	From, To State
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("illegal worker state transition %s -> %s", e.From, e.To)
}
