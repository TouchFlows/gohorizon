package config

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ValidationError aggregates every problem found in a configuration so the operator
// sees all of them at once instead of one per restart.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d configuration problems:\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

func (e *ValidationError) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

var (
	slugRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	queueRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	artisanRE = regexp.MustCompile(`^[A-Za-z0-9._=:-]+$`)
	listenRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+:\d{1,5}$`)
)

// Validate checks every configuration invariant that does not need filesystem or
// network access. Every problem is reported; nothing is silently repaired.
func (c *Config) Validate() error {
	errs := &ValidationError{}

	if c.Version != SupportedVersion {
		errs.add("version: must be %d, got %d", SupportedVersion, c.Version)
	}

	if c.Server.Listen == "" {
		errs.add("server.listen: required (e.g. 127.0.0.1:8090)")
	} else if !listenRE.MatchString(c.Server.Listen) {
		errs.add("server.listen: %q must be host:port", c.Server.Listen)
	} else if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs.add("server.listen: %v", err)
	}

	switch c.Storage.Driver {
	case "sqlite":
		if c.Storage.Path == "" {
			errs.add("storage.path: required for the sqlite driver")
		} else if !filepath.IsAbs(c.Storage.Path) {
			errs.add("storage.path: %q must be an absolute path", c.Storage.Path)
		}
	case "":
		errs.add("storage.driver: required (supported: sqlite)")
	default:
		errs.add("storage.driver: %q is not supported (supported: sqlite)", c.Storage.Driver)
	}

	if c.Redis.Address == "" {
		errs.add("redis.address: required")
	} else if _, _, err := net.SplitHostPort(c.Redis.Address); err != nil {
		errs.add("redis.address: %q must be host:port", c.Redis.Address)
	}
	if c.Redis.DB < 0 {
		errs.add("redis.db: must be zero or positive")
	}
	if c.Redis.PasswordEnv == "" {
		errs.add("redis.password_env: required; secrets are read from the environment, never from this file")
	}

	switch len(c.Applications) {
	case 0:
		errs.add("applications: at least one application is required")
	case 1:
	default:
		errs.add("applications: v1 manages exactly one application, got %d (multi-application support is Milestone 5)", len(c.Applications))
	}

	seenSupervisors := map[string]string{}
	for i := range c.Applications {
		c.Applications[i].validate(errs, seenSupervisors)
	}

	if len(errs.Problems) > 0 {
		return errs
	}
	return nil
}

func (a *Application) validate(errs *ValidationError, seenSupervisors map[string]string) {
	label := fmt.Sprintf("applications[%s]", a.ID)

	if !slugRE.MatchString(a.ID) {
		errs.add("%s.id: %q must match %s", label, a.ID, slugRE)
	}
	if !slugRE.MatchString(a.Environment) {
		errs.add("%s.environment: %q must match %s", label, a.Environment, slugRE)
	}
	if a.Path == "" {
		errs.add("%s.path: required", label)
	} else if !filepath.IsAbs(a.Path) {
		errs.add("%s.path: %q must be absolute", label, a.Path)
	}
	if !filepath.IsAbs(a.PHP) {
		errs.add("%s.php: %q must be an absolute path to the PHP binary", label, a.PHP)
	}
	if a.Artisan == "" {
		errs.add("%s.artisan: required", label)
	} else if filepath.IsAbs(a.Artisan) || strings.Contains(a.Artisan, "..") || !artisanRE.MatchString(a.Artisan) {
		errs.add("%s.artisan: %q must be a relative filename inside the application path", label, a.Artisan)
	}
	if a.RedisConnection == "" {
		errs.add("%s.redis_connection: required (the queue connection name, e.g. redis)", label)
	}
	if len(a.Supervisors) == 0 {
		errs.add("%s.supervisors: at least one supervisor is required", label)
	}

	for i := range a.Supervisors {
		sup := &a.Supervisors[i]
		supLabel := fmt.Sprintf("%s.supervisors[%s]", label, sup.ID)

		if !slugRE.MatchString(sup.ID) {
			errs.add("%s.id: %q must match %s", supLabel, sup.ID, slugRE)
		}
		if owner, dup := seenSupervisors[sup.ID]; dup {
			errs.add("%s.id: duplicate supervisor id (also used by %s); ids are globally unique because the API addresses /supervisors/{id}", supLabel, owner)
		} else if slugRE.MatchString(sup.ID) {
			seenSupervisors[sup.ID] = label
		}

		if len(sup.Queues) == 0 {
			errs.add("%s.queues: at least one queue is required", supLabel)
		}
		seenQueues := map[string]bool{}
		for _, q := range sup.Queues {
			if !queueRE.MatchString(q) {
				errs.add("%s.queues: %q is not a valid queue name", supLabel, q)
			}
			if seenQueues[q] {
				errs.add("%s.queues: %q is listed twice", supLabel, q)
			}
			seenQueues[q] = true
		}

		switch sup.Balance.Strategy {
		case "fixed":
		case "simple", "auto":
			errs.add("%s.balance.strategy: %q is not implemented yet; Milestone 4 adds balancing, v1 supports \"fixed\"", supLabel, sup.Balance.Strategy)
		default:
			errs.add("%s.balance.strategy: unknown strategy %q (v1 supports \"fixed\")", supLabel, sup.Balance.Strategy)
		}

		if sup.Workers.Max <= 0 {
			errs.add("%s.workers.max: must be at least 1", supLabel)
		}
		if sup.Workers.Max > 100 {
			errs.add("%s.workers.max: %d exceeds the v1 ceiling of 100", supLabel, sup.Workers.Max)
		}
		if sup.Workers.Min < 0 {
			errs.add("%s.workers.min: must be zero or positive", supLabel)
		}
		if sup.Workers.Min > sup.Workers.Max {
			errs.add("%s.workers: min (%d) must not exceed max (%d)", supLabel, sup.Workers.Min, sup.Workers.Max)
		}

		w := sup.Worker
		if w.Sleep < 0 || w.Sleep > 60 {
			errs.add("%s.worker.sleep: must be between 0 and 60 seconds", supLabel)
		}
		if w.Tries < 0 {
			errs.add("%s.worker.tries: must be zero or positive (0 means retry indefinitely)", supLabel)
		}
		if w.Timeout <= 0 {
			errs.add("%s.worker.timeout: must be a positive number of seconds", supLabel)
		}
		if err := w.Memory.Validate(); err != nil {
			errs.add("%s.worker.memory: %v", supLabel, err)
		}
		if w.MaxJobs < 0 {
			errs.add("%s.worker.max_jobs: must be zero or positive (0 disables recycling)", supLabel)
		}
		if w.MaxTime < 0 {
			errs.add("%s.worker.max_time: must be zero or positive (0 disables recycling)", supLabel)
		}
		if w.Rest < 0 {
			errs.add("%s.worker.rest: must be zero or positive", supLabel)
		}
		if w.Backoff < 0 {
			errs.add("%s.worker.backoff: must be zero or positive", supLabel)
		}

		if sup.ShutdownGrace <= 0 {
			errs.add("%s.shutdown_grace: must be at least 1 second", supLabel)
		} else if w.Timeout > 0 && sup.ShutdownGrace <= w.Timeout+w.Sleep {
			errs.add("%s.shutdown_grace: %ds must exceed worker.timeout + worker.sleep (%ds) so a graceful stop can finish the job in flight",
				supLabel, sup.ShutdownGrace, w.Timeout+w.Sleep)
		}
		if sup.RestartBackoff < 0 {
			errs.add("%s.restart_backoff: must be zero or positive", supLabel)
		}
	}
}

// Validate is the Memory validator for a single queue:work --memory value.
func (m Memory) Validate() error {
	s := strings.TrimSpace(string(m))
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.HasSuffix(s, "%") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
		if err != nil || n <= 0 || n > 100 {
			return fmt.Errorf("%q must be a percentage between 1%% and 100%%", s)
		}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return fmt.Errorf("%q must be a positive number of megabytes or a percentage (e.g. 256 or 60%%)", s)
	}
	return nil
}
