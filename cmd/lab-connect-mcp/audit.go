// Audit log — see CONTEXT.md "Audit log". A local SQLite store, one row per
// MCP tool call ("machines", "execute", "transport"), distinct from
// lab-connect's own per-Node Audit Entry (internal/rpc/audit.go, in the
// lab-connect repo) until a future change pushes those into this same
// table (see the "source" column below).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const auditSchema = `
CREATE TABLE IF NOT EXISTS audit_log (
	id              INTEGER PRIMARY KEY,
	timestamp       TEXT NOT NULL,
	source          TEXT NOT NULL DEFAULT 'gateway',
	tool            TEXT NOT NULL,
	actor_name      TEXT,
	actor_version   TEXT,
	mcp_session_id  TEXT,
	target_name     TEXT,
	target_node_key TEXT,
	command         TEXT,
	status          INTEGER NOT NULL,
	detail          TEXT
);`

// noExitCode is stored in the "status" column when a call never produced a
// real exit code — a connection error and a pre-dispatch validation
// rejection (unpaired machine, empty argv, bad direction, ...) are both
// recorded this way; "command"/"target_name"/"detail" carry the
// distinction, not a second status value.
const noExitCode = -1

// auditActor is the calling MCP client's self-reported identity — from the
// "initialize" handshake's ClientInfo, plus the per-connection session id.
// Not authenticated: this server has no auth, so this is "who the caller
// claims to be," not a verified identity.
type auditActor struct {
	Name      string
	Version   string
	SessionID string
}

// auditCall is one row: everything known about a tool call once its
// outcome (or rejection) is known.
type auditCall struct {
	Tool          string
	Actor         auditActor
	TargetName    string // "" -> NULL; empty for "machines"
	TargetNodeKey string // "" -> NULL; empty for "machines"
	Command       string // "" -> NULL; empty for "machines"
	Status        int    // real exit code, or noExitCode
	Detail        string // "" -> NULL; error/rejection message only, never output
}

// auditLog appends auditCall rows to a local SQLite database. Fail-open by
// design: a write failure is logged to stderr and otherwise ignored — it
// must never block or fail the tool call it's recording.
type auditLog struct {
	db *sql.DB
}

func openAuditLog(path string) (*auditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit log dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	if _, err := db.Exec(auditSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create audit_log table: %w", err)
	}
	return &auditLog{db: db}, nil
}

// Record writes one row. Errors are logged to stderr and swallowed —
// see auditLog's fail-open doc comment.
func (a *auditLog) Record(ctx context.Context, c auditCall) {
	if a == nil {
		return
	}
	_, err := a.db.ExecContext(ctx, `
		INSERT INTO audit_log (
			timestamp, tool, actor_name, actor_version, mcp_session_id,
			target_name, target_node_key, command, status, detail
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339),
		c.Tool,
		nullIfEmpty(c.Actor.Name),
		nullIfEmpty(c.Actor.Version),
		nullIfEmpty(c.Actor.SessionID),
		nullIfEmpty(c.TargetName),
		nullIfEmpty(c.TargetNodeKey),
		nullIfEmpty(c.Command),
		c.Status,
		nullIfEmpty(c.Detail),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit log: write failed: %v\n", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// auditDataPath resolves the audit log's own data directory —
// LAB_CONNECT_MCP_DATA_DIR, not LAB_CONNECT_CONFIG_DIR (the Runner's own
// dir): this process owns its data file even though both run in the same
// Gateway container.
func auditDataPath() (string, error) {
	if d := os.Getenv("LAB_CONNECT_MCP_DATA_DIR"); d != "" {
		return filepath.Join(d, "audit.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "lab-connect-mcp", "audit.db"), nil
}
