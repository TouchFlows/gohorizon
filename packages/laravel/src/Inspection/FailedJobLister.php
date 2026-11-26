<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Inspection;

use GoHorizon\Laravel\Telemetry\Sanitizer;
use Illuminate\Database\Query\Builder;
use Illuminate\Queue\Failed\DatabaseFailedJobProvider;
use Illuminate\Queue\Failed\DatabaseUuidFailedJobProvider;
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
 *
 * Laravel's database failers expose their table, so a page can be read with a keyset query
 * over `failed_at` and the id instead of materialising the whole store. Every other failer
 * is read through `all()`, which for the file failer is bounded by Laravel's own retention
 * limit for that file.
 *
 * The keyset page is only a seek when the store is indexed in the order it asks for: Laravel's
 * own failed-jobs migration has no such index, so a store should carry `(failed_at, id)` — or
 * `(failed_at, uuid)` for the uuid failer (docs/adr/0013, decision 4). Without it the page is
 * still one bounded read, but the database sorts the table to produce it.
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

        // A store that can be asked in order is asked for one page and nothing else. A
        // cursor this store could not have issued (a uuid cursor sent after switching
        // failers) takes the interface path, where the ordering is defined in PHP.
        $page = $this->pageInTheStore($limit, $cursor);
        if ($page !== null) {
            return $page;
        }

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
     * Reads the page from a database failer's own table with two keyset conditions, so the
     * store is asked for at most one record more than the page. Null means this failer has
     * no table to ask, or its cursor belongs to a different store.
     *
     * The two database failers number their records differently: the classic one exposes the
     * row id, and the uuid one exposes the uuid column as the record id (its `all()` maps it
     * that way). The tie-breaker column and the exposed id therefore differ, while the
     * cursor keeps the same shape for both.
     *
     * @param  array{at: int, id: string}|null  $cursor
     * @return array{failed_jobs: list<array<string, mixed>>, next_before: string|null}|null
     */
    private function pageInTheStore(int $limit, ?array $cursor): ?array
    {
        if ($this->failer instanceof DatabaseUuidFailedJobProvider) {
            return $this->pageQuery(
                $this->failer->getTable()->orderByDesc('failed_at')->orderByDesc('uuid'),
                'uuid',
                static function (mixed $record): mixed {
                    // The interface reports the uuid as the record id, so the page does the
                    // same; the column itself still carries the uuid the actions take.
                    $record->id = $record->uuid;

                    return $record;
                },
                $limit,
                $cursor,
            );
        }

        if ($this->failer instanceof DatabaseFailedJobProvider && ($cursor === null || is_numeric($cursor['id']))) {
            return $this->pageQuery(
                $this->failer->getTable()->orderByDesc('failed_at')->orderByDesc('id'),
                'id',
                static fn (mixed $record): mixed => $record,
                $limit,
                $cursor,
            );
        }

        return null;
    }

    /**
     * @param  callable(mixed): mixed  $normalise
     * @param  array{at: int, id: string}|null  $cursor
     * @return array{failed_jobs: list<array<string, mixed>>, next_before: string|null}
     */
    private function pageQuery(Builder $query, string $column, callable $normalise, int $limit, ?array $cursor): array
    {
        $query->limit($limit + 1);

        if ($cursor !== null) {
            // The store wrote failed_at in the application's timezone, which is PHP's default
            // inside Laravel; formatting the cursor the same way compares like with like.
            $at = date('Y-m-d H:i:s', $cursor['at']);
            $query->where(static function (Builder $query) use ($at, $column, $cursor): void {
                $query->where('failed_at', '<', $at)
                    ->orWhere(static function (Builder $query) use ($at, $column, $cursor): void {
                        $query->where('failed_at', '=', $at)->where($column, '<', $cursor['id']);
                    });
            });
        }

        $records = $query->get()->all();
        $hasMore = count($records) > $limit;
        if ($hasMore) {
            $records = array_slice($records, 0, $limit);
        }

        $rows = [];
        foreach ($records as $record) {
            $row = $this->row($normalise($record));
            if ($row !== null) {
                $rows[] = $row;
            }
        }

        // The cursor names the last record returned here, not the look-ahead one that only
        // proved another page exists.
        $next = $hasMore && $rows !== [] ? self::encodeCursor($rows[count($rows) - 1]) : null;

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
