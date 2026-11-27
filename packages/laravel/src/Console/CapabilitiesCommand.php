<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Console;

use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Console\Command;
use Illuminate\Contracts\Foundation\Application;
use Illuminate\Queue\Events\JobExceptionOccurred;
use Illuminate\Queue\Events\JobFailed;
use Illuminate\Queue\Events\JobProcessed;
use Illuminate\Queue\Events\JobProcessing;
use Illuminate\Queue\Events\JobReleasedAfterException;
use Illuminate\Queue\Events\JobTimedOut;
use Illuminate\Queue\Events\WorkerIdle;
use Illuminate\Queue\Events\WorkerInterrupted;
use Illuminate\Queue\Events\WorkerStarting;
use Illuminate\Queue\Events\WorkerStopping;

/**
 * Reports what this application actually supports.
 *
 * GoHorizon uses this instead of assuming every Laravel version exposes identical
 * functionality (handoff §6.4). No credential value is ever included.
 */
final class CapabilitiesCommand extends Command
{
    protected $signature = 'gohorizon:capabilities {--json : Output JSON}';

    protected $description = 'Report Laravel/GoHorizon capabilities to the Go control plane';

    public function __construct(
        private readonly Application $app,
        private readonly Keys $keys,
    ) {
        parent::__construct();
    }

    public function handle(): int
    {
        $capabilities = $this->capabilities();

        if ($this->option('json')) {
            $this->line((string) json_encode($capabilities, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));

            return self::SUCCESS;
        }

        $this->line('application: '.$capabilities['application']);
        $this->line('environment: '.$capabilities['environment']);
        $this->line('laravel: '.$capabilities['laravel']);
        $this->line('php: '.$capabilities['php']);
        $this->line('pcntl: '.($capabilities['pcntl'] ? 'yes' : 'no'));
        $this->line('events stream: '.$capabilities['telemetry']['stream']);

        return self::SUCCESS;
    }

    /**
     * @return array<string, mixed>
     */
    public function capabilities(): array
    {
        return [
            'schema' => 1,
            'package' => 'gohorizon/laravel',
            'laravel' => $this->app->version(),
            'php' => PHP_VERSION,
            'application' => $this->keys->application(),
            'environment' => $this->keys->environment(),
            'queue' => [
                'connections' => $this->queueConnections(),
            ],
            'telemetry' => [
                'stream' => $this->keys->events(),
            ],
            // The versions of this package's other payloads, so GoHorizon can compare them with
            // the ones it reads and withhold only the screen that would misread one. The numbers
            // live on the commands that print them, so they cannot drift apart from it
            // (docs/adr/0014).
            'payload_schemas' => [
                'inspect' => InspectQueuesCommand::SCHEMA,
                'failed_jobs' => FailedJobsCommand::SCHEMA,
            ],
            'runtime' => [
                // A --timeout is inert without pcntl, so GoHorizon refuses to spawn one.
                'pcntl' => extension_loaded('pcntl'),
            ],
            'events' => $this->availableEvents(),
        ];
    }

    /**
     * @return array<string, array<string, mixed>>
     */
    private function queueConnections(): array
    {
        $connections = [];

        foreach ((array) config('queue.connections', []) as $name => $connection) {
            $connections[(string) $name] = [
                'driver' => $connection['driver'] ?? null,
                // GoHorizon compares worker.timeout against this before spawning.
                'retry_after' => $connection['retry_after'] ?? null,
            ];
        }

        return $connections;
    }

    /**
     * @return list<string>
     */
    private function availableEvents(): array
    {
        $candidates = [
            JobProcessing::class,
            JobProcessed::class,
            JobExceptionOccurred::class,
            JobFailed::class,
            JobReleasedAfterException::class,
            JobTimedOut::class,
            WorkerStarting::class,
            WorkerStopping::class,
            WorkerInterrupted::class,
            WorkerIdle::class,
        ];

        return array_values(array_filter($candidates, static fn (string $class) => class_exists($class)));
    }
}
