<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use Illuminate\Queue\Failed\FailedJobProviderInterface;

/**
 * Stands in for the application's configured failer, so the listing and the command can be
 * read without a database or a file store.
 */
final class FakeFailedJobs implements FailedJobProviderInterface
{
    /**
     * @param list<object> $records
     */
    public function __construct(private readonly array $records)
    {
    }

    public function log($connection, $queue, $payload, $exception)
    {
    }

    public function all($queue = null)
    {
        return $this->records;
    }

    public function find($id)
    {
        return null;
    }

    public function forget($id)
    {
    }

    public function flush($hours = null)
    {
    }

    public function ids($queue = null)
    {
        return array_map(static fn (object $record) => (int) $record->id, $this->records);
    }
}
