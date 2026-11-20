package process

import (
	"strings"
	"testing"
	"time"
)

func TestStateMachineAcceptsDocumentedLifecycle(t *testing.T) {
	rec := Record{State: StateStopped}
	steps := []struct {
		to     State
		reason string
	}{
		{StateStarting, "spawning"},
		{StateRunning, "process started"},
		{StateStopping, "stop requested"},
		{StateStopped, "stopped: exited"},
		{StateStarting, "spawning replacement"},
		{StateRunning, "process started"},
		{StateCrashed, "worker exited with status 1"},
		{StateRestarting, "restarting after crash"},
		{StateStarting, "spawning replacement"},
		{StateRunning, "process started"},
	}

	for _, step := range steps {
		if err := rec.Transition(step.to, step.reason, time.Unix(0, 0)); err != nil {
			t.Fatalf("transition to %s: %v", step.to, err)
		}
	}
	if rec.State != StateRunning {
		t.Fatalf("final state = %s, want running", rec.State)
	}
	if len(rec.Transitions) != len(steps) {
		t.Fatalf("recorded %d transitions, want %d", len(rec.Transitions), len(steps))
	}
}

func TestStateMachineRejectsImpossibleTransitions(t *testing.T) {
	cases := []struct{ from, to State }{
		{StateRunning, StateStarting},
		{StateStopping, StateRunning},
		{StateStopped, StateRunning},
		{StateCrashed, StateRunning},
		{StateStarting, StateStopping},
	}
	for _, tc := range cases {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("%s -> %s should be illegal", tc.from, tc.to)
		}
		rec := Record{State: tc.from}
		err := rec.Transition(tc.to, "test", time.Now())
		if err == nil {
			t.Errorf("%s -> %s did not error", tc.from, tc.to)
			continue
		}
		if _, ok := err.(*ErrIllegalTransition); !ok {
			t.Errorf("%s -> %s returned %T, want *ErrIllegalTransition", tc.from, tc.to, err)
		}
	}
}

func TestTransitionToSameStateIsANoop(t *testing.T) {
	rec := Record{State: StateRunning}
	if err := rec.Transition(StateRunning, "still running", time.Now()); err != nil {
		t.Fatalf("same-state transition: %v", err)
	}
	if len(rec.Transitions) != 0 {
		t.Errorf("same-state transition was recorded")
	}
}

func TestTransitionsAreBounded(t *testing.T) {
	rec := Record{State: StateStopped}
	for i := 0; i < maxTransitions*2; i++ {
		if err := rec.Transition(StateStarting, "spawn", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := rec.Transition(StateRunning, "up", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := rec.Transition(StateCrashed, "down", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.Transitions) > maxTransitions {
		t.Errorf("kept %d transitions, want at most %d", len(rec.Transitions), maxTransitions)
	}
}

func TestLaunchSpecBuildsArgvWithoutShell(t *testing.T) {
	spec := LaunchSpec{
		PHP:        "/usr/bin/php",
		Artisan:    "artisan",
		Dir:        "/var/www/application",
		Connection: "redis",
		Queue:      "emails",
		Name:       "gohorizon:production:production:default-supervisor:1a2b3c4d",
		Sleep:      3,
		Tries:      3,
		Timeout:    60,
		Memory:     "60%",
		MaxJobs:    1000,
		MaxTime:    3600,
		Rest:       1,
		Backoff:    5,
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	args := spec.Args()
	want := []string{
		"artisan", "queue:work", "redis",
		"--name=gohorizon:production:production:default-supervisor:1a2b3c4d",
		"--queue=emails",
		"--sleep=3", "--tries=3", "--timeout=60", "--memory=60%",
		"--max-jobs=1000", "--max-time=3600", "--rest=1", "--backoff=5",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v\nwant %v", args, want)
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\n;&|$`") {
			t.Errorf("arg %q contains shell metacharacters", arg)
		}
	}
}

func TestLaunchSpecOmitsDisabledRecyclingOptions(t *testing.T) {
	spec := LaunchSpec{
		PHP:        "/usr/bin/php",
		Artisan:    "artisan",
		Dir:        "/srv/app",
		Connection: "redis",
		Queue:      "default",
		Name:       "gohorizon:app:production:sup:deadbeef",
		Sleep:      3,
		Tries:      0,
		Timeout:    60,
		Memory:     "256",
	}
	joined := strings.Join(spec.Args(), " ")
	for _, unexpected := range []string{"--max-jobs", "--max-time", "--rest", "--backoff"} {
		if strings.Contains(joined, unexpected) {
			t.Errorf("args contain %s although it is disabled: %s", unexpected, joined)
		}
	}
	if !strings.Contains(joined, "--tries=0") {
		t.Errorf("--tries=0 must be explicit: %s", joined)
	}
}

func TestLaunchSpecRejectsMultiQueueAndInjection(t *testing.T) {
	base := LaunchSpec{
		PHP:        "/usr/bin/php",
		Artisan:    "artisan",
		Dir:        "/srv/app",
		Connection: "redis",
		Queue:      "default",
		Name:       "gohorizon:app:production:sup:deadbeef",
		Sleep:      3,
		Timeout:    60,
		Memory:     "256",
	}
	cases := []struct {
		name   string
		mutate func(*LaunchSpec)
		want   string
	}{
		{"comma queue", func(s *LaunchSpec) { s.Queue = "a,b" }, "comma"},
		{"php injection", func(s *LaunchSpec) { s.PHP = "/usr/bin/php; rm -rf /" }, "absolute path"},
		{"queue injection", func(s *LaunchSpec) { s.Queue = "default;id" }, "single queue name"},
		{"relative dir", func(s *LaunchSpec) { s.Dir = "app" }, "absolute path"},
		{"artisan traversal", func(s *LaunchSpec) { s.Artisan = "../../artisan" }, "relative filename"},
		{"bad name", func(s *LaunchSpec) { s.Name = "php artisan queue:work" }, "gohorizon:"},
		{"bad memory", func(s *LaunchSpec) { s.Memory = "" }, "memory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			tc.mutate(&spec)
			err := spec.Validate()
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestWorkerName(t *testing.T) {
	got := WorkerName("production", "production", "default-supervisor", "1a2b3c4d5e6f7a8b")
	want := "gohorizon:production:production:default-supervisor:1a2b3c4d"
	if got != want {
		t.Errorf("WorkerName = %q, want %q", got, want)
	}
}
