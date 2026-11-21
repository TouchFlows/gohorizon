import type { QueueRow } from '../view'

export default function QueueTable({ rows }: { rows: QueueRow[] }) {
  if (rows.length === 0) {
    return <p className="muted">No queue has been sampled yet.</p>
  }

  return (
    <table className="table">
      <thead>
        <tr>
          <th>Queue</th>
          <th className="num">Pending</th>
          <th className="num">Reserved</th>
          <th className="num">Delayed</th>
          <th>Oldest pending</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={row.queue}>
            <td>{row.queue}</td>
            <td className="num">{row.pending}</td>
            <td className="num">{row.reserved}</td>
            <td className="num">{row.delayed}</td>
            <td>
              {row.oldest}
              {row.error !== null && <span className="muted"> — {row.error}</span>}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
