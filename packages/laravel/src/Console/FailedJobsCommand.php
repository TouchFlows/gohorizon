<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Console;

use GoHorizon\Laravel\Inspection\FailedJobLister;
use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Console\Command;
use InvalidArgumentException;

/**
 * Prints the application's own failed jobs as JSON for the Go control plane.
 *
 * The GoHorizon job history is an observation that is pruned; this is the store Laravel
 * actually retries and forgets from, so the Failed screen reads the same source the two
 * actions act on (docs/adr/0013).
 */
final class FailedJobsCommand extends Command
{
    /**
     * The version of the payload this command prints, reported by `gohorizon:capabilities` so
     * the service can compare it with the version it reads (docs/adr/0014).
     */
    public const SCHEMA = 1;

    protected $signature = 'gohorizon:failed:list
                            {--limit=50 : Records to return, 1-200}
                            {--before= : Continue after the next_before of a previous page}
                            {--json : Output JSON}';

    protected $description = "List the application's own failed jobs through Laravel's failer";

    public function __construct(
        private readonly FailedJobLister $failedJobs,
        private readonly Keys $keys,
    ) {
        parent::__construct();
    }

    public function handle(): int
    {
        $rawLimit = (string) $this->option('limit');
        if (! ctype_digit($rawLimit) || (int) $rawLimit < 1 || (int) $rawLimit > FailedJobLister::MAX_LIMIT) {
            $this->error('limit must be between 1 and '.FailedJobLister::MAX_LIMIT);

            return self::INVALID;
        }

        $before = $this->option('before');
        $before = is_string($before) && trim($before) !== '' ? trim($before) : null;

        try {
            $page = $this->failedJobs->page((int) $rawLimit, $before);
        } catch (InvalidArgumentException) {
            $this->error('before must be the next_before of a previous page');

            return self::INVALID;
        }

        $payload = [
            'schema' => self::SCHEMA,
            'application' => $this->keys->application(),
            'environment' => $this->keys->environment(),
            'failed_jobs' => $page['failed_jobs'],
            'next_before' => $page['next_before'],
            'generated_at' => gmdate('Y-m-d\TH:i:s\Z'),
        ];

        if ($this->option('json')) {
            $this->line((string) json_encode($payload, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));

            return self::SUCCESS;
        }

        $this->table(
            ['id', 'queue', 'job', 'failed at', 'exception'],
            array_map(static fn (array $row) => [
                $row['id'],
                $row['queue'],
                $row['job'],
                $row['failed_at'],
                $row['exception'] ?? '-',
            ], $payload['failed_jobs'])
        );

        return self::SUCCESS;
    }
}
