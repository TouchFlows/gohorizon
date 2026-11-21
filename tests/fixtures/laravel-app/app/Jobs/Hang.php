<?php

declare(strict_types=1);

namespace App\Jobs;

use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Queue\Queueable;

/**
 * Sleeps far past the worker's timeout, so Laravel reports `timed_out` before it kills the
 * worker for it. One try keeps the retry short: the next attempt already exceeded the limit,
 * so it fails instead of timing out again.
 */
final class Hang implements ShouldQueue
{
    use Queueable;

    public int $tries = 1;

    public function handle(): void
    {
        sleep(30);
    }
}
