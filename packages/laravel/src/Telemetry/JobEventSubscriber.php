<?php

declare(strict_types=1);

namespace GoHorizon\Laravel\Telemetry;

use Illuminate\Contracts\Events\Dispatcher;
use Illuminate\Queue\Events\JobExceptionOccurred;
use Illuminate\Queue\Events\JobFailed;
use Illuminate\Queue\Events\JobProcessed;
use Illuminate\Queue\Events\JobProcessing;
use Illuminate\Queue\Events\JobReleasedAfterException;
use Illuminate\Queue\Events\JobTimedOut;
use Illuminate\Queue\Events\WorkerStopping;
use Throwable;

/**
 * Maps Laravel 13 queue events onto the GoHorizon telemetry schema.
 *
 * Several framework events collapse into one terminal record per attempt; see
 * docs/adr/0006-telemetry-schema.md for why JobProcessed alone is not success.
 */
final class JobEventSubscriber
{
    public function __construct(private readonly TelemetryRecorder $recorder)
    {
    }

    public function subscribe(Dispatcher $events): void
    {
        $events->listen(JobProcessing::class, fn (JobProcessing $event) => $this->guard(
            fn () => $this->recorder->jobStarted($event)
        ));

        $events->listen(JobProcessed::class, fn (JobProcessed $event) => $this->guard(function () use ($event): void {
            // JobProcessed fires for released and failed jobs too, so the outcome comes
            // from the job flags, not from the event class. isDeleted() is not one of
            // them: Laravel deletes every job as part of finishing it, so it is true on
            // each success and cannot distinguish anything.
            $outcome = match (true) {
                $event->job->hasFailed() => Outcome::FAILED,
                $event->job->isReleased() => Outcome::RELEASED,
                default => Outcome::PROCESSED,
            };

            $this->recorder->jobFinished($event->job, $outcome);
        }));

        $events->listen(JobReleasedAfterException::class, fn (JobReleasedAfterException $event) => $this->guard(
            fn () => $this->recorder->jobFinished($event->job, Outcome::RELEASED, null, [
                'backoff_seconds' => (int) $event->backoff,
            ])
        ));

        // No record here: the job still has to be released or failed, and that event is
        // the one terminal record for the attempt. The exception rides along with it.
        $events->listen(JobExceptionOccurred::class, fn (JobExceptionOccurred $event) => $this->guard(
            fn () => $this->recorder->jobExceptionOccurred($event->job, $event->exception)
        ));

        $events->listen(JobFailed::class, fn (JobFailed $event) => $this->guard(
            fn () => $this->recorder->jobFinished($event->job, Outcome::FAILED, $event->exception)
        ));

        $events->listen(JobTimedOut::class, fn (JobTimedOut $event) => $this->guard(
            fn () => $this->recorder->jobFinished($event->job, Outcome::TIMED_OUT)
        ));

        $events->listen(WorkerStopping::class, fn (WorkerStopping $event) => $this->guard(
            fn () => $this->recorder->workerStopping($event)
        ));
    }

    private function guard(callable $callback): void
    {
        try {
            $callback();
        } catch (Throwable) {
            // A telemetry listener must never break job execution (handoff §2.11).
        }
    }
}
