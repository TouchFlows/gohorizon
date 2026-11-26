<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Inspection;

use GoHorizon\Laravel\Telemetry\Sanitizer;
use Illuminate\Queue\Failed\FailedJobProviderInterface;

/**
 * Lists the application's own failed jobs through Laravel's failer.
 *
 * The failed record — and the payload behind it — belongs to the application, so this
 * reads it through the failer interface rather than from the table or a queue key
 * (docs/adr/0001, docs/adr/0013). The payload itself never leaves the application: only
 * the job class Laravel recorded in it is read, and the stored exception is reduced to a
 * sanitised first line.
 *
 * Records are ordered by their provider id, newest first, and a page continues before an
 * id. Both built-in failers (database and file) allocate ids in increasing order, which is
 * the only ordering the interface itself offers.
 */
final class FailedJobLister
{
    public const DEFAULT_LIMIT = 50;

    public const MAX_LIMIT = 200;

    public function __construct(private readonly FailedJobProviderInterface $failer)
    {
    }

    /**
     * @return array{failed_jobs: list<array<string, mixed>>, next_before: string|null}
     */
    public function page(int $limit, ?string $before = null): array
    {
        $limit = max(1, min(self::MAX_LIMIT, $limit));
        $cursor = $before === null ? null : (int) $before;

        $rows = [];
        foreach ($this->failer->all() as $record) {
            $row = $this->row($record);
            if ($row === null) {
                continue;
            }
            if ($cursor !== null && $row['sort'] >= $cursor) {
                continue;
            }
            $rows[] = $row;
        }

        usort($rows, static fn (array $a, array $b): int => $b['sort'] <=> $a['sort']);

        $next = null;
        if (count($rows) > $limit) {
            $rows = array_slice($rows, 0, $limit);
            $next = $rows[count($rows) - 1]['id'];
        }

        return [
            'failed_jobs' => array_map(static function (array $row): array {
                unset($row['sort']);

                return $row;
            }, $rows),
            'next_before' => $next,
        ];
    }

    /**
     * @return array{id: string, uuid: string, connection: string, queue: string, job: string, failed_at: string, exception: string|null, sort: int}|null
     */
    private function row(mixed $record): ?array
    {
        $record = (array) $record;
        $id = $record['id'] ?? null;

        // An id is what the next page continues before, so a record without one cannot be
        // paged and is left out rather than returned with a cursor nobody can use.
        if ($id === null || $id === '' || ! is_numeric($id)) {
            return null;
        }

        return [
            'id' => (string) $id,
            'uuid' => (string) ($record['uuid'] ?? ''),
            'connection' => (string) ($record['connection'] ?? ''),
            'queue' => (string) ($record['queue'] ?? ''),
            'job' => $this->jobClass($record['payload'] ?? null),
            'failed_at' => (string) ($record['failed_at'] ?? ''),
            'exception' => $this->exception($record['exception'] ?? null),
            'sort' => (int) $id,
        ];
    }

    /**
     * The payload is Laravel's serialised job envelope. Only the class name is taken from
     * it; its arguments are never read, copied or returned (handoff §6.2).
     */
    private function jobClass(mixed $payload): string
    {
        if (! is_string($payload) || $payload === '') {
            return '';
        }

        $decoded = json_decode($payload, true);
        if (! is_array($decoded)) {
            return '';
        }

        $name = $decoded['displayName'] ?? $decoded['job'] ?? '';

        return is_string($name) ? $name : '';
    }

    /**
     * The failer stores the whole throwable as a stack trace; the list needs one sanitised
     * line, not the trace and not any credential the message happens to carry.
     */
    private function exception(mixed $exception): ?string
    {
        if (! is_string($exception) || trim($exception) === '') {
            return null;
        }

        $firstLine = explode("\n", $exception, 2)[0];

        return Sanitizer::text($firstLine);
    }
}
