// Package redis owns every key GoHorizon writes.
//
// Key ownership (docs/adr/0003-redis-namespace.md):
//
//	gohorizon:{application}:{environment}:events              telemetry stream
//	gohorizon:{application}:{environment}:coord:lock:supervisor ownership lock
//	gohorizon:{application}:{environment}:coord:supervisor:{id} supervisor control
//	gohorizon:{application}:{environment}:metrics:*            aggregates (later)
//
// The Laravel application owns "queues:*" and "illuminate:*" with its own prefix.
// This package has no scan, no raw client access and no key builder outside these
// segments, so a GoHorizon command can never reach an application key by accident.
package redis

import (
	"fmt"
	"regexp"
)

// segmentRE keeps a name from containing ':', whitespace or a cluster hash tag, so a
// key can never escape its namespace or accidentally co-locate in a hash slot.
var segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Keys is the key builder for one application/environment.
type Keys struct {
	application string
	environment string
}

// NewKeys validates the namespace segments and returns a builder.
func NewKeys(application, environment string) (Keys, error) {
	if !segmentRE.MatchString(application) {
		return Keys{}, fmt.Errorf("application id %q must match %s", application, segmentRE)
	}
	if !segmentRE.MatchString(environment) {
		return Keys{}, fmt.Errorf("environment %q must match %s", environment, segmentRE)
	}
	return Keys{application: application, environment: environment}, nil
}

func (k Keys) Application() string { return k.application }
func (k Keys) Environment() string { return k.environment }

// Scope is the common prefix, ending with ':'.
func (k Keys) Scope() string {
	return "gohorizon:" + k.application + ":" + k.environment + ":"
}

// Events is the telemetry stream (handoff §6.3). The Laravel package builds the same
// string in src/Redis/Keys.php and both sides have a test asserting it.
func (k Keys) Events() string { return k.Scope() + "events" }

// Coordinates returns the coordination namespace prefix, ending with ':'.
func (k Keys) Coordinates() string { return k.Scope() + "coord:" }

// SupervisorLock is the single-instance ownership lock (ADR 0004).
func (k Keys) SupervisorLock() string { return k.Coordinates() + "lock:supervisor" }

// SupervisorControl is the per-supervisor control key (pause/resume, desired state).
func (k Keys) SupervisorControl(supervisorID string) (string, error) {
	if !segmentRE.MatchString(supervisorID) {
		return "", fmt.Errorf("supervisor id %q must match %s", supervisorID, segmentRE)
	}
	return k.Coordinates() + "supervisor:" + supervisorID, nil
}
