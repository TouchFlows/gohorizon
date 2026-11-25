import { useCallback, useEffect, useState } from 'react'
import { endSession, readSession, startSession } from './api'

// SessionStatus is the four things the shell can be told before it draws a page:
//   checking   — the question is in flight, and nothing has been decided yet;
//   signed-out — the service has users and this browser is not one of them;
//   signed-in  — a session is in hand, and `user` names it;
//   open       — the service was started without users, so there is nobody to sign in as.
export type SessionStatus = 'checking' | 'signed-out' | 'signed-in' | 'open'

export interface SessionControl {
  status: SessionStatus
  user: string | null
  // signIn mirrors the service's answer: it resolves once the cookie is set, and rejects with
  // the wording the form shows.
  signIn: (name: string, password: string) => Promise<void>
  signOut: () => Promise<void>
  // expire is what a 401 from a page's own read means: the session ended under us.
  expire: () => void
}

// useSession asks who this browser is, once, and then keeps that answer up to date from what
// the API says afterwards. The three actions are stable identities, so the health poll that
// depends on `expire` is not restarted on every render.
export function useSession(): SessionControl {
  const [status, setStatus] = useState<SessionStatus>('checking')
  const [user, setUser] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    readSession(controller.signal)
      .then((lookup) => {
        if (lookup.kind === 'open') {
          setStatus('open')
          return
        }
        if (lookup.kind === 'signed-in') {
          setUser(lookup.user)
          setStatus('signed-in')
          return
        }
        setStatus('signed-out')
      })
      .catch(() => {
        // A service that cannot say who we are is treated as one that will not: the form is
        // the only screen that could get us any further. An abort is the shell going away,
        // and has nobody left to answer.
        if (controller.signal.aborted) {
          return
        }
        setStatus('signed-out')
      })
    return () => controller.abort()
  }, [])

  const signIn = useCallback(async (name: string, password: string) => {
    const session = await startSession(name, password)
    setUser(session.user)
    setStatus('signed-in')
  }, [])

  const signOut = useCallback(async () => {
    try {
      await endSession()
    } finally {
      // The browser drops the cookie on the answer, but reaching the form does not depend on
      // that having arrived: nothing is served to a page here that needs the cookie.
      setUser(null)
      setStatus('signed-out')
    }
  }, [])

  const expire = useCallback(() => {
    setUser(null)
    setStatus('signed-out')
  }, [])

  return { status, user, signIn, signOut, expire }
}
