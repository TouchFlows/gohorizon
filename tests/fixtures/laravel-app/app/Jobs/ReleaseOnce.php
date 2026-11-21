<?php

declare(strict_types=1);

namespace App\Jobs;

use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Queue\Queueable;
use RuntimeException;

/**
 * Fails on every attempt with two tries allowed, so the first attempt is released for retry
 * and the second exceeds the limit and fails. That pair is how the acceptance test sees both
 * the `released` and the `failed` outcome from one job.
 */
final class ReleaseOnce implements ShouldQueue
{
    use Queueable;

    public int $tries = 2;

    public function handle(): void
    {
        throw new RuntimeException('the acceptance fixture releases once and then fails');
    }
}
