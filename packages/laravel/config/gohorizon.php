<?php

declare(strict_types=1);

return [

    /*
    |--------------------------------------------------------------------------
    | Enable GoHorizon integration
    |--------------------------------------------------------------------------
    |
    | Basic worker supervision works without this package. Telemetry and queue
    | inspection require it. Disable to remove every listener the package adds.
    |
    */

    'enabled' => env('GOHORIZON_ENABLED', true),

    /*
    |--------------------------------------------------------------------------
    | Application identity
    |--------------------------------------------------------------------------
    |
    | Both values are slugified into the Redis namespace segments, which must
    | match ^[a-z0-9][a-z0-9-]*$ (see docs/adr/0003-redis-namespace.md). The Go
    | side uses the same values as applications[].id and .environment.
    |
    */

    'application' => env('GOHORIZON_APPLICATION', env('APP_NAME', 'application')),

    'environment' => env('GOHORIZON_ENVIRONMENT', env('APP_ENV', 'production')),

    /*
    |--------------------------------------------------------------------------
    | Redis
    |--------------------------------------------------------------------------
    |
    | The Laravel Redis connection used to publish telemetry. The stream key is
    | derived from the application identity above; do not add a prefix that the
    | Go service does not know about.
    |
    */

    'redis' => [
        'connection' => env('GOHORIZON_REDIS_CONNECTION', 'default'),
    ],

    /*
    |--------------------------------------------------------------------------
    | Telemetry
    |--------------------------------------------------------------------------
    |
    | Telemetry is best effort: a failure to publish never affects a job. The
    | buffer is bounded; when it overflows the oldest events are dropped and
    | counted, so a Redis outage cannot grow memory without limit.
    |
    */

    'telemetry' => [
        'enabled' => env('GOHORIZON_TELEMETRY', true),
        'batch_size' => (int) env('GOHORIZON_TELEMETRY_BATCH', 100),
        'buffer_size' => (int) env('GOHORIZON_TELEMETRY_BUFFER', 1000),
        'retention' => (int) env('GOHORIZON_TELEMETRY_RETENTION', 100000),
    ],

    /*
    |--------------------------------------------------------------------------
    | Tags
    |--------------------------------------------------------------------------
    |
    | Captured only when a job exposes a tags() method (the Horizon convention).
    | Laravel core has no job tag concept, so this is usually empty.
    |
    */

    'tags' => [
        'enabled' => env('GOHORIZON_TAGS', true),
    ],

];
