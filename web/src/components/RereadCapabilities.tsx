import { useState } from 'react'
import type { Capabilities } from '../api'
import { idleReread, rereadFailed, rereadFinished, rereadStarted } from '../reread'
import type { RereadState } from '../reread'

// RereadCapabilities is the control beside the read-only banner: it asks the service to read the
// descriptor again now, so a repair is confirmed by this request rather than by waiting out the
// interval or reloading the page. It is drawn for administrators only — the role the service
// requires of the call — is disabled while the request is in flight, and says what the read
// found, because that is the question it exists to answer (docs/adr/0014).
export default function RereadCapabilities({
  reread,
  canReread = false,
}: {
  reread: () => Promise<Capabilities>
  canReread?: boolean
}) {
  const [state, setState] = useState<RereadState>(idleReread)

  if (!canReread) {
    return null
  }

  const run = async () => {
    setState(rereadStarted())
    try {
      setState(rereadFinished(await reread()))
    } catch (failure) {
      setState(rereadFailed(failure))
    }
  }

  return (
    <>
      <button type="button" className="chip" disabled={state.reading} onClick={() => void run()}>
        {state.reading ? 'Re-reading…' : 'Re-read now'}
      </button>
      {state.outcome !== '' && (
        <span className="muted" role="status">
          {state.outcome}
        </span>
      )}
    </>
  )
}
