<?php

declare(strict_types=1);

namespace App\Jobs;

use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Queue\Queueable;
use RuntimeException;

/**
 * Always fails, so the tests can prove a failed job reaches telemetry and the metrics
 * endpoint instead of only succeeding ones.
 */
final class AlwaysFails implements ShouldQueue
{
    use Queueable;

    public function handle(): void
    {
        throw new RuntimeException('the acceptance fixture always fails');
    }
}
