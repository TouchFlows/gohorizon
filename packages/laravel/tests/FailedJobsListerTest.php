<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Inspection\FailedJobLister;
use PHPUnit\Framework\TestCase;

final class FailedJobsListerTest extends TestCase
{
    public function test_it_pages_newest_first_and_reports_the_cursor(): void
    {
        $lister = new FailedJobLister(new FakeFailedJobs($this->records()));

        $first = $lister->page(2);

        self::assertSame(['9', '8'], array_column($first['failed_jobs'], 'id'));
        // The cursor is the last id on the page, so the next request continues before exactly
        // that record.
        self::assertSame('8', $first['next_before']);

        $second = $lister->page(2, '8');

        self::assertSame(['7'], array_column($second['failed_jobs'], 'id'));
        self::assertNull($second['next_before']);
    }

    public function test_it_returns_the_record_without_the_payload(): void
    {
        $lister = new FailedJobLister(new FakeFailedJobs([$this->records()[0]]));

        $row = $lister->page(10)['failed_jobs'][0];

        self::assertSame('9', $row['id']);
        self::assertSame('e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d', $row['uuid']);
        self::assertSame('redis', $row['connection']);
        self::assertSame('emails', $row['queue']);
        self::assertSame('App\Jobs\SendInvoice', $row['job']);
        self::assertSame('2026-11-26 09:30:00', $row['failed_at']);
        self::assertStringNotContainsString('payload-argument-that-must-not-leak', (string) json_encode($row));
        self::assertStringNotContainsString('hunter2', (string) json_encode($row));
    }

    public function test_it_reduces_the_stored_trace_to_one_sanitised_line(): void
    {
        $lister = new FailedJobLister(new FakeFailedJobs([$this->records()[0]]));

        $exception = $lister->page(10)['failed_jobs'][0]['exception'];

        self::assertIsString($exception);
        self::assertStringStartsWith('RuntimeException: could not reach', $exception);
        self::assertStringNotContainsString("\n", $exception);
        self::assertStringNotContainsString('#0 /srv/application', $exception);
        self::assertStringNotContainsString('hunter2', $exception);
        self::assertStringNotContainsString('shh', $exception);
    }

    public function test_a_record_without_a_usable_id_is_left_out(): void
    {
        $records = $this->records();
        $records[] = (object) ['id' => '', 'queue' => 'default', 'payload' => '{}'];
        $lister = new FailedJobLister(new FakeFailedJobs($records));

        self::assertSame(['9', '8', '7'], array_column($lister->page(10)['failed_jobs'], 'id'));
    }

    public function test_a_payload_that_is_not_json_reports_no_job_class(): void
    {
        $lister = new FailedJobLister(new FakeFailedJobs([
            (object) ['id' => 3, 'queue' => 'default', 'payload' => 'not-json'],
        ]));

        self::assertSame('', $lister->page(10)['failed_jobs'][0]['job']);
    }

    /**
     * @return list<object>
     */
    private function records(): array
    {
        return [
            (object) [
                'id' => 9,
                'uuid' => 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d',
                'connection' => 'redis',
                'queue' => 'emails',
                'payload' => json_encode([
                    'displayName' => 'App\Jobs\SendInvoice',
                    'job' => 'Illuminate\Queue\CallQueuedHandler@call',
                    'data' => ['command' => 'payload-argument-that-must-not-leak'],
                ]),
                'exception' => "RuntimeException: could not reach https://user:hunter2@example.test with password=shh\n#0 /srv/application/vendor/laravel/framework/src/Queue/Worker.php(280)",
                'failed_at' => '2026-11-26 09:30:00',
            ],
            (object) [
                'id' => 8,
                'uuid' => '',
                'connection' => 'redis',
                'queue' => 'default',
                'payload' => json_encode(['displayName' => 'App\Jobs\SyncOrders']),
                'exception' => '',
                'failed_at' => '2026-11-26 09:00:00',
            ],
            (object) [
                'id' => 7,
                'uuid' => '',
                'connection' => 'redis',
                'queue' => 'default',
                'payload' => json_encode(['displayName' => 'App\Jobs\PruneLogs']),
                'exception' => 'RuntimeException: boom',
                'failed_at' => '2026-11-26 08:00:00',
            ],
        ];
    }
}
