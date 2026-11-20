<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Telemetry\TelemetryPublisher;

/**
 * Collects published events so tests can assert on them without Redis.
 */
final class RecordingPublisher implements TelemetryPublisher
{
    /** @var list<array<string, mixed>> */
    public array $events = [];

    public function publish(array $events): void
    {
        foreach ($events as $event) {
            $this->events[] = $event;
        }
    }
}
