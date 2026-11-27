// Package audit records who did what, as one line per action, under a message an operator can
// select on: `{"msg":"audit", "action":"sign-in", "outcome":"allowed", ...}`.
//
// Milestone 3 starts the record with the session events, because they are the ones an operator
// asks about after a lockout or a suspected leak, and because they were the ones with an actor
// already. Restarting a worker is the first action that changes the pool, and it records both
// that it happened and that it was refused — by role or by a missing token — so the line says
// who tried, not only who succeeded. Further administrative actions inherit the same shape and
// the same logger; a queryable audit history is a store away from that, and is deliberately
// not promised here. A state change the service notices on its own — the deployment becoming
// read-only, or a package upgrade under a readable one — is recorded the same way with no user
// and no request: the line says what changed and why, which is what an operator asks later.
//
// A record is written to the service log rather than to a database: it is one line, it is
// append-only, and the log already has rotation, shipping and retention an operator chose.
// Writing it cannot fail the request it describes — a lost audit line must not turn a
// successful sign-in into an error the operator has to work around.
package audit

import (
	"log/slog"
	"net/http"
)

// Actions a record can carry. They are dotted names with a stable spelling, because an
// operator's alerting matches on them.
const (
	ActionSignIn          = "sign-in"
	ActionSignOut         = "sign-out"
	ActionWorkerRestart   = "worker-restart"
	ActionFailedJobRetry  = "failed-job-retry"
	ActionFailedJobDelete = "failed-job-delete"
	// ActionCapabilityChange is the deployment changing what it says about itself: becoming
	// read-only, coming back, or a package upgrade under a readable descriptor. It has no
	// user, because the service noticed rather than an operator doing anything (docs/adr/0014).
	ActionCapabilityChange = "capability-change"
)

// Outcomes a record can carry: the action happened, it was refused, the caller was held back by
// the limiter before anything was checked, or the service observed a state change.
const (
	OutcomeAllowed = "allowed"
	OutcomeRefused = "refused"
	OutcomeLimited = "limited"
	// OutcomeChanged says what the line describes is a state that changed, not an action that was
	// taken or refused.
	OutcomeChanged = "changed"
)

// Event is one auditable action. User and Role are the actor when there is one; a refused
// sign-in has only the name that was claimed, so Detail says what was wrong and Role stays
// empty rather than guessing.
type Event struct {
	Action  string
	Outcome string
	User    string
	Role    string
	Address string
	Detail  string
}

// Logger writes events through the service log. It holds nothing else: no queue, no buffer,
// no ordering promise beyond the log's own.
type Logger struct {
	log *slog.Logger
}

// New returns a logger writing to log. A nil logger is usable and drops the records, which is
// what a test or a command without a log gets instead of a panic on the sign-in path.
func New(log *slog.Logger) *Logger {
	return &Logger{log: log}
}

// Record writes one line. The request contributes its method and path so a record can be tied
// to the call that produced it without duplicating what the access log already says.
func (l *Logger) Record(r *http.Request, event Event) {
	if l == nil || l.log == nil {
		return
	}
	attributes := []any{
		"action", event.Action,
		"outcome", event.Outcome,
		"address", event.Address,
	}
	// The actor is only written when there is one: an empty "user" field on a refused sign-in
	// would read as an operator with a blank name.
	if event.User != "" {
		attributes = append(attributes, "user", event.User)
	}
	if event.Role != "" {
		attributes = append(attributes, "role", event.Role)
	}
	if event.Detail != "" {
		attributes = append(attributes, "detail", event.Detail)
	}
	if r != nil {
		attributes = append(attributes, "method", r.Method, "path", r.URL.Path)
	}
	l.log.Info("audit", attributes...)
}
