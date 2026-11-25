// Package config loads and validates gohorizon.yaml.
//
// Loading rules (see docs/adr/0002-exit-classification.md and
// docs/adr/0003-redis-namespace.md):
//   - ${VAR} and ${VAR:-default} are expanded before parsing; an unset variable
//     without a default is an error, never silently empty.
//   - Secrets are referenced by environment variable name (redis.password_env) and
//     are never stored in the parsed configuration.
//   - Validation is pure (no filesystem or network access) so it can run in tests
//     and in `gohorizon validate` on a machine without the application deployed.
package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// Memory is a worker memory limit: megabytes ("256") or a percentage of PHP's
// memory_limit ("60%"). Laravel 13 accepts both forms for queue:work --memory.
type Memory string

// Config is the root of gohorizon.yaml.
type Config struct {
	Version      int           `yaml:"version"`
	Server       Server        `yaml:"server"`
	Storage      Storage       `yaml:"storage"`
	Redis        Redis         `yaml:"redis"`
	Applications []Application `yaml:"applications"`
}

type Server struct {
	Listen  string `yaml:"listen"`
	Metrics bool   `yaml:"metrics"`
	Auth    Auth   `yaml:"auth"`
}

// Auth configures sign-in for the dashboard and the operational API (handoff §13).
// Without users the service stays open, which is the localhost default; a deployment that
// is reachable for anyone else configures users and a signing key.
type Auth struct {
	// SessionKeyEnv names the environment variable holding the HMAC key that signs
	// session cookies. The key itself never appears in this file.
	SessionKeyEnv string `yaml:"session_key_env"`
	// SessionHours is how long a sign-in lasts. Zero takes DefaultSessionHours.
	SessionHours int    `yaml:"session_hours"`
	Users        []User `yaml:"users"`
}

// Enabled reports whether authentication is configured at all.
func (a Auth) Enabled() bool { return len(a.Users) > 0 }

// User is one operator. The password is stored as a hash produced by `gohorizon passwd`;
// the plaintext is never written here.
type User struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"password_hash"`
}

type Storage struct {
	Driver    string    `yaml:"driver"`
	Path      string    `yaml:"path"`
	Retention Retention `yaml:"retention"`
}

// Retention configures how long each level of metrics history lives (handoff §9). Raw
// telemetry is measured in hours because the handoff's window is 24–72 hours; the
// aggregates are measured in days.
type Retention struct {
	RawHours   int `yaml:"raw_hours"`
	MinuteDays int `yaml:"minute_days"`
	HourlyDays int `yaml:"hourly_days"`
	DailyDays  int `yaml:"daily_days"`
}

type Redis struct {
	Address     string `yaml:"address"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	DB          int    `yaml:"db"`
	TLS         bool   `yaml:"tls"`
}

type Application struct {
	ID              string       `yaml:"id"`
	Name            string       `yaml:"name"`
	Path            string       `yaml:"path"`
	PHP             string       `yaml:"php"`
	Artisan         string       `yaml:"artisan"`
	Environment     string       `yaml:"environment"`
	RedisConnection string       `yaml:"redis_connection"`
	Supervisors     []Supervisor `yaml:"supervisors"`
}

type Supervisor struct {
	ID      string   `yaml:"id"`
	Queues  []string `yaml:"queues"`
	Balance Balance  `yaml:"balance"`
	Workers Workers  `yaml:"workers"`
	Worker  Worker   `yaml:"worker"`
	// ShutdownGrace is the number of seconds between SIGTERM and SIGKILL.
	ShutdownGrace int `yaml:"shutdown_grace"`
	// RestartBackoff is the base number of seconds before restarting a crashed worker.
	RestartBackoff int `yaml:"restart_backoff"`
}

type Balance struct {
	Strategy string `yaml:"strategy"`
}

type Workers struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

type Worker struct {
	Sleep   int    `yaml:"sleep"`
	Tries   int    `yaml:"tries"`
	Timeout int    `yaml:"timeout"`
	Memory  Memory `yaml:"memory"`
	MaxJobs int    `yaml:"max_jobs"`
	MaxTime int    `yaml:"max_time"`
	Rest    int    `yaml:"rest"`
	Backoff int    `yaml:"backoff"`
}

// Defaults from `php artisan queue:work` (Laravel 13) where a zero value would be
// either invalid or surprising.
const (
	DefaultSleep          = 3
	DefaultTries          = 1
	DefaultTimeout        = 60
	DefaultMemory         = Memory("128")
	DefaultShutdownGrace  = 120
	DefaultRestartBackoff = 5

	// DefaultSessionHours bounds a sign-in for a day when the configuration is silent.
	DefaultSessionHours = 12

	// Metrics retention (handoff §9): the upper end of the raw window, then 7 days of
	// minute buckets, 30 days of hours and 12 months of days.
	DefaultRawRetentionHours   = 72
	DefaultMinuteRetentionDays = 7
	DefaultHourlyRetentionDays = 30
	DefaultDailyRetentionDays  = 365
)

const SupportedVersion = 1

// Load reads and parses a configuration file.
func Load(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse expands environment variables, parses YAML and normalises defaults.
// Validate is deliberately not called; callers decide when to validate.
func Parse(src []byte) (*Config, error) {
	expanded, err := ExpandEnv(src)
	if err != nil {
		return nil, err
	}

	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(expanded), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	var cfg Config
	err = k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "yaml",
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook:       memoryDecodeHook,
			WeaklyTypedInput: true,
			ErrorUnused:      true,
			TagName:          "yaml",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	cfg.normalize()
	return &cfg, nil
}

func (c *Config) normalize() {
	retention := &c.Storage.Retention
	if retention.RawHours == 0 {
		retention.RawHours = DefaultRawRetentionHours
	}
	if retention.MinuteDays == 0 {
		retention.MinuteDays = DefaultMinuteRetentionDays
	}
	if retention.HourlyDays == 0 {
		retention.HourlyDays = DefaultHourlyRetentionDays
	}
	if retention.DailyDays == 0 {
		retention.DailyDays = DefaultDailyRetentionDays
	}

	if c.Server.Auth.Enabled() && c.Server.Auth.SessionHours == 0 {
		c.Server.Auth.SessionHours = DefaultSessionHours
	}

	for ai := range c.Applications {
		app := &c.Applications[ai]
		if app.Artisan == "" {
			app.Artisan = "artisan"
		}
		if app.PHP == "" {
			app.PHP = "/usr/bin/php"
		}
		for si := range app.Supervisors {
			sup := &app.Supervisors[si]
			if sup.Worker.Sleep == 0 {
				sup.Worker.Sleep = DefaultSleep
			}
			if sup.Worker.Timeout == 0 {
				sup.Worker.Timeout = DefaultTimeout
			}
			if sup.Worker.Memory == "" {
				sup.Worker.Memory = DefaultMemory
			}
			if sup.ShutdownGrace == 0 {
				sup.ShutdownGrace = DefaultShutdownGrace
			}
			if sup.RestartBackoff == 0 {
				sup.RestartBackoff = DefaultRestartBackoff
			}
			if sup.Balance.Strategy == "" {
				sup.Balance.Strategy = "fixed"
			}
		}
	}
}

// Application returns the single configured application. v1 supports exactly one;
// callers must have validated the configuration first.
func (c *Config) Application() *Application {
	if len(c.Applications) == 0 {
		return nil
	}
	return &c.Applications[0]
}

func (s *Supervisor) ShutdownGraceDuration() time.Duration {
	return time.Duration(s.ShutdownGrace) * time.Second
}

func (s *Supervisor) RestartBackoffDuration() time.Duration {
	if s.RestartBackoff <= 0 {
		return time.Second
	}
	return time.Duration(s.RestartBackoff) * time.Second
}

// Password resolves redis.password_env at connection time. The value is never
// stored on the config or logged.
func (r Redis) Password() string {
	if r.PasswordEnv == "" {
		return ""
	}
	return os.Getenv(r.PasswordEnv)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv substitutes ${VAR} and ${VAR:-default}. It fails on an unset variable
// that has no default, instead of expanding to an empty string.
func ExpandEnv(src []byte) ([]byte, error) {
	var missing []string

	out := envRef.ReplaceAllFunc(src, func(match []byte) []byte {
		groups := envRef.FindSubmatch(match)
		name := string(groups[1])
		if value, ok := os.LookupEnv(name); ok {
			return []byte(value)
		}
		if groups[2] != nil {
			return groups[2]
		}
		missing = append(missing, name)
		return match
	})

	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variable(s): %s (use ${VAR:-default} for an optional value)", strings.Join(missing, ", "))
	}
	return out, nil
}

var memoryType = reflect.TypeOf(Memory(""))

func memoryDecodeHook(_, to reflect.Type, data any) (any, error) {
	if to != memoryType {
		return data, nil
	}
	switch v := data.(type) {
	case Memory:
		return v, nil
	case string:
		return Memory(strings.TrimSpace(v)), nil
	case int:
		return Memory(strconv.Itoa(v)), nil
	case int64:
		return Memory(strconv.FormatInt(v, 10)), nil
	case float64:
		return Memory(strconv.FormatFloat(v, 'f', -1, 64)), nil
	case nil:
		return Memory(""), nil
	}
	return nil, fmt.Errorf("memory must be megabytes or a percentage, got %T", data)
}
