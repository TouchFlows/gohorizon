package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

const minimal = `
version: 1
server:
  listen: "127.0.0.1:8090"
  metrics: true
storage:
  driver: sqlite
  path: /var/lib/gohorizon/gohorizon.db
redis:
  address: "127.0.0.1:6379"
  password_env: GOHORIZON_REDIS_PASSWORD
  db: 0
applications:
  - id: production
    name: My Laravel Application
    path: /var/www/application
    php: /usr/bin/php
    artisan: artisan
    environment: production
    redis_connection: default
    supervisors:
      - id: default-supervisor
        queues: [default, emails]
        balance:
          strategy: fixed
        workers:
          min: 2
          max: 5
        worker:
          sleep: 3
          tries: 3
          timeout: 60
          memory: 256
          max_jobs: 1000
          max_time: 3600
        shutdown_grace: 120
        restart_backoff: 5
`

func TestParseMinimalConfig(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	app := cfg.Application()
	if app == nil || app.ID != "production" {
		t.Fatalf("unexpected application: %+v", app)
	}
	sup := app.Supervisors[0]
	if sup.Worker.Memory != Memory("256") {
		t.Errorf("memory = %q, want 256", sup.Worker.Memory)
	}
	if got := sup.ShutdownGraceDuration(); got != 120*time.Second {
		t.Errorf("shutdown grace = %v, want 2m", got)
	}
	if got := sup.RestartBackoffDuration(); got != 5*time.Second {
		t.Errorf("restart backoff = %v, want 5s", got)
	}
}

func TestParseAcceptsPercentageMemoryAndYAMLInteger(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want Memory
	}{
		{`memory: 60%`, Memory("60%")},
		{`memory: "60%"`, Memory("60%")},
		{`memory: 256`, Memory("256")},
	} {
		src := strings.Replace(minimal, "memory: 256", tc.src, 1)
		cfg, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.src, err)
		}
		if got := cfg.Application().Supervisors[0].Worker.Memory; got != tc.want {
			t.Errorf("%s: memory = %q, want %q", tc.src, got, tc.want)
		}
	}
}

func TestParseAppliesQueueWorkDefaults(t *testing.T) {
	src := strings.NewReplacer(
		"          sleep: 3\n", "",
		"          tries: 3\n", "",
		"          timeout: 60\n", "",
		"          memory: 256\n", "",
		"        shutdown_grace: 120\n", "",
		"        restart_backoff: 5\n", "",
		"        balance:\n          strategy: fixed\n", "",
	).Replace(minimal)

	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	w := cfg.Application().Supervisors[0].Worker
	if w.Sleep != DefaultSleep || w.Timeout != DefaultTimeout || w.Memory != DefaultMemory {
		t.Errorf("defaults not applied: %+v", w)
	}
	if w.Tries != 0 {
		t.Errorf("tries = %d, want 0 (0 means retry indefinitely and is a valid explicit value)", w.Tries)
	}
	sup := cfg.Application().Supervisors[0]
	if sup.ShutdownGrace != DefaultShutdownGrace || sup.Balance.Strategy != "fixed" {
		t.Errorf("supervisor defaults not applied: %+v", sup)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("GH_TEST_APP", "production")
	t.Setenv("GH_TEST_DB", "7")

	out, err := ExpandEnv([]byte("id: ${GH_TEST_APP}\ndb: ${GH_TEST_DB}\nfallback: ${GH_TEST_UNSET:-default}"))
	if err != nil {
		t.Fatalf("ExpandEnv: %v", err)
	}
	want := "id: production\ndb: 7\nfallback: default"
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestExpandEnvFailsOnUnsetVariable(t *testing.T) {
	os.Unsetenv("GH_TEST_MISSING")
	_, err := ExpandEnv([]byte("id: ${GH_TEST_MISSING}"))
	if err == nil || !strings.Contains(err.Error(), "GH_TEST_MISSING") {
		t.Fatalf("expected unset-variable error naming the variable, got %v", err)
	}
}

func TestPasswordComesFromEnvironmentOnly(t *testing.T) {
	r := Redis{PasswordEnv: "GH_TEST_REDIS_PASSWORD"}
	t.Setenv("GH_TEST_REDIS_PASSWORD", "s3cret")
	if got := r.Password(); got != "s3cret" {
		t.Errorf("Password() = %q", got)
	}
	os.Unsetenv("GH_TEST_REDIS_PASSWORD")
	if got := r.Password(); got != "" {
		t.Errorf("Password() with unset env = %q, want empty", got)
	}
}

func TestValidateRejectsEachDocumentedProblem(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"unsupported version", func(s string) string { return strings.Replace(s, "version: 1", "version: 2", 1) }, "version"},
		{"relative app path", func(s string) string {
			return strings.Replace(s, "path: /var/www/application", "path: ./application", 1)
		}, "must be absolute"},
		{"missing redis connection", func(s string) string {
			return strings.Replace(s, "redis_connection: default", "redis_connection: \"\"", 1)
		}, "redis_connection"},
		{"unsupported strategy", func(s string) string { return strings.Replace(s, "strategy: fixed", "strategy: auto", 1) }, "not implemented yet"},
		{"negative timeout", func(s string) string { return strings.Replace(s, "timeout: 60", "timeout: -5", 1) }, "worker.timeout"},
		{"bad memory", func(s string) string { return strings.Replace(s, "memory: 256", "memory: 120%", 1) }, "percentage"},
		{"grace too short", func(s string) string { return strings.Replace(s, "shutdown_grace: 120", "shutdown_grace: 30", 1) }, "shutdown_grace"},
		{"min above max", func(s string) string { return strings.Replace(s, "min: 2", "min: 9", 1) }, "must not exceed max"},
		{"bad queue name", func(s string) string {
			return strings.Replace(s, "queues: [default, emails]", "queues: [default, \"e mails\"]", 1)
		}, "not a valid queue name"},
		{"no password_env", func(s string) string { return strings.Replace(s, "  password_env: GOHORIZON_REDIS_PASSWORD\n", "", 1) }, "password_env"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.mutate(minimal)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsDuplicateSupervisorIDs(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	app := cfg.Application()
	app.Supervisors = append(app.Supervisors, app.Supervisors[0])
	app.Supervisors[1].Queues = []string{"other"}

	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate supervisor id") {
		t.Fatalf("expected duplicate supervisor id error, got %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg, err := Parse([]byte(strings.NewReplacer(
		"timeout: 60", "timeout: -5",
		"min: 2", "min: 99",
		"memory: 256", "memory: nope",
	).Replace(minimal)))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation failure")
	}
	vErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(vErr.Problems) < 3 {
		t.Errorf("expected at least 3 problems, got %d: %v", len(vErr.Problems), vErr.Problems)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	src := strings.Replace(minimal, "  metrics: true", "  metrics: true\n  jsut_a_typo: 1", 1)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected an error for an unknown key")
	}
}
