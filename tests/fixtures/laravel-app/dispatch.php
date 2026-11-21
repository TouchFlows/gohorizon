<?php

declare(strict_types=1);

use App\Jobs\AlwaysFails;
use App\Jobs\Hang;
use App\Jobs\RecordMarker;
use App\Jobs\ReleaseOnce;
use App\Jobs\SlowMarker;
use Illuminate\Contracts\Console\Kernel;

require __DIR__.'/vendor/autoload.php';

/** @var \Illuminate\Foundation\Application $app */
$app = require __DIR__.'/bootstrap/app.php';
$app->make(Kernel::class)->bootstrap();

$kind = $argv[1] ?? '';
$argument = $argv[2] ?? '';
$queue = getenv('GOHORIZON_E2E_QUEUE') ?: 'default';

switch ($kind) {
    case 'marker':
        RecordMarker::dispatch($argument)->onQueue($queue);
        break;
    case 'slow':
        SlowMarker::dispatch($argument)->onQueue($queue);
        break;
    case 'fail':
        AlwaysFails::dispatch()->onQueue($queue);
        break;
    case 'release':
        ReleaseOnce::dispatch()->onQueue($queue);
        break;
    case 'hang':
        Hang::dispatch()->onQueue($queue);
        break;
    default:
        fwrite(STDERR, "usage: dispatch.php marker <path>|slow <path>|fail|release|hang\n");
        exit(2);
}
