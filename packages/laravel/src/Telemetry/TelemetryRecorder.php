<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Contracts\Queue\Job;
use Illuminate\Queue\Events\JobProcessing;
use Illuminate\Queue\Events\WorkerStopping;
use Illuminate\Support\Str;
use Throwable;

/**
 * Builds telemetry events and buffers them.
 *
 * Guarantees (docs/adr/0006-telemetry-schema.md):
 *  - one terminal event per job attempt;
 *  - no payloads, arguments or stack traces;
 *  - every failure is swallowed: telemetry never breaks a job;
 *  - the buffer is bounded and overflow is counted, not unbounded.
 */
final class TelemetryRecorder
{
    public const SCHEMA = 1;

    /** @var list<array<string, mixed>> */
    private array $buffer = [];

    /** @var array<string, float> Start time per in-flight job key. */
    private array $inFlight = [];

    private int $dropped = 0;

    public function __construct(
        private readonly TelemetryPublisher $publisher,
        private readonly Keys $keys,
        private readonly string $workerName = 'unknown',
        private readonly bool $tagsEnabled = true,
        private readonly int $batchSize = 100,
        private readonly int $bufferSize = 1000,
    ) {
        if ($this->batchSize < 1) {
            throw new \InvalidArgumentException('telemetry batch size must be at least 1');
        }
        if ($this->bufferSize < $this->batchSize) {
            throw new \InvalidArgumentException('telemetry buffer size must be at least the batch size');
        }
    }

    public function jobStarted(JobProcessing $event): void
    {
        try {
            $this->inFlight[$this->jobKey($event->job)] = microtime(true);
        } catch (Throwable) {
            // Best effort only.
        }
    }

    public function jobFinished(Job $job, string $outcome, ?Throwable $exception = null, array $extra = []): void
    {
        try {
            $key = $this->jobKey($job);
            $started = $this->inFlight[$key] ?? null;
            unset($this->inFlight[$key]);

            $payload = [
                'schema' => self::SCHEMA,
                'id' => (string) Str::uuid(),
                'type' => 'job.finished',
                'application' => $this->keys->application(),
                'environment' => $this->keys->environment(),
                'worker' => $this->workerName,
                'at' => self::timestamp(),
                'job' => array_filter([
                    'uuid' => $this->jobUuid($job),
                    'id' => (string) $job->getJobId(),
                    'class' => (string) $job->resolveName(),
                    'connection' => (string) $job->getConnectionName(),
                    'queue' => (string) $job->getQueue(),
                    'attempts' => (int) $job->attempts(),
                    'outcome' => $outcome,
                    'duration_ms' => $started === null ? null : (int) round((microtime(true) - $started) * 1000),
                    'exception' => $exception === null ? null : [
                        'class' => $exception::class,
                        'message' => Sanitizer::message($exception),
                    ],
                ] + $extra, static fn ($value) => $value !== null),
                'tags' => $this->tagsEnabled ? $this->tags($job) : [],
            ];

            $this->enqueue($payload);
        } catch (Throwable) {
            $this->dropped++;
        }
    }

    public function workerStopping(WorkerStopping $event): void
    {
        try {
            $reason = $event->reason;

            $this->enqueue([
                'schema' => self::SCHEMA,
                'id' => (string) Str::uuid(),
                'type' => 'worker.stopped',
                'application' => $this->keys->application(),
                'environment' => $this->keys->environment(),
                'worker' => $this->workerName,
                'at' => self::timestamp(),
                'worker_event' => array_filter([
                    'exit_code' => (int) $event->status,
                    'reason' => $reason?->value,
                    'connection' => $event->connectionName,
                    'queue' => $event->queue,
                    'jobs_processed' => $event->jobsProcessed,
                    'memory_mb' => $event->memoryUsage === null ? null : round((float) $event->memoryUsage, 1),
                ], static fn ($value) => $value !== null),
            ]);
        } catch (Throwable) {
            $this->dropped++;
        } finally {
            $this->flush();
        }
    }

    /**
     * Publishes everything buffered. Never throws.
     */
    public function flush(): void
    {
        if ($this->buffer === []) {
            return;
        }

        $batch = $this->buffer;
        $this->buffer = [];

        try {
            $this->publisher->publish($batch);
        } catch (Throwable) {
            $this->dropped += count($batch);
        }
    }

    public function dropped(): int
    {
        return $this->dropped;
    }

    private function enqueue(array $event): void
    {
        while (count($this->buffer) >= $this->bufferSize) {
            array_shift($this->buffer);
            $this->dropped++;
        }

        $this->buffer[] = $event;

        if (count($this->buffer) >= $this->batchSize) {
            $this->flush();
        }
    }

    private function jobKey(Job $job): string
    {
        $uuid = $this->jobUuid($job);

        return $uuid ?? 'id:'.$job->getJobId();
    }

    private function jobUuid(Job $job): ?string
    {
        try {
            if (method_exists($job, 'uuid') && $job->uuid() !== null) {
                return (string) $job->uuid();
            }
        } catch (Throwable) {
            // Some drivers cannot provide a UUID.
        }

        return null;
    }

    /**
     * Tags exist only when a job implements the Horizon-style tags() method.
     *
     * @return list<string>
     */
    private function tags(Job $job): array
    {
        try {
            if (! method_exists($job, 'getResolvedJob')) {
                return [];
            }
            $instance = $job->getResolvedJob();
            if ($instance !== null && method_exists($instance, 'tags')) {
                $tags = array_values(array_filter((array) $instance->tags(), 'is_string'));

                return array_slice($tags, 0, 20);
            }
        } catch (Throwable) {
            // Tags are optional.
        }

        return [];
    }

    private static function timestamp(): string
    {
        return (new \DateTimeImmutable('now', new \DateTimeZone('UTC')))->format('Y-m-d\TH:i:s.v\Z');
    }
}
