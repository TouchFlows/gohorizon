<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Inspection;

use Illuminate\Contracts\Queue\Factory as QueueFactory;
use Illuminate\Contracts\Queue\Queue;
use Throwable;

/**
 * Queue inspection through the Laravel 13 Queue contract.
 *
 * Laravel 13 added pendingSize/delayedSize/reservedSize/creationTimeOfOldestPendingJob
 * to the contract, so none of this needs to know the Redis key layout
 * (docs/adr/0001-inspection-via-adapter.md).
 */
final class QueueInspector
{
    public function __construct(private readonly QueueFactory $queues)
    {
    }

    /**
     * @param list<string> $queues Empty means the connection's configured queues.
     *
     * @return list<array<string, mixed>>
     */
    public function inspect(?string $connection, array $queues): array
    {
        $queueConnection = $this->queues->connection($connection);

        if ($queues === []) {
            $queues = $this->configuredQueues($connection);
        }

        $rows = [];
        foreach ($queues as $queue) {
            $rows[] = $this->inspectQueue($queueConnection, $queue);
        }

        return $rows;
    }

    public function connectionName(?string $connection): string
    {
        return $connection ?: (string) config('queue.default');
    }

    /**
     * @return list<string>
     */
    private function configuredQueues(?string $connection): array
    {
        $name = $this->connectionName($connection);
        $configured = config("queue.connections.{$name}.queue", 'default');

        if (is_string($configured)) {
            $configured = explode(',', $configured);
        }

        $queues = [];
        foreach ((array) $configured as $queue) {
            $queue = trim((string) $queue);
            if ($queue !== '') {
                $queues[] = $queue;
            }
        }

        return $queues === [] ? ['default'] : $queues;
    }

    /**
     * @return array<string, mixed>
     */
    private function inspectQueue(Queue $connection, string $queue): array
    {
        $row = [
            'queue' => $queue,
            'driver' => class_basename($connection),
        ];

        $methods = [
            'pendingSize' => 'pending',
            'delayedSize' => 'delayed',
            'reservedSize' => 'reserved',
            'size' => 'size',
        ];

        foreach ($methods as $method => $key) {
            try {
                if (method_exists($connection, $method)) {
                    $row[$key] = (int) $connection->{$method}($queue);
                }
            } catch (Throwable $e) {
                $row[$key.'_error'] = $e::class;
            }
        }

        try {
            if (method_exists($connection, 'creationTimeOfOldestPendingJob')) {
                $row['oldest_pending_at'] = $connection->creationTimeOfOldestPendingJob($queue);
            }
        } catch (Throwable $e) {
            $row['oldest_pending_error'] = $e::class;
        }

        return $row;
    }
}
