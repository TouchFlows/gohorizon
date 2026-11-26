import { useState } from 'react'
import { AdminActionError, deleteFailedJob, retryFailedJob } from '../api'
import type { FailedJob } from '../api'
import { failedActionNotice, failedJobClass } from '../failed'
import { mutationMessage } from '../view'

type Verb = 'retry' | 'delete'

interface Notice {
  failed: boolean
  text: string
}

// FailedJobActions is the two controls the failed screen offers for one record, and the answer
// to whichever was pressed. Both go through the service's own endpoints, which carry the CSRF
// token from the cookie in the header and ask the application to act (docs/adr/0012).
//
// Either answer can leave the store without the record: a retry takes it back and a delete
// removes it, and a 404 says it was already gone. onStoreChanged is how the screen learns to
// re-read its list, because a record the application no longer stores may not stay on it.
//
// Delete is the one action nothing can undo, so it asks first, naming the record it would
// remove; the press that removes it is a second, deliberate one.
export default function FailedJobActions({
  job,
  onStoreChanged,
  // The application's descriptor says which of the two it supports (docs/adr/0014). An action
  // it does not support is disabled rather than hidden, so the panel still names what exists.
  canRetry = true,
  canDelete = true,
  reason = '',
}: {
  job: FailedJob
  onStoreChanged: () => void
  canRetry?: boolean
  canDelete?: boolean
  reason?: string
}) {
  const [busy, setBusy] = useState<Verb | null>(null)
  const [notice, setNotice] = useState<Notice | null>(null)
  // Whether the application still stores this failed record: a retry pushes it back and a
  // delete removes it, and after either the same id has nothing left to act on until the job
  // fails again.
  const [settled, setSettled] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const act = async (verb: Verb) => {
    setBusy(verb)
    setNotice(null)
    try {
      const queue =
        verb === 'retry' ? await retryFailedJob(job.uuid) : await deleteFailedJob(job.uuid)
      setSettled(true)
      setConfirming(false)
      setNotice({ failed: false, text: failedActionNotice(verb, queue) })
      onStoreChanged()
    } catch (failure) {
      // The question is over whatever the answer was, so the controls and the answer itself are
      // visible again rather than the confirmation hiding the refusal behind it.
      setConfirming(false)
      // A 404 is the application saying the record is already gone, so the buttons go quiet
      // with the message: pressing them again could only ask the same question.
      if (failure instanceof AdminActionError && failure.status === 404) {
        setSettled(true)
        // The application says the record is not there, so the list has to stop showing it
        // even though this call asked for a change.
        onStoreChanged()
      }
      setNotice({ failed: true, text: mutationMessage(failure) })
    } finally {
      setBusy(null)
    }
  }

  // While the question is being asked the two actions step aside for it; once the delete is
  // in flight the normal buttons return, disabled, so the progress is where it has always been.
  if (confirming && busy === null && !settled) {
    return (
      <div className="detail__actions" role="group" aria-label="Confirm delete">
        <span className="notice notice--bad">
          Delete the failed record of {failedJobClass(job)} on{' '}
          {job.queue === '' ? 'an unknown queue' : job.queue}? This cannot be undone.
        </span>
        <button
          type="button"
          className="chip chip--danger"
          disabled={!canDelete}
          onClick={() => void act('delete')}
        >
          Delete now
        </button>
        <button type="button" className="chip" onClick={() => setConfirming(false)}>
          Cancel
        </button>
      </div>
    )
  }

  return (
    <div className="detail__actions">
      <button
        type="button"
        className="chip"
        disabled={busy !== null || settled || !canRetry}
        title={canRetry ? undefined : 'the application does not support this action'}
        onClick={() => void act('retry')}
      >
        {busy === 'retry' ? 'Retrying…' : 'Retry'}
      </button>
      <button
        type="button"
        className="chip chip--danger"
        disabled={busy !== null || settled || !canDelete}
        title={canDelete ? undefined : 'the application does not support this action'}
        onClick={() => setConfirming(true)}
      >
        {busy === 'delete' ? 'Deleting…' : 'Delete'}
      </button>
      {reason !== '' && <span className="muted">{reason}</span>}
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
