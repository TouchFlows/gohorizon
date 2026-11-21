<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Tests;

use GoHorizon\Laravel\Redis\Keys;
use GoHorizon\Laravel\Telemetry\JobEventSubscriber;
use GoHorizon\Laravel\Telemetry\TelemetryRecorder;
use Illuminate\Container\Container;
use Illuminate\Contracts\Queue\Job;
use Illuminate\Events\Dispatcher;
use Illuminate\Queue\Events\JobExceptionOccurred;
use Illuminate\Queue\Events\JobFailed;
use Illuminate\Queue\Events\JobProcessed;
use Illuminate\Queue\Events\JobProcessing;
use Illuminate\Queue\Events\JobReleasedAfterException;
use PHPUnit\Framework\TestCase;
use RuntimeException;

final class JobEventSubscriberTest extends TestCase
{
    public function test_processed_jobs_are_reported_as_processed(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();

        $events->dispatch(new JobProcessing('redis', $job));
        $events->dispatch(new JobProcessed('redis', $job));

        self::assertCount(1, $publisher->events);
        self::assertSame('processed', $publisher->events[0]['job']['outcome']);
    }

    public function test_a_deleted_job_is_a_success_because_laravel_deletes_every_finished_job(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();
        // The real worker calls delete() on each job it finishes, so this flag is true
        // for ordinary successes too.
        $job->method('isDeleted')->willReturn(true);

        $events->dispatch(new JobProcessed('redis', $job));

        self::assertSame('processed', $publisher->events[0]['job']['outcome']);
    }

    public function test_job_processed_after_exceeding_attempts_is_a_failure_not_a_success(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();
        $job->method('hasFailed')->willReturn(true);

        $events->dispatch(new JobProcessed('redis', $job));

        self::assertSame('failed', $publisher->events[0]['job']['outcome']);
    }

    public function test_released_jobs_are_reported_with_their_backoff(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();

        $events->dispatch(new JobReleasedAfterException('redis', $job, 30));

        self::assertSame('released', $publisher->events[0]['job']['outcome']);
        self::assertSame(30, $publisher->events[0]['job']['backoff_seconds']);
    }

    public function test_failures_are_reported_with_a_sanitised_exception(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();

        $events->dispatch(new JobFailed('redis', $job, new RuntimeException('cannot reach redis://user:secret@host:6379')));

        $event = $publisher->events[0];
        self::assertSame('failed', $event['job']['outcome']);
        self::assertSame(RuntimeException::class, $event['job']['exception']['class']);
        self::assertStringNotContainsString('secret', (string) $event['job']['exception']['message']);
    }

    public function test_a_released_job_reports_one_record_carrying_the_exception(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();

        $events->dispatch(new JobProcessing('redis', $job));
        $events->dispatch(new JobExceptionOccurred('redis', $job, new RuntimeException('cannot reach redis://user:secret@host:6379')));
        $events->dispatch(new JobReleasedAfterException('redis', $job, 30));

        self::assertCount(1, $publisher->events, 'an exception plus a release is one attempt');
        $event = $publisher->events[0];
        self::assertSame('released', $event['job']['outcome']);
        self::assertSame(30, $event['job']['backoff_seconds']);
        self::assertSame(RuntimeException::class, $event['job']['exception']['class']);
        self::assertStringNotContainsString('secret', (string) $event['job']['exception']['message']);
    }

    public function test_a_failed_job_reports_one_record_even_with_both_events(): void
    {
        [$events, $publisher] = $this->subscriber();
        $job = $this->job();

        $events->dispatch(new JobProcessing('redis', $job));
        $events->dispatch(new JobExceptionOccurred('redis', $job, new RuntimeException('first')));
        $events->dispatch(new JobFailed('redis', $job, new RuntimeException('second')));

        self::assertCount(1, $publisher->events, 'an exception plus a failure is one attempt');
        self::assertSame('failed', $publisher->events[0]['job']['outcome']);
        self::assertSame('second', $publisher->events[0]['job']['exception']['message']);
    }

    /**
     * @return array{0: Dispatcher, 1: RecordingPublisher}
     */
    private function subscriber(): array
    {
        $publisher = new RecordingPublisher();
        $recorder = new TelemetryRecorder($publisher, new Keys('app', 'production'), 'worker', true, 1, 100);
        $events = new Dispatcher(new Container());
        (new JobEventSubscriber($recorder))->subscribe($events);

        return [$events, $publisher];
    }

    private function job(): Job
    {
        $job = $this->createMock(Job::class);
        $job->method('uuid')->willReturn('11111111-2222-3333-4444-555555555555');
        $job->method('getJobId')->willReturn('id-1');
        $job->method('resolveName')->willReturn('App\\Jobs\\Work');
        $job->method('getConnectionName')->willReturn('redis');
        $job->method('getQueue')->willReturn('default');
        $job->method('attempts')->willReturn(1);

        return $job;
    }
}
