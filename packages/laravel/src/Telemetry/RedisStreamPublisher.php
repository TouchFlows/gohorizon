<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Contracts\Redis\Factory;
use Illuminate\Redis\Connections\PredisConnection;
use Throwable;

/**
 * Publishes telemetry to the GoHorizon Redis Stream.
 *
 * Entries are trimmed approximately to `telemetry.retention`, so a consumer that is
 * down for a long time cannot grow the stream without limit.
 */
final class RedisStreamPublisher implements TelemetryPublisher
{
    public function __construct(
        private readonly Factory $redis,
        private readonly ?string $connection,
        private readonly Keys $keys,
        private readonly int $retention = 100000,
    ) {
    }

    public function publish(array $events): void
    {
        if ($events === []) {
            return;
        }

        $redis = $this->redis->connection($this->connection);
        $stream = $this->keys->events();

        foreach ($events as $event) {
            $this->xadd($redis, $stream, $this->encode($event));
        }
    }

    /**
     * XADD is issued as a raw command for two reasons.
     *
     * Laravel gives a Redis connection a key prefix by default (derived from APP_NAME),
     * while the Go consumer reads the literal `gohorizon:{application}:{environment}:`
     * stream key (docs/adr/0003). A normal command would land under a prefix GoHorizon
     * cannot know, so telemetry would silently never be consumed.
     *
     * Raw execution also avoids the client difference: PhpRedis takes
     * (key, id, fields, maxlen, approximate), Predis takes (key, fields, id, options).
     */
    private function xadd(object $connection, string $stream, string $payload): void
    {
        $command = ['XADD', $stream, 'MAXLEN', '~', (string) $this->retention, '*', 'payload', $payload];

        if ($connection instanceof PredisConnection) {
            $connection->client()->executeRaw($command);

            return;
        }

        $connection->executeRaw($command);
    }

    private function encode(array $event): string
    {
        try {
            return json_encode($event, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
        } catch (Throwable $e) {
            // Should be impossible for a scalar-only payload, but never let encoding
            // break the job that produced the event.
            return json_encode([
                'schema' => 1,
                'type' => 'telemetry.encoding_failed',
                'error' => $e::class,
            ], JSON_THROW_ON_ERROR);
        }
    }
}
