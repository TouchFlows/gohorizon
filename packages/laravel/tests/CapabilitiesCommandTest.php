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
        self::assertSame('redis', $data['queue']['connections']['redis']['driver']);
        self::assertSame(90, $data['queue']['connections']['redis']['retry_after']);
        self::assertSame(extension_loaded('pcntl'), $data['runtime']['pcntl']);
        self::assertContains('Illuminate\\Queue\\Events\\JobReleasedAfterException', $data['events']);

        self::assertStringNotContainsString('queue-password-that-must-not-leak', $output);
    }

    public function test_it_reports_the_payload_versions_its_commands_emit(): void
    {
        $this->app->instance('queue.failer', new FakeFailedJobs([]));

        Artisan::call('gohorizon:capabilities', ['--json' => true]);
        $capabilities = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        Artisan::call('gohorizon:inspect', ['--json' => true, '--queue' => ['default']]);
        $inspect = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        Artisan::call('gohorizon:failed:list', ['--json' => true]);
        $failed = json_decode(Artisan::output(), true, flags: JSON_THROW_ON_ERROR);

        // The descriptor reports the versions the other commands actually print, so a package
        // that bumps one of those payloads cannot leave this answer behind (docs/adr/0014).
        self::assertSame($inspect['schema'], $capabilities['payload_schemas']['inspect']);
        self::assertSame($failed['schema'], $capabilities['payload_schemas']['failed_jobs']);
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
