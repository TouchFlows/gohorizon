import { useState, type FormEvent } from 'react'

interface SignInProps {
  // onSignedIn resolves once the service has set the session cookie, and rejects with the
  // wording this form shows.
  onSignedIn: (name: string, password: string) => Promise<void>
}

// SignIn is the whole screen a signed-out browser gets: no navigation and no pages, because
// every read behind them would answer 401. The refusal is the service's, not this form's —
// it does not say whether the name or the password was wrong, and neither does the answer.
export default function SignIn({ onSignedIn }: SignInProps) {
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (pending) {
      return
    }
    setPending(true)
    setError(null)
    try {
      await onSignedIn(name, password)
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure))
      // The password is cleared and the name kept: a refused attempt is nearly always a typo
      // in the password, and the name is the part that stays the same.
      setPassword('')
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="signin">
      <form className="signin__form" onSubmit={(event) => void submit(event)}>
        <h1 className="signin__title">Sign in</h1>
        <p className="muted signin__intro">
          GoHorizon reports queue depth, worker state and job history, so it asks who you are
          first.
        </p>

        <label className="signin__label" htmlFor="signin-name">
          Name
        </label>
        <input
          className="signin__input"
          id="signin-name"
          name="name"
          autoComplete="username"
          value={name}
          onChange={(event) => setName(event.target.value)}
          required
          autoFocus
        />

        <label className="signin__label" htmlFor="signin-password">
          Password
        </label>
        <input
          className="signin__input"
          id="signin-password"
          name="password"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          required
        />

        {error !== null && (
          <p className="banner signin__error" role="alert">
            {error}
          </p>
        )}

        <button className="signin__submit" type="submit" disabled={pending}>
          {pending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}
