// Package supervisor wires configuration, the ownership lock and the process managers
// into one service. It owns no worker semantics itself.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/config"
	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/process"
	"github.com/gohorizon/gohorizon/internal/redis"
	"github.com/gohorizon/gohorizon/internal/version"
)

const (
	lockTTL     = 30 * time.Second
	lockRenew   = 10 * time.Second
	lockTimeout = 5 * time.Second
)

// ErrLocked is returned when another instance owns the supervisor lock. Startup
// aborts: two supervisors must never manage the same pool (docs/adr/0004).
var ErrLocked = errors.New("another GoHorizon instance holds the supervisor lock")

type Service struct {
	cfg      *config.Config
	log      *slog.Logger
	client   *redis.Client
	managers []*process.Manager
	started  time.Time
	owner    string

	mu       sync.Mutex
	lockHeld bool
	lockErr  string
}

func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Service, error) {
	app := cfg.Application()
	if app == nil {
		return nil, errors.New("configuration has no application")
	}

	// A timeout Laravel cannot enforce, or one at or above the connection's
	// retry_after, stops startup instead of producing workers that silently
	// double-run jobs (docs/adr/0002). A missing package degrades the check with a
	// warning rather than blocking supervision (docs/adr/0001).
	probe := adapter.Probe{PHP: app.PHP, Artisan: app.Artisan, Dir: app.Path}
	caps, probeErr := probe.Capabilities(ctx)
	if probeErr != nil {
		log.Warn("application capabilities unavailable; pcntl and retry_after preflight skipped",
			"application", app.ID, "php", app.PHP, "error", probeErr)
	} else if err := adapter.Check(&caps, app); err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}

	keys, err := redis.NewKeys(app.ID, app.Environment)
	if err != nil {
		return nil, err
	}
	client, err := redis.Open(ctx, redis.Config{
		Address:  cfg.Redis.Address,
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password(),
		DB:       cfg.Redis.DB,
		TLS:      cfg.Redis.TLS,
	}, keys)
	if err != nil {
		return nil, err
	}

	s := &Service{
		cfg:     cfg,
		log:     log,
		client:  client,
		started: time.Now(),
		owner:   fmt.Sprintf("%s-%d", version.Version, time.Now().UnixNano()),
	}

	for i := range app.Supervisors {
		sup := &app.Supervisors[i]
		manager, err := process.New(process.Spec{
			ApplicationID: app.ID,
			Environment:   app.Environment,
			SupervisorID:  sup.ID,
			Launch: process.LaunchSpec{
				PHP:        app.PHP,
				Artisan:    app.Artisan,
				Dir:        app.Path,
				Connection: app.RedisConnection,
				Sleep:      sup.Worker.Sleep,
				Tries:      sup.Worker.Tries,
				Timeout:    sup.Worker.Timeout,
				Memory:     string(sup.Worker.Memory),
				MaxJobs:    sup.Worker.MaxJobs,
				MaxTime:    sup.Worker.MaxTime,
				Rest:       sup.Worker.Rest,
				Backoff:    sup.Worker.Backoff,
			},
			Queues:         sup.Queues,
			Desired:        sup.Workers.Min,
			ShutdownGrace:  sup.ShutdownGraceDuration(),
			RestartBackoff: sup.RestartBackoffDuration(),
			StatePath:      filepath.Join(filepath.Dir(cfg.Storage.Path), "workers-"+sup.ID+".json"),
			Logger:         log,
		})
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("supervisor %q: %w", sup.ID, err)
		}
		s.managers = append(s.managers, manager)
	}

	return s, nil
}

func (s *Service) Close() error { return s.client.Close() }

// Run acquires the ownership lock, starts every supervisor and blocks until the
// context is cancelled. Worker shutdown happens inside the managers, so cancelling
// the context is a graceful stop, not a kill.
func (s *Service) Run(ctx context.Context) error {
	acquired, err := s.client.AcquireSupervisorLock(ctx, s.owner, lockTTL)
	if err != nil {
		return err
	}
	if !acquired {
		holder, _ := s.client.SupervisorLockHolder(ctx)
		return fmt.Errorf("%w (owner %q)", ErrLocked, holder)
	}
	s.setLock(true, "")
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), lockTimeout)
		defer cancel()
		if err := s.client.ReleaseSupervisorLock(releaseCtx, s.owner); err != nil {
			s.log.Warn("could not release supervisor lock", "error", err)
		}
	}()

	app := s.cfg.Application()
	s.log.Info("supervisor lock acquired",
		"application", app.ID, "environment", app.Environment,
		"supervisors", len(s.managers), "owner", s.owner)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(s.managers))

	for _, manager := range s.managers {
		wg.Add(1)
		go func(m *process.Manager) {
			defer wg.Done()
			if err := m.Run(runCtx); err != nil {
				errs <- fmt.Errorf("supervisor %q: %w", m.Snapshot().SupervisorID, err)
			}
		}(manager)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.renewLoop(runCtx)
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

// renewLoop keeps the ownership observable. A renewal failure degrades health but
// never stops supervision: a Redis outage must not stop jobs (handoff §2.11).
func (s *Service) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(lockRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.Background(), lockTimeout)
			held, err := s.client.RenewSupervisorLock(renewCtx, s.owner, lockTTL)
			cancel()
			switch {
			case err != nil:
				s.log.Warn("supervisor lock renewal failed; supervision continues, telemetry is degraded", "error", err)
				s.setLock(false, err.Error())
			case !held:
				s.log.Error("supervisor lock is no longer owned by this instance; supervision continues, a second instance may start")
				s.setLock(false, "lock is no longer owned by this instance")
			default:
				s.setLock(true, "")
			}
		}
	}
}

func (s *Service) setLock(held bool, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockHeld = held
	s.lockErr = lastErr
}

// Report builds the health payload.
func (s *Service) Report() health.Report {
	s.mu.Lock()
	held, lockErr := s.lockHeld, s.lockErr
	s.mu.Unlock()

	app := s.cfg.Application()
	report := health.Report{
		Status:        "ok",
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Lock:          health.Lock{Held: held, Degraded: lockErr != "", LastError: lockErr},
	}
	if app != nil {
		report.Application = app.ID
		report.Environment = app.Environment
	}
	if !held {
		report.Status = "degraded"
	}

	for _, manager := range s.managers {
		stats := manager.Snapshot()
		sup := health.Supervisor{
			ID:          stats.SupervisorID,
			Desired:     stats.Desired,
			Running:     stats.Running,
			Stopping:    stats.Stopping,
			Crashed:     stats.Crashed,
			Adopted:     stats.Adopted,
			Escalations: stats.Escalations,
		}
		for _, worker := range stats.Workers {
			sup.Workers = append(sup.Workers, health.Worker{
				UUID:           worker.UUID,
				Queue:          worker.Queue,
				PID:            worker.PID,
				State:          string(worker.State),
				Restarts:       worker.RestartCount,
				LastExitCode:   worker.LastExitCode,
				LastExitReason: worker.LastExitReason,
				Adopted:        worker.Adopted,
			})
		}
		report.Supervisors = append(report.Supervisors, sup)
	}
	return report
}
