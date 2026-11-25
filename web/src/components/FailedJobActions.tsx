import { useState } from 'react'
import { deleteFailedJob, FailedJobActionError, retryFailedJob, Unauthorized } from '../api'
import type { Job } from '../api'

type Verb = 'retry' | 'delete'

interface Notice {
  failed: boolean
  text: string
}

// FailedJobActions is the two controls the failed screen offers for one attempt, and the answer
// to whichever was pressed. Both go through the service's own endpoints, which carry the CSRF
// token from the cookie in the header and ask the application to act (docs/adr/0012).
export default function FailedJobActions({ job }: { job: Job }) {
  const [busy, setBusy] = useState<Verb | null>(null)
  const [notice, setNotice] = useState<Notice | null>(null)
  // Whether the application still stores this failed record: a retry pushes it back and a
  // delete removes it, and after either the same id has nothing left to act on until the job
  // fails again.
  const [settled, setSettled] = useState(false)

  const act = async (verb: Verb) => {
    setBusy(verb)
    setNotice(null)
    try {
      const answer =
        verb === 'retry' ? await retryFailedJob(job.uuid) : await deleteFailedJob(job.uuid)
      setSettled(true)
      setNotice({
        failed: false,
        text:
          verb === 'retry' ? `queued again on ${answer.queue}` : `deleted from ${answer.queue}`,
      })
    } catch (failure) {
      // A 404 is the application saying the record is already gone, so the buttons go quiet
      // with the message: pressing them again could only ask the same question.
      if (failure instanceof FailedJobActionError && failure.status === 404) {
        setSettled(true)
      }
      setNotice({ failed: true, text: messageOf(failure) })
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="detail__actions">
      <button
        type="button"
        className="chip"
        disabled={busy !== null || settled}
        onClick={() => void act('retry')}
      >
        {busy === 'retry' ? 'Retrying…' : 'Retry'}
      </button>
      <button
        type="button"
        className="chip chip--danger"
        disabled={busy !== null || settled}
        onClick={() => void act('delete')}
      >
        {busy === 'delete' ? 'Deleting…' : 'Delete'}
      </button>
      {notice !== null && (
        <span
          className={notice.failed ? 'notice notice--bad' : 'notice'}
          role={notice.failed ? 'alert' : 'status'}
        >
          {notice.text}
        </span>
      )}
    </div>
  )
}

function messageOf(failure: unknown): string {
  if (failure instanceof Unauthorized) {
    return 'Your session ended. Sign in again.'
  }
  return failure instanceof Error ? failure.message : String(failure)
}
