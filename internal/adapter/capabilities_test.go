package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/config"
)

// TestCapabilitiesHelperProcess stands in for `php artisan`: it prints
// GOHORIZON_TEST_CAPABILITIES and exits with GOHORIZON_TEST_CAPABILITIES_EXIT.
func TestCapabilitiesHelperProcess(t *testing.T) {
	payload, ok := os.LookupEnv("GOHORIZON_TEST_CAPABILITIES")
	if !ok {
		t.Skip("helper process only; launched by the capabilities tests")
	}
	os.Stdout.WriteString(payload)
	if os.Getenv("GOHORIZON_TEST_CAPABILITIES_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func testProbe(t *testing.T) Probe {
	t.Helper()
	return Probe{
		PHP:     os.Args[0],
		Artisan: "-test.run=TestCapabilitiesHelperProcess",
		Dir:     t.TempDir(),
	}
}

// capabilitiesJSON is the documented payload, including fields GoHorizon ignores.
func capabilitiesJSON(retryAfter string, pcntl bool) string {
	return `{"schema":1,"package":"gohorizon/laravel","package_version":"0.1.0",` +
		`"laravel":"13.0.0","php":"8.4.28","application":"production","environment":"production",` +
		`"queue":{"default_connection":"redis","connections":{` +
		`"redis":{"driver":"redis","queue":"default","retry_after":` + retryAfter + `,"block_for":null,"after_commit":false,"has_password":false},` +
		`"sync":{"driver":"sync","queue":"default","retry_after":null,"block_for":null,"after_commit":false,"has_password":false}}},` +
		`"runtime":{"pcntl":` + map[bool]string{true: "true", false: "false"}[pcntl] + `,"posix":true,"sapi":"cli"},` +
		`"telemetry":{"enabled":true,"stream":"gohorizon:production:production:events","batch_size":100,"buffer_size":1000},` +
		`"operations":["inspect_queues","telemetry","capabilities"]}`
}

func TestProbeDecodesCapabilities(t *testing.T) {
	tests := []struct{ name, payload string }{
		{"documented payload", capabilitiesJSON("90", true)},
		{"numeric string retry_after", capabilitiesJSON(`"90"`, true)},
		{"application output before the json line", "PHP Deprecated: something happened\n" + capabilitiesJSON("90", true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOHORIZON_TEST_CAPABILITIES", tc.payload)

			caps, err := testProbe(t).Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if caps.Schema != schemaVersion {
				t.Errorf("schema = %d, want %d", caps.Schema, schemaVersion)
			}
			if !caps.Runtime.PCNTL {
				t.Error("pcntl = false, want true")
			}
			if got := caps.Queue.Connections["redis"].RetryAfter; got != 90 {
				t.Errorf("redis retry_after = %d, want 90", got)
			}
			if got := caps.Queue.Connections["sync"].RetryAfter; got != 0 {
				t.Errorf("sync retry_after = %d, want 0 for null", got)
			}
		})
	}
}

func TestProbeReportsAnUnavailableAdapter(t *testing.T) {
	t.Run("command fails", func(t *testing.T) {
		t.Setenv("GOHORIZON_TEST_CAPABILITIES", "Could not open input file: artisan\n")
		t.Setenv("GOHORIZON_TEST_CAPABILITIES_EXIT", "1")

		_, err := testProbe(t).Capabilities(context.Background())
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
		if !strings.Contains(err.Error(), "Could not open input file: artisan") {
			t.Errorf("error = %v, want the artisan output in the message", err)
		}
	})

	t.Run("not json", func(t *testing.T) {
		t.Setenv("GOHORIZON_TEST_CAPABILITIES", "there is no such command\n")

		_, err := testProbe(t).Capabilities(context.Background())
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
	})
}

func checkApp(timeout int) *config.Application {
	return &config.Application{
		ID:              "production",
		Environment:     "production",
		PHP:             "/usr/bin/php",
		RedisConnection: "redis",
		Supervisors: []config.Supervisor{{
			ID:     "default-supervisor",
			Queues: []string{"default"},
			Worker: config.Worker{Sleep: 3, Timeout: timeout},
		}},
	}
}

func decoded(t *testing.T, payload string) *Capabilities {
	t.Helper()
	caps, err := decodeCapabilities([]byte(payload))
	if err != nil {
		t.Fatalf("decodeCapabilities: %v", err)
	}
	return &caps
}

func TestCheckRefusesUnsafeTimeoutConfigurations(t *testing.T) {
	tests := []struct {
		name    string
		caps    string
		timeout int
		want    string
	}{
		{
			name:    "timeout equal to retry_after",
			caps:    capabilitiesJSON("60", true),
			timeout: 60,
			want:    "must be lower than the retry_after",
		},
		{
			name:    "timeout without pcntl",
			caps:    capabilitiesJSON("90", false),
			timeout: 60,
			want:    "pcntl extension is missing",
		},
		{
			name:    "connection not defined",
			caps:    `{"schema":1,"queue":{"connections":{"sqs":{"retry_after":90}}},"runtime":{"pcntl":true}}`,
			timeout: 60,
			want:    `queue connection "redis" is not defined`,
		},
		{
			name:    "unknown schema",
			caps:    `{"schema":2,"queue":{"connections":{}},"runtime":{"pcntl":true}}`,
			timeout: 60,
			want:    "schema 2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check(decoded(t, tc.caps), checkApp(tc.timeout)); err == nil {
				t.Fatalf("Check accepted timeout %d with the fixture's retry_after/pcntl", tc.timeout)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCheckAcceptsACompatibleLaunch(t *testing.T) {
	if err := Check(decoded(t, capabilitiesJSON("90", true)), checkApp(60)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckAcceptsAMissingRetryAfter(t *testing.T) {
	// A connection without a reservation window (sync/database) has nothing to
	// compare against; that is not a failure.
	app := checkApp(60)
	app.RedisConnection = "sync"
	if err := Check(decoded(t, capabilitiesJSON("90", true)), app); err != nil {
		t.Fatalf("Check: %v", err)
	}
}
