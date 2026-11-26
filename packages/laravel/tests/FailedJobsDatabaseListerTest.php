<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Inspection\FailedJobLister;
use Illuminate\Database\Capsule\Manager as Capsule;
use Illuminate\Database\Connection;
use Illuminate\Queue\Failed\DatabaseFailedJobProvider;
use Illuminate\Queue\Failed\DatabaseUuidFailedJobProvider;
use PHPUnit\Framework\TestCase;
use RuntimeException;

/**
 * The database failers expose their table, so the lister can read one page in the store
 * instead of materialising every record through `all()`. Both failers are covered because
 * they expose different ids: the classic one the row id, the uuid one the uuid column.
 */
final class FailedJobsDatabaseListerTest extends TestCase
{
    private Capsule $capsule;

    private Connection $connection;

    protected function setUp(): void
    {
        parent::setUp();

        $this->capsule = new Capsule;
        $this->capsule->addConnection([
            'driver' => 'sqlite',
            'database' => ':memory:',
            'prefix' => '',
        ]);
        $this->capsule->setAsGlobal();
        $this->connection = $this->capsule->getConnection();
    }

    public function test_it_pages_the_database_store_without_reading_all_of_it(): void
    {
        $this->createClassicTable();
        // Inserted out of failure order on purpose: the page has to come from the store's
        // ordering, not from the order the rows happen to sit in.
        $this->insertClassic('2026-11-26 08:00:00', 1);
        $this->insertClassic('2026-11-26 09:30:00', 2);
        $this->insertClassic('2026-11-26 09:00:00', 3);
        $this->insertClassic('2026-11-26 09:00:00', 4);

        $lister = new FailedJobLister(new NoFullScanDatabaseFailedJobs(
            $this->capsule->getDatabaseManager(),
            'default',
            'failed_jobs',
        ));

        $this->connection->flushQueryLog();
        $this->connection->enableQueryLog();

        $first = $lister->page(2);

        self::assertSame(['2', '4'], array_column($first['failed_jobs'], 'id'));
        self::assertNotNull($first['next_before']);
        // One select, and it asked for the page plus the one record that proves another page
        // exists. `all()` would have thrown instead.
        self::assertCount(1, $this->connection->getQueryLog());
        self::assertStringContainsString('limit 3', strtolower((string) $this->connection->getQueryLog()[0]['query']));

        $second = $lister->page(2, $first['next_before']);

        // Both records failed in the same second, so the row id breaks the tie downwards.
        self::assertSame(['3', '1'], array_column($second['failed_jobs'], 'id'));
        self::assertNull($second['next_before']);
    }

    public function test_it_pages_a_uuid_store_by_the_uuid_it_reports_as_the_id(): void
    {
        $this->createUuidTable();
        // The uuids descend in failure order but ascend in insertion order, so a page ordered
        // by the row id would return them the wrong way round.
        $this->insertUuid('bbbbbbbb-0000-4000-8000-000000000002', '2026-11-26 09:00:00');
        $this->insertUuid('aaaaaaaa-0000-4000-8000-000000000001', '2026-11-26 09:00:00');
        $this->insertUuid('cccccccc-0000-4000-8000-000000000003', '2026-11-26 09:30:00');

        $lister = new FailedJobLister(new NoFullScanDatabaseUuidFailedJobs(
            $this->capsule->getDatabaseManager(),
            'default',
            'failed_jobs',
        ));

        $first = $lister->page(2);

        self::assertSame([
            'cccccccc-0000-4000-8000-000000000003',
            'bbbbbbbb-0000-4000-8000-000000000002',
        ], array_column($first['failed_jobs'], 'id'));
        // The uuid the actions take is still reported beside the id.
        self::assertSame($first['failed_jobs'][0]['id'], $first['failed_jobs'][0]['uuid']);
        self::assertNotNull($first['next_before']);

        $second = $lister->page(2, $first['next_before']);

        self::assertSame(['aaaaaaaa-0000-4000-8000-000000000001'], array_column($second['failed_jobs'], 'id'));
        self::assertNull($second['next_before']);
    }

    public function test_it_asks_for_one_page_and_one_look_ahead_on_a_larger_store(): void
    {
        $this->createClassicTable();
        for ($sequence = 1; $sequence <= 25; $sequence++) {
            $this->insertClassic('2026-11-26 09:00:00', $sequence);
        }

        $this->connection->flushQueryLog();
        $this->connection->enableQueryLog();

        $page = (new FailedJobLister(new NoFullScanDatabaseFailedJobs(
            $this->capsule->getDatabaseManager(),
            'default',
            'failed_jobs',
        )))->page(5);

        self::assertCount(5, $page['failed_jobs']);
        self::assertStringContainsString('limit 6', strtolower((string) $this->connection->getQueryLog()[0]['query']));
        self::assertCount(1, $this->connection->getQueryLog());
    }

    public function test_it_pins_the_cursor_bytes_both_paging_paths_reuse(): void
    {
        // The cursor is a contract with every client that stores and replays one, so its
        // exact bytes are pinned rather than only round-tripped through encode/decode. The
        // literal is `{"at":1795683600,"id":"2"}` in unpadded base64url: the epoch of
        // 2026-11-26 09:00:00 UTC and the row id that broke the tie.
        $cursor = 'eyJhdCI6MTc5NTY4MzYwMCwiaWQiOiIyIn0';

        $timezone = date_default_timezone_get();
        date_default_timezone_set('UTC');

        try {
            $this->createClassicTable();
            // Inserted out of failure order, and with two records at 09:00:00 so the cursor
            // has to carry the row id that broke the tie.
            $this->insertClassic('2026-11-26 08:00:00', 1);
            $this->insertClassic('2026-11-26 09:00:00', 2);
            $this->insertClassic('2026-11-26 07:00:00', 3);
            $this->insertClassic('2026-11-26 09:00:00', 4);
            $this->insertClassic('2026-11-26 06:00:00', 5);

            $database = new FailedJobLister(new NoFullScanDatabaseFailedJobs(
                $this->capsule->getDatabaseManager(),
                'default',
                'failed_jobs',
            ));

            // The first page ends on the record whose position the literal names.
            self::assertSame($cursor, $database->page(2)['next_before']);

            // The interface path sees the same records and the same store order, so the very
            // same bytes have to continue it: a client's stored cursor does not know which
            // failer the application is configured with.
            $memory = new FailedJobLister(new FakeFailedJobs(
                array_map(static fn (int $id, string $failedAt): object => (object) [
                    'id' => $id,
                    'connection' => 'redis',
                    'queue' => 'default',
                    'payload' => (string) json_encode(['displayName' => 'App\\Jobs\\AlwaysFails']),
                    'failed_at' => $failedAt,
                ], [1, 2, 3, 4, 5], [
                    '2026-11-26 08:00:00',
                    '2026-11-26 09:00:00',
                    '2026-11-26 07:00:00',
                    '2026-11-26 09:00:00',
                    '2026-11-26 06:00:00',
                ]),
            ));

            $fromStore = $database->page(2, $cursor);
            $fromMemory = $memory->page(2, $cursor);

            self::assertSame(['1', '3'], array_column($fromStore['failed_jobs'], 'id'));
            self::assertSame(['1', '3'], array_column($fromMemory['failed_jobs'], 'id'));
            // Both paths continue from the same bytes and issue the same next cursor, so the
            // two stores are interchangeable to a client paging through them.
            self::assertNotNull($fromStore['next_before']);
            self::assertSame(
                (string) json_encode($fromMemory['next_before']),
                (string) json_encode($fromStore['next_before']),
            );
        } finally {
            date_default_timezone_set($timezone);
        }
    }

    private function createClassicTable(): void
    {
        $this->connection->statement(
            'create table failed_jobs (id integer primary key autoincrement, connection varchar(255), queue varchar(255), payload text, exception text, failed_at datetime)'
        );
    }

    private function createUuidTable(): void
    {
        $this->connection->statement(
            'create table failed_jobs (id integer primary key autoincrement, uuid varchar(255), connection varchar(255), queue varchar(255), payload text, exception text, failed_at datetime)'
        );
    }

    private function insertClassic(string $failedAt, int $sequence): void
    {
        $this->connection->table('failed_jobs')->insert([
            'connection' => 'redis',
            'queue' => 'default',
            'payload' => $this->payload(sprintf('uuid-%04d', $sequence)),
            'exception' => 'RuntimeException: boom',
            'failed_at' => $failedAt,
        ]);
    }

    private function insertUuid(string $uuid, string $failedAt): void
    {
        $this->connection->table('failed_jobs')->insert([
            'uuid' => $uuid,
            'connection' => 'redis',
            'queue' => 'default',
            'payload' => $this->payload($uuid),
            'exception' => 'RuntimeException: boom',
            'failed_at' => $failedAt,
        ]);
    }

    private function payload(string $uuid): string
    {
        return (string) json_encode([
            'uuid' => $uuid,
            'displayName' => 'App\\Jobs\\AlwaysFails',
        ]);
    }
}

/**
 * A database failer whose interface listing is forbidden: a page that still works proves the
 * lister never asked the store for all of its records.
 */
final class NoFullScanDatabaseFailedJobs extends DatabaseFailedJobProvider
{
    public function all($queue = null)
    {
        throw new RuntimeException('the lister read the whole failed store');
    }
}

/**
 * The same guard for the uuid store, which is a separate provider in Laravel and exposes the
 * uuid column as its record id.
 */
final class NoFullScanDatabaseUuidFailedJobs extends DatabaseUuidFailedJobProvider
{
    public function all($queue = null)
    {
        throw new RuntimeException('the lister read the whole failed store');
    }
}
