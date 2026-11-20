<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

/**
 * Reads the identity GoHorizon stamps onto queue:work (--name=gohorizon:...).
 *
 * The supervisor sets it, the framework ignores it, and telemetry uses it to attribute
 * a job to a worker without asking the Go service first.
 */
final class WorkerIdentity
{
    public static function fromArgv(array $argv): string
    {
        foreach ($argv as $argument) {
            if (is_string($argument) && str_starts_with($argument, '--name=')) {
                $name = substr($argument, 7);

                return $name !== '' ? $name : 'unknown';
            }
        }

        return 'unknown';
    }
}
