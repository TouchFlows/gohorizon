<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

use Throwable;

/**
 * Sanitises exception messages before they leave the application.
 *
 * The raw payload, the job's arguments and any stack trace are never emitted; this
 * class only cleans the short message that is.
 */
final class Sanitizer
{
    private const MAX_LENGTH = 512;

    private const PATTERNS = [
        // Bearer tokens first: otherwise the key=value rule below consumes only the
        // word "Authorization" and leaves the token visible.
        '/\bBearer\s+[A-Za-z0-9\-._~+\/]+=*/i' => 'Bearer ***',
        // scheme://user:password@host
        '#([a-z][a-z0-9+.\-]*://)([^:@/\s]+):([^@/\s]+)@#i' => '$1***:***@',
        // key=value pairs for common secret names, including prefixed forms such as
        // client_secret or refresh_token (the prefix is part of the matched key).
        '/\b((?:[a-z0-9]+[_-])*(?:password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|authorization))\b(\s*[=:]\s*)([^\s,;&"\']+)/i' => '$1$2***',
    ];

    public static function message(?Throwable $exception): ?string
    {
        if ($exception === null) {
            return null;
        }

        return self::text($exception->getMessage());
    }

    /**
     * Sanitises text that is already a string, such as the message line of a stored stack
     * trace. It applies the same redaction as a thrown exception.
     */
    public static function text(?string $message): ?string
    {
        if ($message === null) {
            return null;
        }

        $message = preg_replace('/\s+/', ' ', $message) ?? '';

        foreach (self::PATTERNS as $pattern => $replacement) {
            $message = preg_replace($pattern, $replacement, $message) ?? $message;
        }

        $message = trim($message);

        if ($message === '') {
            return null;
        }

        if (mb_strlen($message) > self::MAX_LENGTH) {
            $message = mb_substr($message, 0, self::MAX_LENGTH).'…';
        }

        return $message;
    }
}
