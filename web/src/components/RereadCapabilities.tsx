import { useState } from 'react'
import type { Capabilities } from '../api'
import { idleReread, noticeFor, rereadFailed, rereadFinished, rereadStarted } from '../reread'
import type { RereadState } from '../reread'

// RereadCapabilities is the control beside the read-only banner: it asks the service to read the
// descriptor again now, so a repair is confirmed by this request rather than by waiting out the
// interval or reloading the page. It is drawn for administrators only — the role the service
// requires of the call — is disabled while the request is in flight, and says what the read
// found, because that is the question it exists to answer (docs/adr/0014).
//
// The banner is a prop rather than a sibling drawn by the page, so this component stays mounted
// while a successful read takes the banner away: the answer that removed it is the proof the
// repair worked, and a control unmounted with the sentence would blank it before anyone read it.
export default function RereadCapabilities({
  reread,
  canReread = false,
  reason = '',
}: {
  reread: () => Promise<Capabilities>
  canReread?: boolean
  reason?: string
}) {
  const [state, setState] = useState<RereadState>(idleReread)
  const notice = noticeFor(reason, state, canReread)

  const run = async () => {
    setState(rereadStarted())
    try {
      setState(rereadFinished(await reread()))
    } catch (failure) {
      setState(rereadFailed(failure))
    }
  }

  // Nothing withheld and nothing read yet: the page draws exactly what it drew before this
  // control existed, and a readable deployment grows no empty row.
  if (!notice.button && notice.outcome === '') {
    return notice.banner === '' ? null : <p className="muted">{notice.banner}</p>
  }

  return (
    <div className="banner__row">
      {notice.banner !== '' && <p className="muted">{notice.banner}</p>}
      {notice.button && (
        <button type="button" className="chip" disabled={state.reading} onClick={() => void run()}>
          {state.reading ? 'Re-reading…' : 'Re-read now'}
        </button>
      )}
      {notice.outcome !== '' && (
        <span className="muted" role="status">
          {notice.outcome}
        </span>
      )}
    </div>
  )
}
