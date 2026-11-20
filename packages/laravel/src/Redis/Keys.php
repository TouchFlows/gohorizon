<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Redis;

use Illuminate\Support\Str;
use InvalidArgumentException;

/**
 * The only place the package builds a Redis key.
 *
 * The Go service builds the same keys in internal/redis/keys.go and both sides
 * assert the exact strings in tests: this is a cross-language protocol.
 */
final class Keys
{
    public function __construct(
        private readonly string $application,
        private readonly string $environment,
    ) {
        self::assertSegment($application, 'application');
        self::assertSegment($environment, 'environment');
    }

    /**
     * @param array<string, mixed> $config
     */
    public static function fromConfig(array $config): self
    {
        return new self(
            self::segment((string) ($config['application'] ?? 'application')),
            self::segment((string) ($config['environment'] ?? 'production')),
        );
    }

    /**
     * Slugifies a human name into a valid namespace segment. Throws instead of
     * silently producing an empty segment (for example an all-symbols app name).
     */
    public static function segment(string $value): string
    {
        $slug = Str::slug($value);

        if ($slug === '') {
            throw new InvalidArgumentException(
                sprintf('GoHorizon: "%s" cannot be used as a Redis namespace segment', $value)
            );
        }

        return $slug;
    }

    public function application(): string
    {
        return $this->application;
    }

    public function environment(): string
    {
        return $this->environment;
    }

    public function scope(): string
    {
        return sprintf('gohorizon:%s:%s:', $this->application, $this->environment);
    }

    /**
     * Telemetry stream consumed by GoHorizon (handoff §6.3).
     */
    public function events(): string
    {
        return $this->scope().'events';
    }

    public function coordinates(): string
    {
        return $this->scope().'coord:';
    }

    private static function assertSegment(string $segment, string $label): void
    {
        if (preg_match('/^[a-z0-9][a-z0-9-]*$/', $segment) !== 1) {
            throw new InvalidArgumentException(
                sprintf('GoHorizon %s segment "%s" must match ^[a-z0-9][a-z0-9-]*$', $label, $segment)
            );
        }
    }
}
