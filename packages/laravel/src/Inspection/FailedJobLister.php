<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Inspection;

use GoHorizon\Laravel\Telemetry\Sanitizer;
use Illuminate\Queue\Failed\FailedJobProviderInterface;
use InvalidArgumentException;

/**
 * Lists the application's own failed jobs through Laravel's failer.
 *
 * The failed record — and the payload behind it — belongs to the application, so this
 * reads it through the failer interface rather than from the table or a queue key
 * (docs/adr/0001, docs/adr/0013). The payload itself never leaves the application: only
 * the job class and uuid Laravel recorded in it are read, and the stored exception is
 * reduced to a sanitised first line.
 *
 * The interface offers no ordering and no paging, so records are ordered newest first by
 * their failure time, then by id, and a page continues after an opaque cursor holding the
 * last record's time and id. Ids are not assumed to be numbers: Laravel's file failer uses
 * the job uuid as the record id, and its database failer uses a row id.
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
     *
     * @throws InvalidArgumentException when the cursor is not one this command issued.
     */
    public function page(int $limit, ?string $before = null): array
    {
        $limit = max(1, min(self::MAX_LIMIT, $limit));
        $cursor = $this->decodeCursor($before);

        $rows = [];
        foreach ($this->failer->all() as $record) {
            $row = $this->row($record);
            if ($row === null) {
                continue;
            }
            if ($cursor !== null && ! $this->isOlder($row, $cursor)) {
                continue;
            }
            $rows[] = $row;
        }

        usort($rows, static function (array $a, array $b): int {
            if ($a['at'] !== $b['at']) {
                return $b['at'] <=> $a['at'];
            }

            return self::compareIds($b['id'], $a['id']);
        });

        $next = null;
        if (count($rows) > $limit) {
            $rows = array_slice($rows, 0, $limit);
            $next = self::encodeCursor($rows[count($rows) - 1]);
        }

        return [
            'failed_jobs' => array_map(static function (array $row): array {
                unset($row['at'], $row['payload']);

                return $row;
            }, $rows),
            'next_before' => $next,
        ];
    }

    /**
     * @return array{id: string, uuid: string, connection: string, queue: string, job: string, failed_at: string, exception: string|null, at: int}|null
     */
    private function row(mixed $record): ?array
    {
        $record = (array) $record;
        $id = $record['id'] ?? null;

        // An id is the record's cursor tie-breaker, so a record without one cannot be paged
        // and is left out rather than returned with a cursor nobody can use.
        if ($id === null || $id === '') {
            return null;
        }

        $payload = $this->payload($record['payload'] ?? null);
        $failedAt = $record['failed_at'] ?? null;

        return [
            'id' => (string) $id,
            'uuid' => (string) ($record['uuid'] ?? $payload['uuid'] ?? ''),
            'connection' => (string) ($record['connection'] ?? ''),
            'queue' => (string) ($record['queue'] ?? ''),
            'job' => $this->jobClass($payload),
            'failed_at' => (string) ($failedAt ?? ''),
            'exception' => $this->exception($record['exception'] ?? null),
            'at' => $this->failedAt($record, $failedAt),
        ];
    }

    /**
     * The record's failure time in unix seconds. The file failer reports it directly; the
     * database failer stores a datetime string, which is parsed in the application's zone —
     * the zone the application wrote it in.
     */
    private function failedAt(array $record, mixed $failedAt): int
    {
        $timestamp = $record['failed_at_timestamp'] ?? null;
        if (is_int($timestamp)) {
            return $timestamp;
        }

        $parsed = strtotime((string) ($failedAt ?? ''));

        return $parsed === false ? 0 : $parsed;
    }

    /**
     * @return array<string, mixed>|null
     */
    private function payload(mixed $payload): ?array
    {
        if (! is_string($payload) || $payload === '') {
            return null;
        }

        $decoded = json_decode($payload, true);

        return is_array($decoded) ? $decoded : null;
    }

    /**
     * The payload is Laravel's serialised job envelope. Only the class name is taken from
     * it; its arguments are never read, copied or returned (handoff §6.2).
     */
    private function jobClass(?array $payload): string
    {
        $name = $payload['displayName'] ?? $payload['job'] ?? '';

        return is_string($name) ? $name : '';
    }

    /**
     * The failer stores the whole throwable as a stack trace; the list needs one sanitised
     * line, not the trace, the file it happened in, or any credential the message carries.
     */
    private function exception(mixed $exception): ?string
    {
        if (! is_string($exception) || trim($exception) === '') {
            return null;
        }

        $firstLine = explode("\n", $exception, 2)[0];
        // PHP renders the location as " in <file>:<line>"; the line and the path are noise
        // in a list, and the message before it is what an operator reads.
        $firstLine = preg_replace('/ in \S+:\d+$/', '', $firstLine) ?? $firstLine;

        return Sanitizer::text($firstLine);
    }

    /**
     * @return array{at: int, id: string}|null
     */
    private function decodeCursor(?string $before): ?array
    {
        if ($before === null || $before === '') {
            return null;
        }

        $raw = base64_decode(strtr($before, '-_', '+/'), true);
        $data = $raw === false ? null : json_decode($raw, true);
        if (! is_array($data) || ! is_int($data['at'] ?? null) || ! is_string($data['id'] ?? null) || $data['id'] === '') {
            throw new InvalidArgumentException('before must be the next_before of a previous page');
        }

        return ['at' => $data['at'], 'id' => $data['id']];
    }

    /**
     * @param  array{at: int, id: string}  $cursor
     */
    private function isOlder(array $row, array $cursor): bool
    {
        if ($row['at'] !== $cursor['at']) {
            return $row['at'] < $cursor['at'];
        }

        return self::compareIds($row['id'], $cursor['id']) < 0;
    }

    private static function encodeCursor(array $row): string
    {
        $json = (string) json_encode(['at' => $row['at'], 'id' => $row['id']]);

        return rtrim(strtr(base64_encode($json), '+/', '-_'), '=');
    }

    /**
     * Orders ids the way the store does: numeric ids by value, and the file failer's uuids
     * lexically. Both are total orders, which is all the tie-breaker needs.
     */
    private static function compareIds(string $a, string $b): int
    {
        if (is_numeric($a) && is_numeric($b)) {
            return (float) $a <=> (float) $b;
        }

        return strcmp($a, $b);
    }
}
