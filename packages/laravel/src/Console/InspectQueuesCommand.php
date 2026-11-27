<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Console;

use GoHorizon\Laravel\Inspection\QueueInspector;
use GoHorizon\Laravel\Redis\Keys;
use Illuminate\Console\Command;

/**
 * Prints queue depth as JSON for the Go control plane.
 *
 * Counts are observations from the Queue contract, never an execution ledger
 * (handoff §14.7).
 */
final class InspectQueuesCommand extends Command
{
    /**
     * The version of the payload this command prints. GoHorizon reads only this one, and
     * `gohorizon:capabilities` reports it so the service can compare it with the version it
     * reads instead of discovering a mismatch from a failed read (docs/adr/0014).
     */
    public const SCHEMA = 1;

    protected $signature = 'gohorizon:inspect
                            {--connection= : Queue connection name}
                            {--queue=* : Queues to inspect; defaults to the connection configuration}
                            {--json : Output JSON}';

    protected $description = 'Inspect queue depth and reservations through the Laravel queue contract';

    public function __construct(
        private readonly QueueInspector $inspector,
        private readonly Keys $keys,
    ) {
        parent::__construct();
    }

    public function handle(): int
    {
        $connection = $this->option('connection');
        $queues = array_values(array_filter(
            array_map(static fn ($queue) => trim((string) $queue), (array) $this->option('queue')),
            static fn (string $queue) => $queue !== ''
        ));

        $payload = [
            'schema' => self::SCHEMA,
            'application' => $this->keys->application(),
            'environment' => $this->keys->environment(),
            'connection' => $this->inspector->connectionName(is_string($connection) ? $connection : null),
            'queues' => $this->inspector->inspect(is_string($connection) ? $connection : null, $queues),
            'generated_at' => gmdate('Y-m-d\TH:i:s\Z'),
        ];

        if ($this->option('json')) {
            $this->line((string) json_encode($payload, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));

            return self::SUCCESS;
        }

        $this->table(
            ['queue', 'pending', 'reserved', 'delayed', 'size'],
            array_map(static fn (array $row) => [
                $row['queue'],
                $row['pending'] ?? '-',
                $row['reserved'] ?? '-',
                $row['delayed'] ?? '-',
                $row['size'] ?? '-',
            ], $payload['queues'])
        );

        return self::SUCCESS;
    }
}
