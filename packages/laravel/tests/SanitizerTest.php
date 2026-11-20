<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Telemetry\Sanitizer;
use PHPUnit\Framework\TestCase;
use RuntimeException;

final class SanitizerTest extends TestCase
{
    public function test_it_redacts_credentials_in_urls(): void
    {
        $message = Sanitizer::message(new RuntimeException(
            'SQLSTATE[HY000] could not connect to redis://default:hunter2@cache.internal:6379/0'
        ));

        self::assertIsString($message);
        self::assertStringNotContainsString('hunter2', $message);
        self::assertStringContainsString('redis://***:***@cache.internal', $message);
    }

    public function test_it_redacts_key_value_secrets(): void
    {
        $message = Sanitizer::message(new RuntimeException(
            'Request failed: password=supersecret token: abc123 api_key=xyz client_secret=shh'
        ));

        self::assertIsString($message);
        foreach (['supersecret', 'abc123', 'xyz', 'shh'] as $secret) {
            self::assertStringNotContainsString($secret, $message);
        }
    }

    public function test_it_redacts_bearer_tokens(): void
    {
        $message = Sanitizer::message(new RuntimeException('Unauthorized: Authorization: Bearer eyJhbGciOi.payload.sig'));

        self::assertIsString($message);
        self::assertStringNotContainsString('eyJhbGciOi.payload.sig', $message);
    }

    public function test_it_collapses_whitespace_and_truncates(): void
    {
        $message = Sanitizer::message(new RuntimeException(str_repeat("line one\nline two\t", 200)));

        self::assertIsString($message);
        self::assertLessThanOrEqual(513, mb_strlen($message));
        self::assertStringNotContainsString("\n", $message);
    }

    public function test_it_returns_null_without_an_exception_or_message(): void
    {
        self::assertNull(Sanitizer::message(null));
        self::assertNull(Sanitizer::message(new RuntimeException('   ')));
    }
}
