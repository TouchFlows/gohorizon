<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Inspection\QueueInspector;
use Illuminate\Contracts\Queue\Factory;
use Illuminate\Contracts\Queue\Queue;
use PHPUnit\Framework\TestCase;

final class QueueInspectorTest extends TestCase
{
    public function test_it_reads_counts_through_the_queue_contract(): void
    {
        $queue = new FakeQueue(['default' => 3, 'emails' => 7], ['default' => 1], ['default' => 2]);
        $inspector = new QueueInspector(new FakeQueueFactory($queue));

        $rows = $inspector->inspect('redis', ['default', 'emails']);

        self::assertSame([
            'queue' => 'default',
            'driver' => 'FakeQueue',
            'pending' => 3,
            'delayed' => 2,
            'reserved' => 1,
            'size' => 3,
            'oldest_pending_at' => 1700000000,
        ], $rows[0]);
        self::assertSame(7, $rows[1]['pending']);
    }

    	public function test_a_driver_that_fails_a_count_reports_it_and_keeps_the_rest(): void
	{
		$queue = new ThrowingQueue(['default' => 3], ['default' => 1], ['default' => 2]);
		$inspector = new QueueInspector(new FakeQueueFactory($queue));

		$rows = $inspector->inspect('weird', ['default']);

		self::assertArrayHasKey('pending_error', $rows[0]);
		self::assertArrayHasKey('size_error', $rows[0]);
		self::assertSame(2, $rows[0]['delayed']);
		self::assertSame(1, $rows[0]['reserved']);
	}
}

final class FakeQueueFactory implements Factory
{
    public function __construct(private readonly Queue $connection)
    {
    }

    public function connection($name = null)
    {
        return $this->connection;
    }
}

/**
 * Implements only the methods QueueInspector probes, plus the contract surface.
 */
class FakeQueue implements Queue
{
    /** @param array<string, int> $pending */
    /** @param array<string, int> $reserved */
    /** @param array<string, int> $delayed */
    public function __construct(
        private readonly array $pending,
        private readonly array $reserved,
        private readonly array $delayed,
    ) {
    }

    public function pendingSize($queue = null)
    {
        return $this->pending[$queue] ?? 0;
    }

    public function delayedSize($queue = null)
    {
        return $this->delayed[$queue] ?? 0;
    }

    public function reservedSize($queue = null)
    {
        return $this->reserved[$queue] ?? 0;
    }

    public function size($queue = null)
    {
        return $this->pendingSize($queue);
    }

    public function creationTimeOfOldestPendingJob($queue = null)
    {
        return 1700000000;
    }

    public function push($job, $data = '', $queue = null) {}

    public function pushOn($queue, $job, $data = '') {}

    public function pushRaw($payload, $queue = null, array $options = []) {}

    public function later($delay, $job, $data = '', $queue = null) {}

    public function laterOn($queue, $delay, $job, $data = '') {}

    public function bulk($jobs, $data = '', $queue = null) {}

    public function pop($queue = null) {}

    public function getConnectionName()
    {
        return 'fake';
    }

    public function setConnectionName($name)
    {
        return $this;
    }
}

final class ThrowingQueue extends FakeQueue
{
    public function pendingSize($queue = null)
    {
        throw new \RuntimeException('driver cannot count pending jobs');
    }

    public function size($queue = null)
    {
        throw new \RuntimeException('driver cannot report size');
    }
}
