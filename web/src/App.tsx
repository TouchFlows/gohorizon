// The dashboard shell. Routes, data and screens arrive in later slices; the point of this
// one is that the bundle, the embed and the CI drift check are proven before a screen
// depends on them.
export default function App() {
  return (
    <div className="shell">
      <header className="shell__header">
        <span className="shell__brand">GoHorizon</span>
        <span className="shell__note">queue supervisor</span>
      </header>
      <main className="shell__main">
        <h1>Overview</h1>
        <p>The dashboard is being built slice by slice. This is the shell it will render in.</p>
      </main>
    </div>
  )
}
