<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Redis\Keys;
use GoHorizon\Laravel\Telemetry\TelemetryPublisher;
use GoHorizon\Laravel\Telemetry\TelemetryRecorder;
use Illuminate\Contracts\Queue\Job;
use Illuminate\Queue\Events\JobProcessing;
use Illuminate\Queue\Events\WorkerStopping;
use Illuminate\Queue\WorkerStopReason;
use PHPUnit\Framework\TestCase;
use RuntimeException;

final class TelemetryRecorderTest extends TestCase
{
    public function test_it_emits_one_terminal_event_per_attempt(): void
    {
        $publisher = new RecordingPublisher();
        $recorder = $this->recorder($publisher);
        $job = $this->job();

        $recorder->jobStarted(new JobProcessing('redis', $job));
        $recorder->jobFinished($job, 'processed');
        $recorder->flush();

        self::assertCount(1, $publisher->events);
        $event = $publisher->events[0];

        self::assertSame(1, $event['schema']);
        self::assertSame('job.finished', $event['type']);
        self::assertSame('production', $event['application']);
        self::assertSame('production', $event['environment']);
        self::assertSame('gohorizon:production:production:default-supervisor:1a2b3c4d', $event['worker']);
        self::assertSame('processed', $event['job']['outcome']);
        self::assertSame('App\\Jobs\\SendMail', $event['job']['class']);
        self::assertSame('emails', $event['job']['queue']);
        self::assertSame(2, $event['job']['attempts']);
        self::assertArrayHasKey('duration_ms', $event['job']);
        self::assertMatchesRegularExpression('/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/', $event['at']);
        self::assertSame([], $event['tags']);
    }

    public function test_it_never_emits_the_payload_or_stack_trace(): void
    {
        $publisher = new RecordingPublisher();
        $recorder = $this->recorder($publisher);
        $job = $this->job();

        $recorder->jobFinished($job, 'failed', new RuntimeException('boom at /srv/app/x.php'));
        $recorder->flush();

        $encoded = json_encode($publisher->events, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);
        self::assertStringNotContainsString('{job payload', $encoded);
        self::assertStringNotContainsString('#0 /srv', $encoded);
        self::assertStringContainsString('boom at /srv/app/x.php', $encoded);
    }

    public function test_a_failing_publisher_is_counted_and_never_thrown(): void
    {
        $publisher = new FailingPublisher();
        $recorder = $this->recorder($publisher, batchSize: 1);

        $recorder->jobFinished($this->job(), 'processed'); // flushes immediately, fails

        self::assertSame(1, $recorder->dropped());
        self::assertCount(1, $publisher->attempts, 'the batch was attempted exactly once');
    }

    public function test_worker_stopping_flushes_and_records_the_reason(): void
    {
        $publisher = new RecordingPublisher();
        $recorder = $this->recorder($publisher, batchSize: 1000);

        $recorder->jobFinished($this->job(), 'processed');
        self::assertSame([], $publisher->events, 'nothing is published before the batch fills');

        $recorder->workerStopping(new WorkerStopping(
            status: 0,
            workerOptions: null,
            reason: WorkerStopReason::Interrupted,
            jobsProcessed: 12,
            lastJobProcessedAt: null,
            memoryUsage: 42.25,
            connectionName: 'redis',
            queue: 'emails',
        ));

        self::assertCount(2, $publisher->events);
        $stopped = $publisher->events[1];
        self::assertSame('worker.stopped', $stopped['type']);
        self::assertSame('interrupted', $stopped['worker_event']['reason']);
        self::assertSame(12, $stopped['worker_event']['jobs_processed']);
        self::assertSame(42.3, $stopped['worker_event']['memory_mb']);
    }

    public function test_it_rejects_a_buffer_smaller_than_the_batch(): void
    {
        $this->expectException(\InvalidArgumentException::class);

        new TelemetryRecorder(new RecordingPublisher(), new Keys('app', 'production'), 'worker', true, 100, 10);
    }

    private function recorder(TelemetryPublisher $publisher, int $batchSize = 100): TelemetryRecorder
    {
        return new TelemetryRecorder(
            $publisher,
            new Keys('production', 'production'),
            'gohorizon:production:production:default-supervisor:1a2b3c4d',
            true,
            $batchSize,
            max($batchSize, 1000),
        );
    }

    private function job(): Job
    {
        $job = $this->createMock(Job::class);
        $job->method('uuid')->willReturn('11111111-2222-3333-4444-555555555555');
        $job->method('getJobId')->willReturn('job-id-1');
        $job->method('resolveName')->willReturn('App\\Jobs\\SendMail');
        $job->method('getConnectionName')->willReturn('redis');
        $job->method('getQueue')->willReturn('emails');
        $job->method('attempts')->willReturn(2);

        return $job;
    }
}

final class FailingPublisher implements TelemetryPublisher
{
    /** @var list<array<string, mixed>> */
    public array $attempts = [];

    public function publish(array $events): void
    {
        $this->attempts[] = $events;

        throw new RuntimeException('redis is down');
    }
}
