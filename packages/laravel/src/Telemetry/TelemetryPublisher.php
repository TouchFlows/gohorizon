<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

interface TelemetryPublisher
{
    /**
     * @param list<array<string, mixed>> $events
     *
     * @throws \Throwable The caller treats every failure as best-effort.
     */
    public function publish(array $events): void;
}
