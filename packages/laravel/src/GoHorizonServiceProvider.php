<?php

declare(strict_types=1);

namespace GoHorizon\Laravel;

use GoHorizon\Laravel\Console\CapabilitiesCommand;
use GoHorizon\Laravel\Console\InspectQueuesCommand;
use GoHorizon\Laravel\Inspection\QueueInspector;
use GoHorizon\Laravel\Redis\Keys;
use GoHorizon\Laravel\Telemetry\JobEventSubscriber;
use GoHorizon\Laravel\Telemetry\RedisStreamPublisher;
use GoHorizon\Laravel\Telemetry\TelemetryPublisher;
use GoHorizon\Laravel\Telemetry\TelemetryRecorder;
use GoHorizon\Laravel\Telemetry\WorkerIdentity;
use Illuminate\Support\ServiceProvider;
use Throwable;

final class GoHorizonServiceProvider extends ServiceProvider
{
    public function register(): void
    {
        $this->mergeConfigFrom(__DIR__.'/../config/gohorizon.php', 'gohorizon');

        $this->app->singleton(Keys::class, static fn ($app): Keys => Keys::fromConfig(
            (array) $app['config']->get('gohorizon', [])
        ));

        $this->app->singleton(TelemetryPublisher::class, static fn ($app): TelemetryPublisher => new RedisStreamPublisher(
            $app['redis'],
            $app['config']->get('gohorizon.redis.connection'),
            $app->make(Keys::class),
            (int) $app['config']->get('gohorizon.telemetry.retention', 100000),
        ));

        $this->app->singleton(TelemetryRecorder::class, static fn ($app): TelemetryRecorder => new TelemetryRecorder(
            $app->make(TelemetryPublisher::class),
            $app->make(Keys::class),
            WorkerIdentity::fromArgv($_SERVER['argv'] ?? []),
            (bool) $app['config']->get('gohorizon.tags.enabled', true),
            (int) $app['config']->get('gohorizon.telemetry.batch_size', 100),
            (int) $app['config']->get('gohorizon.telemetry.buffer_size', 1000),
        ));

        $this->app->singleton(QueueInspector::class, static fn ($app): QueueInspector => new QueueInspector($app['queue']));
    }

    public function boot(): void
    {
        if ($this->app->runningInConsole()) {
            $this->publishes([
                __DIR__.'/../config/gohorizon.php' => config_path('gohorizon.php'),
            ], 'gohorizon-config');

            $this->commands([
                CapabilitiesCommand::class,
                InspectQueuesCommand::class,
            ]);
        }

        if (! (bool) config('gohorizon.enabled', true) || ! (bool) config('gohorizon.telemetry.enabled', true)) {
            return;
        }

        $recorder = $this->app->make(TelemetryRecorder::class);
        (new JobEventSubscriber($recorder))->subscribe($this->app['events']);

        // A worker that stops unexpectedly still flushes what it buffered; a SIGKILL
        // loses it, which is the accepted trade-off (docs/adr/0006).
        register_shutdown_function(static function () use ($recorder): void {
            try {
                $recorder->flush();
            } catch (Throwable) {
                // Never let telemetry surface during shutdown.
            }
        });
    }
}
