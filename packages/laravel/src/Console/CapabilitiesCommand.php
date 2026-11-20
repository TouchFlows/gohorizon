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
        $defaultConnection = (string) config('queue.default');
        $redisConnection = (string) config('gohorizon.redis.connection', 'default');

        return [
            'schema' => 1,
            'package' => 'gohorizon/laravel',
            'package_version' => '0.1.0',
            'laravel' => $this->app->version(),
            'php' => PHP_VERSION,
            'application' => $this->keys->application(),
            'environment' => $this->keys->environment(),
            'queue' => [
                'default_connection' => $defaultConnection,
                'connections' => $this->queueConnections(),
            ],
            'redis' => $this->redisDescription($redisConnection),
            'telemetry' => [
                'enabled' => (bool) config('gohorizon.enabled', true) && (bool) config('gohorizon.telemetry.enabled', true),
                'stream' => $this->keys->events(),
                'batch_size' => (int) config('gohorizon.telemetry.batch_size', 100),
                'buffer_size' => (int) config('gohorizon.telemetry.buffer_size', 1000),
            ],
            'runtime' => [
                'pcntl' => extension_loaded('pcntl'),
                'posix' => extension_loaded('posix'),
                'sapi' => PHP_SAPI,
            ],
            'events' => $this->availableEvents(),
            'operations' => [
                'inspect_queues',
                'telemetry',
                'capabilities',
            ],
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
                'queue' => $connection['queue'] ?? null,
                // GoHorizon compares worker.timeout against this before spawning.
                'retry_after' => $connection['retry_after'] ?? null,
                'block_for' => $connection['block_for'] ?? null,
                'after_commit' => (bool) ($connection['after_commit'] ?? false),
                'has_password' => ! empty($connection['password']),
            ];
        }

        return $connections;
    }

    /**
     * @return array<string, mixed>
     */
    private function redisDescription(string $connection): array
    {
        $config = (array) config("database.redis.{$connection}", []);

        return [
            'connection' => $connection,
            'client' => config('database.redis.client'),
            'host' => $config['host'] ?? null,
            'port' => isset($config['port']) ? (int) $config['port'] : null,
            'database' => isset($config['database']) ? (int) $config['database'] : null,
            'has_password' => ! empty($config['password']),
            'has_username' => ! empty($config['username']),
        ];
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
