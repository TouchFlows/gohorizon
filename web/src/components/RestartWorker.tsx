import { useState } from 'react'
import { restartWorker } from '../api'
import { mutationMessage } from '../view'

// RestartWorker is the control one worker row offers: the supervisor stops the worker
// gracefully and starts its replacement in the same slot, so the job in flight finishes
// instead of being killed (docs/adr/0002). The button stays after an answer, because the
// worker keeps its uuid — it names the slot — and may be recycled again.
export default function RestartWorker({ uuid }: { uuid: string }) {
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<{ failed: boolean; text: string } | null>(null)

  const restart = async () => {
    setBusy(true)
    setNotice(null)
    try {
      await restartWorker(uuid)
      setNotice({ failed: false, text: 'restart requested' })
    } catch (failure) {
      setNotice({ failed: true, text: mutationMessage(failure) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <button type="button" className="chip" disabled={busy} onClick={() => void restart()}>
        {busy ? 'Restarting…' : 'Restart'}
      </button>
      {notice !== null && (
        <span
          className={notice.failed ? 'notice notice--bad' : 'notice'}
          role={notice.failed ? 'alert' : 'status'}
        >
          {notice.text}
        </span>
      )}
    </>
  )
}
