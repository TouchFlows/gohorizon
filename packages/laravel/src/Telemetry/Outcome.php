<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

/**
 * The terminal job outcomes this package can publish.
 *
 * The Go consumer accepts exactly this set. tests/fixtures/telemetry-outcomes.json is
 * the same vocabulary, read by both test suites, so an outcome cannot be added on one
 * side alone (docs/adr/0006-telemetry-schema.md).
 */
final class Outcome
{
    public const PROCESSED = 'processed';

    public const RELEASED = 'released';

    public const FAILED = 'failed';

    public const TIMED_OUT = 'timed_out';

    /**
     * @return list<string>
     */
    public static function all(): array
    {
        return [
            self::PROCESSED,
            self::RELEASED,
            self::FAILED,
            self::TIMED_OUT,
        ];
    }
}
