<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\GoHorizonServiceProvider;
use Illuminate\Support\Facades\Artisan;
use Orchestra\Testbench\TestCase;

final class CapabilitiesCommandTest extends TestCase
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

        $app['config']->set('database.redis.default', [
            'host' => '127.0.0.1',
            'port' => 6379,
            'database' => 2,
            'password' => 'redis-password-that-must-not-leak',
        ]);
        $app['config']->set('queue.connections.redis', [
            'driver' => 'redis',
            'queue' => 'default',
            'retry_after' => 90,
            'password' => 'queue-password-that-must-not-leak',
        ]);
    }

    public function test_it_reports_capabilities_as_json_without_secrets(): void
    {
        $exit = Artisan::call('gohorizon:capabilities', ['--json' => true]);
        $output = Artisan::output();

        self::assertSame(0, $exit);

        $data = json_decode($output, true, flags: JSON_THROW_ON_ERROR);

        self::assertSame(1, $data['schema']);
        self::assertSame('my-application', $data['application']);
        self::assertSame('staging', $data['environment']);
        self::assertSame('gohorizon:my-application:staging:events', $data['telemetry']['stream']);
        self::assertSame(90, $data['queue']['connections']['redis']['retry_after']);
        self::assertTrue($data['queue']['connections']['redis']['has_password']);
        self::assertTrue($data['redis']['has_password']);
        self::assertSame(2, $data['redis']['database']);
        self::assertContains('Illuminate\\Queue\\Events\\JobReleasedAfterException', $data['events']);
        self::assertContains('inspect_queues', $data['operations']);

        self::assertStringNotContainsString('redis-password-that-must-not-leak', $output);
        self::assertStringNotContainsString('queue-password-that-must-not-leak', $output);
    }

    public function test_the_inspection_command_degrades_on_a_driver_without_counts(): void
    {
        config()->set('queue.default', 'sync');

        $exit = Artisan::call('gohorizon:inspect', ['--json' => true, '--queue' => ['default']]);
        $data = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        self::assertSame(0, $exit);
        self::assertSame('sync', $data['connection']);
        self::assertSame('default', $data['queues'][0]['queue']);
    }
}
