<?php

declare(strict_types=1);

namespace App\Jobs;

use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Queue\Queueable;
use RuntimeException;

/**
 * Appends one line to the given file. The acceptance tests use the file as proof that a
 * real Laravel worker under GoHorizon's supervision executed the job.
 */
final class RecordMarker implements ShouldQueue
{
    use Queueable;

    public function __construct(public readonly string $path)
    {
    }

    public function handle(): void
    {
        if (file_put_contents($this->path, "ran\n", FILE_APPEND | LOCK_EX) === false) {
            throw new RuntimeException('could not write the marker file');
        }
    }
}
