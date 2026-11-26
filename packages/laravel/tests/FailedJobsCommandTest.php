<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\GoHorizonServiceProvider;
use Illuminate\Support\Facades\Artisan;
use Orchestra\Testbench\TestCase;

final class FailedJobsCommandTest extends TestCase
{
    protected function getPackageProviders($app): array
    {
        return [GoHorizonServiceProvider::class];
    }

    protected function defineEnvironment($app): void
    {
        $app['config']->set('gohorizon.application', 'My Application');
        $app['config']->set('gohorizon.environment', 'staging');
        $app['config']->set('gohorizon.telemetry.enabled', false);
    }

    public function test_it_prints_one_page_of_the_applications_failed_jobs(): void
    {
        $this->app->instance('queue.failer', new FakeFailedJobs([
            (object) ['id' => 9, 'queue' => 'emails', 'payload' => json_encode(['uuid' => 'job-9', 'displayName' => 'App\Jobs\SendInvoice']), 'exception' => 'RuntimeException: boom in /srv/app/Job.php:12', 'failed_at' => '2026-11-26 09:30:00'],
            (object) ['id' => 8, 'queue' => 'default', 'payload' => json_encode(['displayName' => 'App\Jobs\SyncOrders']), 'exception' => '', 'failed_at' => '2026-11-26 09:00:00'],
            (object) ['id' => 7, 'queue' => 'default', 'payload' => json_encode(['displayName' => 'App\Jobs\PruneLogs']), 'exception' => '', 'failed_at' => '2026-11-26 08:00:00'],
        ]));

        $exit = Artisan::call('gohorizon:failed:list', ['--json' => true, '--limit' => 2]);
        $output = Artisan::output();

        self::assertSame(0, $exit);
        $data = json_decode($output, true, flags: JSON_THROW_ON_ERROR);
        self::assertSame(1, $data['schema']);
        self::assertSame('my-application', $data['application']);
        self::assertSame('staging', $data['environment']);
        self::assertSame(['9', '8'], array_column($data['failed_jobs'], 'id'));
        self::assertIsString($data['next_before']);
        self::assertSame('App\Jobs\SendInvoice', $data['failed_jobs'][0]['job']);
        self::assertSame('job-9', $data['failed_jobs'][0]['uuid']);
        self::assertSame('RuntimeException: boom', $data['failed_jobs'][0]['exception']);
        self::assertNull($data['failed_jobs'][1]['exception']);
    }

    public function test_it_continues_after_the_cursor_it_printed(): void
    {
        $this->app->instance('queue.failer', new FakeFailedJobs([
            (object) ['id' => 9, 'queue' => 'default', 'payload' => '{}', 'exception' => '', 'failed_at' => '2026-11-26 09:30:00'],
            (object) ['id' => 8, 'queue' => 'default', 'payload' => '{}', 'exception' => '', 'failed_at' => '2026-11-26 09:00:00'],
            (object) ['id' => 7, 'queue' => 'default', 'payload' => '{}', 'exception' => '', 'failed_at' => '2026-11-26 08:00:00'],
        ]));

        Artisan::call('gohorizon:failed:list', ['--json' => true, '--limit' => 2]);
        $first = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        $exit = Artisan::call('gohorizon:failed:list', ['--json' => true, '--before' => $first['next_before']]);
        $second = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        self::assertSame(0, $exit);
        self::assertSame(['7'], array_column($second['failed_jobs'], 'id'));
        self::assertNull($second['next_before']);
    }

    public function test_it_refuses_a_page_it_cannot_ask_for(): void
    {
        $this->app->instance('queue.failer', new FakeFailedJobs([]));

        // The limit mirrors the endpoint's; a cursor that is not one of ours is refused here
        // rather than sent to the application as an argument.
        self::assertSame(2, Artisan::call('gohorizon:failed:list', ['--limit' => 0]));
        self::assertSame(2, Artisan::call('gohorizon:failed:list', ['--limit' => 201]));
        self::assertSame(2, Artisan::call('gohorizon:failed:list', ['--before' => 'not-a-cursor']));
        self::assertSame(2, Artisan::call('gohorizon:failed:list', ['--before' => '--json']));
    }
}
