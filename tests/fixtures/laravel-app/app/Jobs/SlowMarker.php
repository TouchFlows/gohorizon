<?php

declare(strict_types=1);

namespace App\Jobs;

use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Queue\Queueable;
use RuntimeException;

/**
 * A job that is deliberately slow, so the acceptance tests can signal a worker while
 * the job is in flight. It records when it starts and when it finishes, which is how a
 * test separates "ran once" from "ran again after the worker died".
 */
final class SlowMarker implements ShouldQueue
{
    use Queueable;

    /**
     * The worker is launched with --tries=1. This job allows a second attempt because a
     * worker killed mid-job leaves the job reserved, and Laravel only replays it once
     * the reservation expires — that replay counts as attempt 2.
     */
    public int $tries = 3;

    public function __construct(
        public readonly string $path,
        public readonly int $seconds = 3,
    ) {
    }

    public function handle(): void
    {
        $this->append($this->path, 'started');

        // sleep() returns as soon as a signal arrives, which would make a graceful stop
        // look instantaneous. Waiting in slices keeps the job busy for its whole
        // duration, so SIGTERM really arrives while the job is running.
        $until = microtime(true) + $this->seconds;
        while (($left = $until - microtime(true)) > 0) {
            usleep((int) min(50_000, max(1_000, $left * 1_000_000)));
        }

        $this->append($this->path, 'ran');
    }

    private function append(string $file, string $line): void
    {
        if (file_put_contents($file, $line."\n", FILE_APPEND | LOCK_EX) === false) {
            throw new RuntimeException('could not write the marker file');
        }
    }
}
