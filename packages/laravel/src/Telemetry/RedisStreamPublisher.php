<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Contracts\Redis\Factory;
use Illuminate\Redis\Connections\Connection;
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

        if (count($events) === 1) {
            $this->add($redis, $stream, $events[0]);

            return;
        }

        $redis->pipeline(function ($pipe) use ($events, $stream): void {
            foreach ($events as $event) {
                $pipe->xadd($stream, '*', ['payload' => $this->encode($event)], $this->retention, true);
            }
        });
    }

    private function add(Connection $redis, string $stream, array $event): void
    {
        $redis->xadd($stream, '*', ['payload' => $this->encode($event)], $this->retention, true);
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
