// Package supervisor wires configuration, the ownership lock and the process managers
// into one service. It owns no worker semantics itself.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/audit"
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

	// lockSilenceAllowance is how long renewals may go unconfirmed before this instance
	// treats the silence as a loss. One TTL is the point at which any other instance could
	// have taken the key; the extra half TTL covers a renewal that was already in flight
	// when Redis stopped answering (docs/adr/0004).
	lockSilenceAllowance = lockTTL + lockTTL/2

	// telemetryDrainTimeout bounds the final ingestion pass of a stop. A slow or
	// unreachable Redis must never hold a shutdown open (handoff §2.11).
	telemetryDrainTimeout = 5 * time.Second

	// defaultPruneInterval bounds how far the metrics database can outgrow its
	// retention windows between two prune passes. It is a field on the service so a
	// test can watch the scheduled pass without waiting an hour.
	defaultPruneInterval = time.Hour

	// defaultQueueInterval is how often queue depth is sampled through the adapter.
	// Counts are observations, never an execution ledger (docs/adr/0001).
	defaultQueueInterval = 15 * time.Second

	// defaultProbeInterval is how often the application's capabilities are re-read. It is slow
	// on purpose: the answer changes when the deployment is repaired, upgraded or broken, not
	// between two jobs, and every read starts PHP. It is a field on the service so a test can
	// watch a re-read without waiting (docs/adr/0014).
	defaultProbeInterval = 30 * time.Second

	// probeFailureThreshold is how many consecutive failed re-reads replace a readable descriptor
	// with the failure. One is a blip — a PHP run can time out because the machine is busy — and
	// withdrawing every control for an interval over that is worse than waiting for the next
	// read, which is the same reason the dashboard keeps the last descriptor it was given
	// (docs/adr/0014).
	probeFailureThreshold = 2
)

// ErrLocked is returned when another instance owns the supervisor lock. Startup
// aborts: two supervisors must never manage the same pool (docs/adr/0004).
var ErrLocked = errors.New("another GoHorizon instance holds the supervisor lock")

// ErrLockLost is returned when this instance no longer owns the lock, either because
// Redis answered that the key is gone or taken over, or because renewals went unconfirmed
// for longer than the lock could survive. The service stops its workers before returning,
// so the pool is left to the new owner instead of being supervised by two instances
// (docs/adr/0004).
var ErrLockLost = errors.New("supervisor lock lost; this instance stopped supervising")

// errLockKeyGone names the definitive loss Redis reports when the key is missing: an
// expiry that another instance can take over from immediately.
var errLockKeyGone = errors.New("the supervisor lock key no longer exists")

// queueInspector is the adapter surface queue depth is sampled through
// (docs/adr/0001). adapter.Probe implements it.
type queueInspector interface {
	Queues(ctx context.Context, connection string, queues []string) (adapter.Inspection, error)
}

// capabilityProbe is the adapter surface the descriptor is read through, at startup and then
// on the service's own interval. adapter.Probe implements it; a test supplies answers without
// a PHP application behind them.
type capabilityProbe interface {
	Capabilities(ctx context.Context) (adapter.Capabilities, error)
}

// failedJobStore is the adapter surface the failed-job read and the two actions go through,
// so a failed job is listed, re-published or deleted by the application, never by GoHorizon
// writing a queue key (docs/adr/0012, docs/adr/0013). adapter.Probe implements it.
type failedJobStore interface {
	FailedJobs(ctx context.Context, query adapter.FailedJobQuery) (adapter.FailedJobsPage, error)
	RetryFailedJob(ctx context.Context, id string) error
	ForgetFailedJob(ctx context.Context, id string) error
}

type Service struct {
	cfg      *config.Config
	log      *slog.Logger
	client   *redis.Client
	consumer *telemetry.Consumer
	store    *metrics.Store
	managers []*process.Manager

	inspector     queueInspector
	failer        failedJobStore
	queues        []string
	queueInterval time.Duration
	pruneInterval time.Duration
	// renewInterval is lockRenew in production. It is a field so a test can watch a
	// lock loss without waiting 10s for the next renewal.
	renewInterval time.Duration
	// renewLock is client.RenewSupervisorLock in production. It is a field so the silent
	// path can be tested with a renewal that never answers, which a real Redis cannot be
	// made to do on demand without disturbing the machine it runs on.
	renewLock func(ctx context.Context, owner string, ttl time.Duration) (bool, error)
	// fenceAfter is lockSilenceAllowance in production: how long renewals may go
	// unconfirmed before the lock is presumed gone.
	fenceAfter time.Duration
	started    time.Time
	owner      string
	// probed is the capabilities answer the endpoint publishes and probeErr says why it could
	// not answer when it did not. They begin as the launch preflight's answer and are re-read on
	// probeInterval, so a deployment repaired while the service runs is published as repaired
	// without a restart (docs/adr/0014). capsMu guards them: the HTTP path reads what the loop
	// writes, and neither may see a half-updated pair.
	probed        adapter.Capabilities
	probeErr      error
	probe         capabilityProbe
	probeInterval time.Duration
	capsMu        sync.RWMutex
	// probeFailures counts the failed re-reads since the last one that answered, so a single
	// blip cannot withdraw a readable descriptor. It is written under capsMu with the answer it
	// belongs to.
	probeFailures int
	// contradiction is what the current answer contradicts in the launch configuration, in the
	// words the preflight refuses with. A pool that is already running cannot be un-launched, so
	// this is published rather than enforced (docs/adr/0014). Written under capsMu with the
	// answer it belongs to.
	contradiction string
	// lastRead is when the loop last got an answer, zero when it never has. A re-read loop that
	// stopped publishes a healthy-looking descriptor, so staleness needs its own field
	// (docs/adr/0014). Written under capsMu with the answer it belongs to.
	lastRead time.Time
	// appID names the application in the re-read's log lines.
	appID string
	// auditor records a descriptor transition with no actor: "when did this go read-only, and
	// why" is asked after the fact, and the log line it would otherwise be lost in is a
	// warning about a state, not a record an operator can select on (docs/adr/0014).
	auditor *audit.Logger

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
	switch {
	case probeErr != nil:
		log.Warn("application capabilities unavailable; pcntl and retry_after preflight skipped",
			"application", app.ID, "php", app.PHP, "error", probeErr)
	case !caps.Supported():
		// A schema this service does not understand is as unusable as one that never
		// arrived: the launch checks would read fields that may mean something else, so
		// they are skipped, and the endpoint publishes the deployment as read-only with
		// both schema numbers as the reason (docs/adr/0014). Supervision continues, so
		// the operator can see the deployment and repair it instead of finding a service
		// that refuses to start.
		log.Warn("application capabilities schema not understood; pcntl and retry_after preflight skipped",
			"application", app.ID, "schema", caps.Schema, "understood", adapter.SchemaVersion)
	default:
		if err := adapter.Check(&caps, app); err != nil {
			return nil, fmt.Errorf("refusing to start: %w", err)
		}
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

	// The launch read is the first answer the loop has: without it health would report no
	// re-read at all until the first interval, which is the state a stopped loop also looks
	// like (docs/adr/0014).
	var launchRead time.Time
	if probeErr == nil {
		launchRead = time.Now()
	}

	s := &Service{
		cfg:           cfg,
		log:           log,
		client:        client,
		renewLock:     client.RenewSupervisorLock,
		started:       time.Now(),
		owner:         fmt.Sprintf("%s-%d", version.Version, time.Now().UnixNano()),
		inspector:     probe,
		failer:        probe,
		probe:         probe,
		probeInterval: defaultProbeInterval,
		appID:         app.ID,
		auditor:       audit.New(log),
		probed:        caps,
		probeErr:      probeErr,
		lastRead:      launchRead,
		queues:        configuredQueues(app),
		queueInterval: defaultQueueInterval,
		pruneInterval: defaultPruneInterval,
		renewInterval: lockRenew,
		fenceAfter:    lockSilenceAllowance,
	}

	for i := range app.Supervisors {
		sup := &app.Supervisors[i]
		warnIfShortPool(log, sup)
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
		opened, openErr := metrics.Open(cfg.Storage.Path, metricsRetention(cfg))
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

// Buckets reads persisted queue history at one resolution for the read-only metrics
// endpoint. A service whose store never opened answers ErrDisabled instead of an empty
// list that would look like "no jobs ran" (docs/adr/0007).
func (s *Service) Buckets(ctx context.Context, resolution metrics.Resolution, queue string, from, to time.Time) ([]metrics.Bucket, error) {
	if s.store == nil {
		return nil, metrics.ErrDisabled
	}
	return s.store.Buckets(ctx, resolution, queue, from, to)
}

// Jobs reads one page of job history for the read-only jobs endpoint. As with Buckets, a
// service whose store never opened answers ErrDisabled rather than an empty page that would
// look like "no job ran" (docs/adr/0007).
func (s *Service) Jobs(ctx context.Context, query metrics.JobQuery) ([]metrics.Job, error) {
	if s.store == nil {
		return nil, metrics.ErrDisabled
	}
	return s.store.Jobs(ctx, query)
}

// Job reads one job's newest attempt by identity for the detail endpoint, and answers
// ErrDisabled when history was never opened.
func (s *Service) Job(ctx context.Context, jobID string) (metrics.Job, error) {
	if s.store == nil {
		return metrics.Job{}, metrics.ErrDisabled
	}
	return s.store.Job(ctx, jobID)
}

// RestartWorker stops one worker gracefully. The supervisor's next reconcile starts its
// replacement in the same slot, so the job in flight finishes instead of being killed — which
// is what "restart" has to mean for a queue worker (docs/adr/0002). A UUID no supervisor runs
// is reported so the caller can answer 404 rather than pretend something happened.
func (s *Service) RestartWorker(uuid string) error {
	if uuid == "" {
		return errors.New("no worker id given")
	}
	for _, manager := range s.managers {
		if err := manager.StopWorker(uuid); err == nil {
			s.log.Info("worker restart requested", "uuid", uuid)
			return nil
		}
	}
	return fmt.Errorf("no running worker with id %s", uuid)
}

// Capabilities returns the application descriptor the service last read, or the error that
// stopped it answering. The launch preflight's answer is the first one; probeLoop replaces it,
// so the endpoint publishes what the application says now rather than only what it said at
// startup (docs/adr/0014).
func (s *Service) Capabilities() (adapter.Capabilities, error) {
	s.capsMu.RLock()
	defer s.capsMu.RUnlock()
	return s.probed, s.probeErr
}

// probeLoop keeps the published descriptor current, so a deployment repaired, upgraded or
// broken while the service runs is presented as it is now without a restart. The endpoint
// itself still starts no PHP: it reads the answer this loop published (docs/adr/0014).
func (s *Service) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(s.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeOnce(ctx)
		}
	}
}

// probeOnce re-reads the application's capabilities and publishes that answer in place of the
// one it replaces. Only the descriptor changes: the launch preflight is a decision about workers
// that are already running, so a contradiction found here is published, never enforced.
//
// A read that fails is debounced. One failure keeps the last answer, because a single PHP run
// can fail for reasons that have nothing to do with the deployment, and taking every control
// away over it would be a worse lie than the answer a moment ago; consecutive failures are the
// application failing to answer, and those are published as the read-only state they are. A read
// that answers at all — including one with a schema this service does not know — resets the
// count, because the application is answering.
func (s *Service) probeOnce(ctx context.Context) {
	caps, err := s.probe.Capabilities(ctx)

	s.capsMu.Lock()
	before, beforeErr := s.probed, s.probeErr
	beforeContradiction := s.contradiction
	publish := true
	if err != nil {
		s.probeFailures++
		// An already unreadable descriptor is republished with the newest reason: there is no
		// good answer to protect, and the reason is the operator's only signal.
		publish = beforeErr != nil || s.probeFailures >= probeFailureThreshold
	} else {
		s.probeFailures = 0
	}
	if publish {
		s.probed, s.probeErr = caps, err
		// Any answer proves the loop is running, including one this service cannot read: a
		// schema it does not understand is reported by `available`, and using it to withhold
		// this timestamp would make two different failures look alike (docs/adr/0014).
		if err == nil {
			s.lastRead = time.Now()
		}
		// The preflight ran once, at launch. An application that lowers a retry_after under a
		// worker's timeout while the pool runs is the same contradiction, found late: the
		// answer is published with the new descriptor instead of waiting for a restart
		// (docs/adr/0014).
		s.contradiction = s.contradictionFor(caps, err)
	}
	after, afterErr := s.probed, s.probeErr
	failures := s.probeFailures
	contradiction := s.contradiction
	s.capsMu.Unlock()

	if !publish {
		s.log.Warn("application capabilities re-read failed; keeping the last answer",
			"application", s.appID, "failures", failures, "threshold", probeFailureThreshold, "error", err)
		return
	}
	if detail := capabilityTransition(before, after, beforeErr, afterErr); detail != "" {
		// No request and no user: the service noticed, and the line says what changed and why.
		s.auditor.Record(nil, audit.Event{
			Action:  audit.ActionCapabilityChange,
			Outcome: audit.OutcomeChanged,
			Detail:  detail,
		})
	}
	if contradiction != beforeContradiction {
		if contradiction == "" {
			s.log.Info("worker launch contradiction cleared; the pool keeps running", "application", s.appID)
		} else {
			s.log.Warn("worker launch contradicts the application's capabilities; the pool keeps running",
				"application", s.appID, "contradiction", contradiction)
		}
	}
	if sameCapabilityAnswer(before, beforeErr, after, afterErr) {
		return
	}
	switch {
	case err != nil:
		s.log.Warn("application capabilities unavailable; the deployment is presented as read-only",
			"application", s.appID, "failures", failures, "error", err)
	case !caps.Supported():
		s.log.Warn("application capabilities schema not understood; the deployment is presented as read-only",
			"application", s.appID, "schema", caps.Schema, "understood", adapter.SchemaVersion)
	default:
		s.log.Info("application capabilities re-read; the controls follow the application",
			"application", s.appID, "schema", caps.Schema, "package", caps.Package)
	}
}

// capabilityTransition is what an operator would look for later, or "" when the two answers
// describe the same deployment: the readable state, the reason it is not readable, or a package
// upgrade under a readable one. A schema change only happens across that boundary, and the
// read-only reason already names both numbers. A reason that varies while the deployment stays
// read-only is left to the log: the audit line records when it went read-only and why it was
// then, not every wording of a failing command's output (docs/adr/0014).
func capabilityTransition(before, after adapter.Capabilities, beforeErr, afterErr error) string {
	wasReadable := beforeErr == nil && before.Supported()
	isReadable := afterErr == nil && after.Supported()

	switch {
	case wasReadable && !isReadable:
		return "presented as read-only: " + after.Reason(afterErr)
	case !wasReadable && isReadable:
		return fmt.Sprintf("readable again (schema %d, package %s %s)",
			after.Schema, after.Package, after.PackageVersion)
	case wasReadable && isReadable:
		if before.Package != after.Package || before.PackageVersion != after.PackageVersion {
			return fmt.Sprintf("package %s %s -> %s %s",
				before.Package, before.PackageVersion, after.Package, after.PackageVersion)
		}
	}
	return ""
}

// contradictionFor is what the current answer contradicts in the launch configuration, or "" when
// it contradicts nothing. It is the launch preflight's own check, so the sentence a monitor reads
// is the one a refused launch would have printed; an answer this service cannot read claims
// nothing and is not a contradiction (docs/adr/0014).
func (s *Service) contradictionFor(caps adapter.Capabilities, err error) string {
	if err != nil || !caps.Supported() || s.cfg == nil {
		return ""
	}
	app := s.cfg.Application()
	if app == nil {
		return ""
	}
	if checkErr := adapter.Check(&caps, app); checkErr != nil {
		return checkErr.Error()
	}
	return ""
}

// unixSeconds is a timestamp for the health body, 0 for a time that never happened so the field
// stays out of the document.
func unixSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// sameCapabilityAnswer reports whether two reads would be published identically, so a loop that
// runs every interval records a change once instead of a line per read.
func sameCapabilityAnswer(before adapter.Capabilities, beforeErr error, after adapter.Capabilities, afterErr error) bool {
	if (beforeErr == nil) != (afterErr == nil) {
		return false
	}
	if beforeErr != nil && beforeErr.Error() != afterErr.Error() {
		return false
	}
	return before.Schema == after.Schema && before.Package == after.Package
}

// FailedJobs lists the application's own failed jobs for the dashboard. It is a read through
// the same adapter retry and delete use, so the screen names the records those actions can
// actually act on rather than the pruned history GoHorizon observed (docs/adr/0013).
func (s *Service) FailedJobs(ctx context.Context, query adapter.FailedJobQuery) (adapter.FailedJobsPage, error) {
	if s.failer == nil {
		return adapter.FailedJobsPage{}, errors.New("the application adapter is unavailable")
	}
	return s.failer.FailedJobs(ctx, query)
}

// RetryFailedJob asks the application to push one failed job back onto its queue. The
// application's own command is the only thing that touches its failed record or its payload,
// so this is delegation rather than a second implementation of retry (docs/adr/0012). A
// history store that never opened is not consulted here: the caller decides whether an
// identity is worth asking about.
func (s *Service) RetryFailedJob(ctx context.Context, id string) error {
	if s.failer == nil {
		return errors.New("the application adapter is unavailable")
	}
	if err := s.failer.RetryFailedJob(ctx, id); err != nil {
		return err
	}
	s.log.Info("failed job retry requested", "job", id)
	return nil
}

// ForgetFailedJob asks the application to delete one failed job, through the same adapter as a
// retry: the failed record belongs to the application (docs/adr/0012).
func (s *Service) ForgetFailedJob(ctx context.Context, id string) error {
	if s.failer == nil {
		return errors.New("the application adapter is unavailable")
	}
	if err := s.failer.ForgetFailedJob(ctx, id); err != nil {
		return err
	}
	s.log.Info("failed job delete requested", "job", id)
	return nil
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

	now := time.Now()
	// Rollups run before pruning: pruning the minutes first would leave holes in the
	// hours and days that are built from them. A failed rollup skips this prune pass
	// rather than risking that, and the next pass retries (docs/adr/0007).
	if err := s.store.Rollup(pruneCtx, now); err != nil {
		s.log.Warn("metrics rollup failed; skipping this prune pass", "error", err)
		return
	}

	pruned, err := s.store.Prune(pruneCtx, now)
	if err != nil {
		s.log.Warn("metrics retention pruning failed", "error", err)
		return
	}
	if pruned.Events > 0 || pruned.Jobs > 0 || pruned.Minutes > 0 || pruned.Hourly > 0 || pruned.Daily > 0 {
		s.log.Info("pruned metrics history", "events", pruned.Events, "jobs", pruned.Jobs,
			"minutes", pruned.Minutes, "hours", pruned.Hourly, "days", pruned.Daily)
	}
}

// metricsRetention maps the configured windows onto the store's durations.
func metricsRetention(cfg *config.Config) metrics.Retention {
	retention := cfg.Storage.Retention
	return metrics.Retention{
		Raw:    time.Duration(retention.RawHours) * time.Hour,
		Minute: time.Duration(retention.MinuteDays) * 24 * time.Hour,
		Hourly: time.Duration(retention.HourlyDays) * 24 * time.Hour,
		Daily:  time.Duration(retention.DailyDays) * 24 * time.Hour,
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
	// A payload this service cannot read is refused before the command runs: every tick would
	// otherwise start PHP to decode an answer it already knows it cannot use. The descriptor's
	// reason is published as the queue error instead, and the next tick after a repair reads
	// again (docs/adr/0014).
	if reason := s.inspectRefusal(); reason != "" {
		s.publishQueueError(errors.New(reason),
			"queue depth withheld; the application's inspect payload is not one this service reads")
		return
	}

	inspection, err := s.inspector.Queues(ctx, s.cfg.Application().RedisConnection, s.queues)
	if err != nil {
		s.publishQueueError(err, "queue depth unavailable; the application's inspection command failed")
		return
	}

	depths := make([]health.QueueDepth, 0, len(inspection.Queues))
	for _, queue := range inspection.Queues {
		oldestPendingError := ""
		if queue.OldestPendingError != nil {
			oldestPendingError = *queue.OldestPendingError
		}
		depths = append(depths, health.QueueDepth{
			Queue:              queue.Queue,
			Pending:            queue.Pending,
			Reserved:           queue.Reserved,
			Delayed:            queue.Delayed,
			OldestPendingAt:    queue.OldestPendingAt,
			OldestPendingError: oldestPendingError,
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

// inspectRefusal is the descriptor's reason when queue depth must not be read at all, or "" when
// the read may go ahead. It is the same sentence the descriptor publishes for the withheld
// operation, so the queue screen and the API cannot tell two stories about one deployment.
func (s *Service) inspectRefusal() string {
	s.capsMu.RLock()
	caps := s.probed
	s.capsMu.RUnlock()
	return caps.InspectPayloadRefusal()
}

// publishQueueError records why there is no depth to publish, and logs it once per change: the
// loop runs every few seconds, so an unchanged reason is not news.
func (s *Service) publishQueueError(err error, message string) {
	s.mu.Lock()
	changed := s.queueDepth.Error != err.Error()
	s.queueDepth = health.Queues{Error: err.Error()}
	s.mu.Unlock()
	if changed {
		s.log.Warn(message, "error", err)
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
// the context is a graceful stop, not a kill. Ingestion outlives those workers by one
// final pass, so the event for a job that finishes during the stop is accounted for by
// the process that ran it.
func (s *Service) Run(ctx context.Context) error {
	acquired, err := s.client.AcquireSupervisorLock(ctx, s.owner, lockTTL)
	if err != nil {
		return err
	}
	if !acquired {
		holder, _ := s.client.SupervisorLockHolder(ctx)
		return fmt.Errorf("%w (owner %q)", ErrLocked, holder)
	}
	acquiredAt := time.Now()
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
	// lost carries a definitive lock loss. Redis that cannot answer is an outage, not a
	// loss, so only a compare-and-pexpire miss lands here.
	lost := make(chan error, 1)
	var lockLost error

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
	// It also does not share runCtx: a worker publishes its last job's event while it
	// exits, so ingestion has to outlive the workers to account for that job here.
	consumerCtx, stopConsumer := context.WithCancel(context.Background())
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		s.consumer.Run(consumerCtx)
		stats := s.consumer.Stats()
		s.log.Info("telemetry consumer stopped", "events", stats.Events,
			"duplicates", stats.Duplicates, "invalid", stats.Invalid, "errors", stats.Errors)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.renewLoop(runCtx, lost, acquiredAt)
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.probeLoop(runCtx)
	}()

	// A definitive lock loss fences this instance: it stops every worker through the
	// normal graceful path, so an in-flight job still finishes, and the pool is left to
	// whoever owns the lock (docs/adr/0004). Cancelling runCtx is that graceful stop.
	select {
	case <-ctx.Done():
	case lockLost = <-lost:
		s.log.Error("supervisor lock lost; stopping this instance's workers so the pool has one owner",
			"owner", s.owner, "error", lockLost)
		cancel()
	}

	wg.Wait()
	close(errs)

	// The workers are gone, so everything they published is already in the stream. One
	// last pass records the job a graceful stop let finish before this process ends;
	// without it the next start would be the first to read that job's event.
	stopConsumer()
	<-consumerDone
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), telemetryDrainTimeout)
	s.consumer.Drain(drainCtx)
	cancelDrain()

	// The lock loss explains the exit, so it outranks a manager error that the same
	// shutdown produced.
	if lockLost != nil {
		return lockLost
	}
	for err := range errs {
		return err
	}
	return nil
}

// renewLoop keeps the ownership observable and fences when ownership is lost, either
// because Redis answered that the key is no longer ours or because it has not confirmed a
// renewal for longer than the lock could possibly survive. A shorter silence only degrades
// health: Redis may be unreachable, and stopping healthy workers over a telemetry outage
// would break handoff §2.11. Confirmed ownership restarts the clock.
func (s *Service) renewLoop(ctx context.Context, lost chan<- error, confirmedAt time.Time) {
	ticker := time.NewTicker(s.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.Background(), lockTimeout)
			held, err := s.renewLock(renewCtx, s.owner, lockTTL)
			var holder string
			if err == nil && !held {
				// Only used to say who owns the pool now; the failure to read it is not
				// itself a reason to change the decision.
				holder, _ = s.client.SupervisorLockHolder(renewCtx)
			}
			cancel()

			now := time.Now()
			switch {
			case err != nil:
				// No answer is not proof by itself, but the key expires without a renewal,
				// so past the allowance the lock is gone whether or not Redis speaks.
				if shouldFence(confirmedAt, now, s.fenceAfter) {
					s.fence(lost, fmt.Errorf("renewal has not confirmed ownership for %s",
						now.Sub(confirmedAt).Round(time.Second)))
					return
				}
				s.log.Warn("supervisor lock renewal failed; supervision continues, telemetry is degraded", "error", err)
				s.setLock(false, err.Error())
			case !held:
				reason := errLockKeyGone
				if holder != "" {
					reason = fmt.Errorf("the lock is now held by %q", holder)
				}
				s.fence(lost, reason)
				return
			default:
				confirmedAt = now
				s.setLock(true, "")
			}
		}
	}
}

// fence tells Run that this instance no longer owns the pool, so its workers are stopped
// gracefully instead of sharing it with whoever owns the lock now.
func (s *Service) fence(lost chan<- error, reason error) {
	fenced := fmt.Errorf("%w (%v)", ErrLockLost, reason)
	s.setLock(false, fenced.Error())
	// Never blocks the loop's exit: Run may already be stopping for a signal.
	select {
	case lost <- fenced:
	default:
	}
}

// shouldFence reports whether renewals have gone unconfirmed long enough that the lock
// cannot still be ours. The allowance is one TTL plus slack for a renewal that was in
// flight when Redis stopped answering.
func shouldFence(confirmedAt, now time.Time, allowance time.Duration) bool {
	return !now.Before(confirmedAt.Add(allowance))
}

func (s *Service) setLock(held bool, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockHeld = held
	s.lockErr = lastErr
}

// unservedQueues returns the queues that get no worker slot. A manager assigns
// queues[slot % len(queues)], so slots 0..workers-1 cover the first workers queues and the
// rest are only reached once the pool is at least as long as the list.
func unservedQueues(queues []string, workers int) []string {
	if workers < 0 {
		workers = 0
	}
	if workers >= len(queues) {
		return nil
	}
	return queues[workers:]
}

// warnIfShortPool logs one startup warning naming the queues this supervisor will never
// launch a worker for. It is a warning rather than an error because a queue may
// legitimately be served by another supervisor or by Laravel itself, but a pool that
// silently serves half its list is a configuration mistake nobody would notice
// (docs/adr/0005).
func warnIfShortPool(log *slog.Logger, sup *config.Supervisor) {
	unserved := unservedQueues(sup.Queues, sup.Workers.Min)
	if len(unserved) == 0 {
		return
	}
	log.Warn("pool is shorter than its queue list; these queues get no worker of their own",
		"supervisor", sup.ID, "workers", sup.Workers.Min, "queues", len(sup.Queues),
		"unserved", strings.Join(unserved, ","))
}

// Report builds the health payload.
func (s *Service) Report() health.Report {
	s.mu.Lock()
	held, lockErr := s.lockHeld, s.lockErr
	queues := s.queueDepth
	s.mu.Unlock()

	// The same answer the endpoint publishes, read once so a scrape cannot report a descriptor
	// and a streak from different reads.
	s.capsMu.RLock()
	caps, capsErr := s.probed, s.probeErr
	capsFailures := s.probeFailures
	contradiction := s.contradiction
	lastRead := s.lastRead
	s.capsMu.RUnlock()

	app := s.cfg.Application()
	report := health.Report{
		Status:        "ok",
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Lock:          health.Lock{Held: held, Degraded: lockErr != "", LastError: lockErr},
		Queues:        queues,
		Capabilities: health.Capabilities{
			Available:     capsErr == nil && caps.Supported(),
			Schema:        caps.Schema,
			FailedReads:   capsFailures,
			Contradiction: contradiction,
			LastReadAt:    unixSeconds(lastRead),
		},
	}
	if !report.Capabilities.Available {
		report.Capabilities.Error = caps.Reason(capsErr)
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
				CrashLooping:   worker.CrashLooping,
				LastExitCode:   worker.LastExitCode,
				LastExitReason: worker.LastExitReason,
				Adopted:        worker.Adopted,
			})
		}
		report.Supervisors = append(report.Supervisors, sup)
	}
	return report
}
