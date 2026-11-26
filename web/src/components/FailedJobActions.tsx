import { useState } from 'react'
import { AdminActionError, deleteFailedJob, retryFailedJob } from '../api'
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
export default function FailedJobActions({
  job,
  onStoreChanged,
}: {
  job: { id: string; uuid: string }
  onStoreChanged: () => void
}) {
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
      const queue =
        verb === 'retry' ? await retryFailedJob(job.uuid) : await deleteFailedJob(job.uuid)
      setSettled(true)
      setNotice({
        failed: false,
        text: verb === 'retry' ? `queued again on ${queue}` : `deleted from ${queue}`,
      })
      onStoreChanged()
    } catch (failure) {
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
