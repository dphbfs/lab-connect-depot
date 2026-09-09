import { useEffect, useMemo, useState } from 'react'
import './App.css'

const PAGE_SIZE_OPTIONS = [25, 50, 100, 200]

const OVERVIEW_COLUMNS = [
  { key: 'timestamp', label: 'Time' },
  { key: 'tool', label: 'Tool' },
  { key: 'target_name', label: 'Target' },
  { key: 'actor_name', label: 'Actor' },
  { key: 'status', label: 'Status' },
]

const DETAIL_FIELDS = [
  { key: 'id', label: 'ID' },
  { key: 'source', label: 'Source' },
  { key: 'actor_version', label: 'Actor version' },
  { key: 'mcp_session_id', label: 'MCP session' },
  { key: 'target_node_key', label: 'Target node key' },
  { key: 'command', label: 'Command' },
  { key: 'detail', label: 'Detail' },
]

function useTheme() {
  const [theme, setTheme] = useState(() => {
    try {
      const stored = localStorage.getItem('audit-ui-theme')
      if (stored === 'light' || stored === 'dark') return stored
    } catch {
      // localStorage unavailable — fall through to system preference
    }
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  })

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    try {
      localStorage.setItem('audit-ui-theme', theme)
    } catch {
      // best-effort only
    }
  }, [theme])

  return [theme, setTheme]
}

function formatCell(key, value) {
  if (value === null || value === undefined || value === '') return '—'
  if (key === 'timestamp') {
    const d = new Date(value)
    return Number.isNaN(d.getTime()) ? value : d.toLocaleString()
  }
  if (key === 'command') {
    try {
      const argv = JSON.parse(value)
      return Array.isArray(argv) ? argv.join(' ') : value
    } catch {
      return value
    }
  }
  return String(value)
}

function StatusBadge({ status }) {
  const ok = status === 0
  return (
    <span className={`status-badge ${ok ? 'status-ok' : 'status-err'}`}>
      {status === -1 ? 'no exit code' : status}
    </span>
  )
}

function AuditRow({ row, expanded, onToggle }) {
  return (
    <>
      <tr className="audit-row" onClick={onToggle}>
        <td className="expand-cell">{expanded ? '▾' : '▸'}</td>
        {OVERVIEW_COLUMNS.map((col) => (
          <td key={col.key} className={col.key === 'status' ? 'status-cell' : undefined}>
            {col.key === 'status' ? <StatusBadge status={row.status} /> : formatCell(col.key, row[col.key])}
          </td>
        ))}
      </tr>
      {expanded && (
        <tr className="audit-row-detail">
          <td colSpan={OVERVIEW_COLUMNS.length + 1}>
            <dl className="detail-grid">
              {DETAIL_FIELDS.map((f) => (
                <div className="detail-field" key={f.key}>
                  <dt>{f.label}</dt>
                  <dd>{formatCell(f.key, row[f.key])}</dd>
                </div>
              ))}
            </dl>
          </td>
        </tr>
      )}
    </>
  )
}

export default function App() {
  const [theme, setTheme] = useTheme()
  const [pageSize, setPageSize] = useState(PAGE_SIZE_OPTIONS[0])
  const [offset, setOffset] = useState(0)
  const [rows, setRows] = useState([])
  const [total, setTotal] = useState(0)
  const [expandedId, setExpandedId] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    fetch(`/api/audit-log?limit=${pageSize}&offset=${offset}`)
      .then((res) => {
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
        return res.json()
      })
      .then((data) => {
        if (cancelled) return
        setRows(data.rows ?? [])
        setTotal(data.total ?? 0)
      })
      .catch((err) => {
        if (!cancelled) setError(err.message)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [pageSize, offset])

  const page = Math.floor(offset / pageSize) + 1
  const pageCount = Math.max(1, Math.ceil(total / pageSize))

  const changePageSize = (size) => {
    setPageSize(size)
    setOffset(0)
    setExpandedId(null)
  }

  const goToPage = (delta) => {
    setOffset((o) => Math.min(Math.max(o + delta * pageSize, 0), Math.max(0, (pageCount - 1) * pageSize)))
    setExpandedId(null)
  }

  return (
    <div>
      <header className="app-header">
        <h1>Audit log</h1>
        <button
          className="theme-toggle"
          onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
          aria-label="Toggle dark mode"
        >
          {theme === 'dark' ? '☀️ Light' : '🌙 Dark'}
        </button>
      </header>

      <div className="toolbar">
        <label>
          Rows per page{' '}
          <select value={pageSize} onChange={(e) => changePageSize(Number(e.target.value))}>
            {PAGE_SIZE_OPTIONS.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <div className="pagination">
          <button disabled={offset === 0} onClick={() => goToPage(-1)}>
            ← Prev
          </button>
          <span>
            Page {page} of {pageCount} ({total} rows)
          </span>
          <button disabled={offset + pageSize >= total} onClick={() => goToPage(1)}>
            Next →
          </button>
        </div>
      </div>

      {error && <p className="error-banner">Failed to load audit log: {error}</p>}

      <table className="audit-table">
        <thead>
          <tr>
            <th aria-hidden="true"></th>
            {OVERVIEW_COLUMNS.map((col) => (
              <th key={col.key}>{col.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {!loading && rows.length === 0 && (
            <tr>
              <td colSpan={OVERVIEW_COLUMNS.length + 1} className="empty-cell">
                No audit log rows.
              </td>
            </tr>
          )}
          {rows.map((row) => (
            <AuditRow
              key={row.id}
              row={row}
              expanded={expandedId === row.id}
              onToggle={() => setExpandedId(expandedId === row.id ? null : row.id)}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}
