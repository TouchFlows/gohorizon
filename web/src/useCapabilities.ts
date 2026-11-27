import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchCapabilities, rereadCapabilities, Unauthorized } from './api'
import type { Capabilities } from './api'
import { pollInterval } from './useHealth'

// CapabilityEvent is one settled refresh: the descriptor the service published, or the fact
// that the read did not answer. A failure carries no value on purpose — it is not a descriptor,
// and the last one stays on screen.
export type CapabilityEvent = { kind: 'descriptor'; capabilities: Capabilities } | { kind: 'failure' }

// applyCapabilities is the hook's state decision, kept out of React so both outcomes can be read
// without a DOM. An answer replaces the descriptor; a failure keeps it, because a blip must not
// put a control back that this deployment cannot perform — and the first read failing still
// leaves null, which is the session deciding on its own (docs/adr/0014).
export function applyCapabilities(current: Capabilities | null, event: CapabilityEvent): Capabilities | null {
  return event.kind === 'descriptor' ? event.capabilities : current
}

// pollCapabilities asks once, reports the answer, and asks again on the dashboard's interval
// until it is stopped, which is what the effect's cleanup does on unmount. Refreshing rather
// than reading once is what lets a deployment that was repaired, or restarted with a package
// it could not answer for before, get its controls back without a page reload: the descriptor
// is a snapshot of the service's own startup, so a restarted service is the one that changes it.
//
// Stopping aborts the request in flight and clears the timer, so a late answer cannot land on a
// screen nobody is watching and nothing is asked afterwards.
export function pollCapabilities(
  fetcher: (signal: AbortSignal) => Promise<Capabilities>,
  onChange: (event: CapabilityEvent) => void,
  onUnauthorized?: () => void,
): () => void {
  const controller = new AbortController()
  let timer: ReturnType<typeof setTimeout> | undefined

  const load = async () => {
    try {
      const capabilities = await fetcher(controller.signal)
      onChange({ kind: 'descriptor', capabilities })
    } catch (failure) {
      if (controller.signal.aborted) {
        return
      }
      // A 401 is not a failed read: the session ended, so this stops asking and lets the shell
      // ask for a sign-in, exactly as the health poll does.
      if (failure instanceof Unauthorized) {
        onUnauthorized?.()
        return
      }
      onChange({ kind: 'failure' })
    }
    if (!controller.signal.aborted) {
      timer = setTimeout(load, pollInterval)
    }
  }

  void load()
  return () => {
    controller.abort()
    clearTimeout(timer)
  }
}

// CapabilityControl is the descriptor and the one action a page can take on it.
export interface CapabilityControl {
  capabilities: Capabilities | null
  // reread asks the service to read the descriptor again now instead of waiting for the poll,
  // and publishes the answer into the same state the poll feeds, so the controls follow a
  // repair immediately. It rejects when the call did not answer at all — a refusal, a 401, a
  // failed request — and the caller says so beside the banner (docs/adr/0014).
  reread: () => Promise<Capabilities>
}

// useCapabilities is what the application supports, refreshed alongside the health poll. It
// decides which administrative controls are drawn; the session decides whether there are any at
// all (docs/adr/0014).
export function useCapabilities(onUnauthorized?: () => void): CapabilityControl {
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null)

  // Held in a ref so the loop keeps one identity: a callback rebuilt on every render would
  // otherwise restart the poll, and its timer with it.
  const expired = useRef(onUnauthorized)
  expired.current = onUnauthorized

  useEffect(() => {
    return pollCapabilities(
      fetchCapabilities,
      (event) => {
        setCapabilities((current) => applyCapabilities(current, event))
      },
      () => expired.current?.(),
    )
  }, [])

  const reread = useCallback(async () => {
    try {
      const answer = await rereadCapabilities()
      setCapabilities(answer)
      return answer
    } catch (failure) {
      // The read is the session's request like any other: a 401 sends the shell back to the
      // sign-in screen rather than reporting a failed re-read nobody could have avoided.
      if (failure instanceof Unauthorized) {
        expired.current?.()
      }
      throw failure
    }
  }, [])

  return { capabilities, reread }
}
