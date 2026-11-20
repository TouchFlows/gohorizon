<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Redis\Keys;
use InvalidArgumentException;
use PHPUnit\Framework\TestCase;

final class KeysTest extends TestCase
{
    public function test_it_matches_the_go_key_builder_exactly(): void
    {
        // Mirrors internal/redis/keys_test.go. Changing either side is a protocol
        // change, not a refactor.
        $keys = new Keys('production', 'production');

        self::assertSame('gohorizon:production:production:events', $keys->events());
    }

    public function test_it_slugifies_human_names(): void
    {
        $keys = Keys::fromConfig([
            'application' => 'My Laravel Application',
            'environment' => 'Staging EU',
        ]);

        self::assertSame('my-laravel-application', $keys->application());
        self::assertSame('staging-eu', $keys->environment());
        self::assertSame('gohorizon:my-laravel-application:staging-eu:events', $keys->events());
    }

    public function test_it_rejects_a_name_that_cannot_be_a_segment(): void
    {
        $this->expectException(InvalidArgumentException::class);

        Keys::fromConfig(['application' => '***', 'environment' => 'production']);
    }

    public function test_it_rejects_unsafe_segments_directly(): void
    {
        $this->expectException(InvalidArgumentException::class);

        new Keys('Production:App', 'production');
    }
}
