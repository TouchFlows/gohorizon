<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Telemetry\Outcome;
use GoHorizon\Laravel\Telemetry\TelemetryRecorder;
use PHPUnit\Framework\TestCase;

final class OutcomeVocabularyTest extends TestCase
{
    public function test_the_shared_fixture_lists_every_outcome_this_package_publishes(): void
    {
        $path = dirname(__DIR__, 3).'/tests/fixtures/telemetry-outcomes.json';
        self::assertFileExists($path, 'the shared outcome fixture must exist for the Go consumer too');

        $fixture = json_decode(
            (string) file_get_contents($path),
            true,
            flags: JSON_THROW_ON_ERROR,
        );

        self::assertSame(TelemetryRecorder::SCHEMA, $fixture['schema']);
        self::assertSame(Outcome::all(), $fixture['job_outcomes']);
    }
}
