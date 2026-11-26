import { useEffect, useState } from 'react'
import { fetchCapabilities, Unauthorized } from './api'
import type { Capabilities } from './api'

// useCapabilities reads the service's descriptor once, when the dashboard is drawn. A failure
// leaves it null rather than reporting one: the controls then stay as the session alone
// decides, and the service refuses an action it cannot take. A 401 is not a failure of this
// read, so it ends the session exactly like the health poll's does.
export function useCapabilities(onUnauthorized?: () => void): Capabilities | null {
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    fetchCapabilities(controller.signal)
      .then(setCapabilities)
      .catch((failure: unknown) => {
        if (controller.signal.aborted) {
          return
        }
        if (failure instanceof Unauthorized) {
          onUnauthorized?.()
        }
      })
    return () => controller.abort()
  }, [onUnauthorized])

  return capabilities
}
