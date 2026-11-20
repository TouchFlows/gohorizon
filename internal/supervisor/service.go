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
	"github.com/gohorizon/gohorizon/internal/metrics"
	"github.com/gohorizon/gohorizon/internal/process"
	"github.com/gohorizon/gohorizon/internal/redis"
	"github.com/gohorizon/gohorizon/internal/telemetry"
	"github.com/gohorizon/gohorizon/internal/version"
)

const (
	lockTTL     = 30 * time.Second
	lockRenew   = 10 * time.Second
	lockTimeout = 5 * time.Second

	// defaultPruneInterval bounds how far the metrics database can outgrow its
	// retention windows between two prune passes. It is a field on the service so a
	// test can watch the scheduled pass without waiting an hour.
	defaultPruneInterval = time.Hour

	// defaultQueueInterval is how often queue depth is sampled through the adapter.
	// Counts are observations, never an execution ledger (docs/adr/0001).
	defaultQueueInterval = 15 * time.Second
)

// ErrLocked is returned when another instance owns the supervisor lock. Startup
// aborts: two supervisors must never manage the same pool (docs/adr/0004).
var ErrLocked = errors.New("another GoHorizon instance holds the supervisor lock")

// queueInspector is the adapter surface queue depth is sampled through
// (docs/adr/0001). adapter.Probe implements it.
type queueInspector interface {
	Queues(ctx context.Context, connection string, queues []string) (adapter.Inspection, error)
}

type Service struct {
	cfg      *config.Config
	log      *slog.Logger
	client   *redis.Client
	consumer *telemetry.Consumer
	store    *metrics.Store
	managers []*process.Manager

	inspector     queueInspector
	queues        []string
	queueInterval time.Duration
	pruneInterval time.Duration
	started       time.Time
	owner         string

	mu         sync.Mutex
	lockHeld   bool
	lockErr    string
	queueDepth health.Queues
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
		cfg:           cfg,
		log:           log,
		client:        client,
		started:       time.Now(),
		owner:         fmt.Sprintf("%s-%d", version.Version, time.Now().UnixNano()),
		inspector:     probe,
		queues:        configuredQueues(app),
		queueInterval: defaultQueueInterval,
		pruneInterval: defaultPruneInterval,
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

	// Metrics history is durable but never a reason to refuse supervision: if the
	// database cannot be opened, history is disabled with a warning and the workers
	// start anyway (docs/adr/0007).
	var store *metrics.Store
	if cfg.Storage.Driver == "sqlite" {
		opened, openErr := metrics.Open(cfg.Storage.Path)
		if openErr != nil {
			log.Warn("metrics history disabled: the storage database could not be opened",
				"path", cfg.Storage.Path, "error", openErr)
		} else {
			store = opened
		}
	} else {
		log.Warn("metrics history disabled: storage.driver is not sqlite", "driver", cfg.Storage.Driver)
	}

	// The consumer feeds WorkerStopping reasons back to the managers, so an exit is
	// classified by what Laravel reported instead of by the exit status alone
	// (docs/adr/0002 item 5), and persists job metrics. It is built last because it
	// needs the managers.
	spec := telemetry.Spec{
		Application:     app.ID,
		Environment:     app.Environment,
		Logger:          log,
		OnWorkerStopped: s.noteStopReason,
	}
	if store != nil {
		spec.Sink = store
	}
	consumer, err := telemetry.New(client, spec)
	if err != nil {
		if store != nil {
			_ = store.Close()
		}
		client.Close()
		return nil, err
	}
	s.consumer = consumer
	s.store = store

	return s, nil
}

// Buckets reads persisted queue history for the read-only metrics endpoint. A service
// whose store never opened answers ErrDisabled instead of an empty list that would look
// like "no jobs ran" (docs/adr/0007).
func (s *Service) Buckets(ctx context.Context, queue string, from, to time.Time) ([]metrics.Bucket, error) {
	if s.store == nil {
		return nil, metrics.ErrDisabled
	}
	return s.store.Buckets(ctx, queue, from, to)
}

func (s *Service) Close() error {
	err := s.client.Close()
	if s.store != nil {
		if storeErr := s.store.Close(); storeErr != nil && err == nil {
			err = storeErr
		}
	}
	return err
}

// pruneLoop keeps the metrics database from growing without bound: it prunes on startup
// and then every pruneInterval (an hour in production, docs/adr/0007). It is a no-op
// when history is disabled.
func (s *Service) pruneLoop(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.pruneOnce(ctx)
	ticker := time.NewTicker(s.pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pruneOnce(ctx)
		}
	}
}

func (s *Service) pruneOnce(ctx context.Context) {
	pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pruned, err := s.store.Prune(pruneCtx, time.Now())
	if err != nil {
		s.log.Warn("metrics retention pruning failed", "error", err)
		return
	}
	if pruned.Events > 0 || pruned.Buckets > 0 {
		s.log.Info("pruned metrics history", "events", pruned.Events, "buckets", pruned.Buckets)
	}
}

// queueLoop samples queue depth through the Laravel adapter. A missing package or a
// failing command leaves an error in the report body and never affects the workers
// (docs/adr/0001).
func (s *Service) queueLoop(ctx context.Context) {
	s.sampleQueues(ctx)
	ticker := time.NewTicker(s.queueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleQueues(ctx)
		}
	}
}

func (s *Service) sampleQueues(ctx context.Context) {
	inspection, err := s.inspector.Queues(ctx, s.cfg.Application().RedisConnection, s.queues)
	if err != nil {
		s.mu.Lock()
		changed := s.queueDepth.Error != err.Error()
		s.queueDepth = health.Queues{Error: err.Error()}
		s.mu.Unlock()
		if changed {
			s.log.Warn("queue depth unavailable; the application's inspection command failed", "error", err)
		}
		return
	}

	depths := make([]health.QueueDepth, 0, len(inspection.Queues))
	for _, queue := range inspection.Queues {
		depths = append(depths, health.QueueDepth{
			Queue:    queue.Queue,
			Pending:  queue.Pending,
			Reserved: queue.Reserved,
			Delayed:  queue.Delayed,
		})
	}
	s.mu.Lock()
	recovered := s.queueDepth.Error != ""
	s.queueDepth = health.Queues{Connection: inspection.Connection, Depths: depths}
	s.mu.Unlock()
	if recovered {
		s.log.Info("queue depth inspection recovered")
	}
}

// configuredQueues lists every queue the supervisors launch workers for, once.
func configuredQueues(app *config.Application) []string {
	seen := make(map[string]bool)
	var queues []string
	for _, sup := range app.Supervisors {
		for _, queue := range sup.Queues {
			if !seen[queue] {
				seen[queue] = true
				queues = append(queues, queue)
			}
		}
	}
	return queues
}

// noteStopReason hands a reported WorkerStopping reason to the manager that owns the
// worker. An unmatched reason belongs to a worker this instance no longer tracks.
func (s *Service) noteStopReason(worker, reason string) {
	for _, manager := range s.managers {
		if manager.NoteStopReason(worker, reason) {
			return
		}
	}
	s.log.Debug("worker stop reason did not match a live worker", "worker", worker, "reason", reason)
}

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

	// Telemetry is observation: Run never returns an error and never touches the
	// workers, so a Redis outage degrades telemetry and nothing else (handoff §2.11).
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.consumer.Run(runCtx)
		stats := s.consumer.Stats()
		s.log.Info("telemetry consumer stopped", "events", stats.Events,
			"duplicates", stats.Duplicates, "invalid", stats.Invalid, "errors", stats.Errors)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.renewLoop(runCtx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pruneLoop(runCtx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.queueLoop(runCtx)
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
	queues := s.queueDepth
	s.mu.Unlock()

	app := s.cfg.Application()
	report := health.Report{
		Status:        "ok",
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Lock:          health.Lock{Held: held, Degraded: lockErr != "", LastError: lockErr},
		Queues:        queues,
	}
	if app != nil {
		report.Application = app.ID
		report.Environment = app.Environment
	}
	stats := s.consumer.Stats()
	report.Telemetry = health.Telemetry{
		Events:     stats.Events,
		Duplicates: stats.Duplicates,
		Invalid:    stats.Invalid,
		Errors:     stats.Errors,
		Failing:    stats.Failing,
		LastError:  stats.LastError,
	}
	// Ingestion is reported in the body but never degrades the status: a failed health
	// check would restart or kill a supervisor whose workers are fine.
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
