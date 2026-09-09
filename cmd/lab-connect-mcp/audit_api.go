// Audit log HTTP API + the Audit log UI's static files — see CONTEXT.md
// "Audit log". Served on its own port (default :4224), separate from the
// MCP protocol port, so a browser can hit it same-origin with no CORS
// setup and no risk of an MCP client mistaking it for part of the MCP
// wire protocol.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

const (
	defaultAuditPageLimit = 25
	maxAuditPageLimit     = 500
)

// auditRow is one row of the audit_log table, JSON-shaped for the UI.
// Field names match the columns directly — see audit.go's auditSchema.
type auditRow struct {
	ID            int64   `json:"id"`
	Timestamp     string  `json:"timestamp"`
	Source        string  `json:"source"`
	Tool          string  `json:"tool"`
	ActorName     *string `json:"actor_name"`
	ActorVersion  *string `json:"actor_version"`
	MCPSessionID  *string `json:"mcp_session_id"`
	TargetName    *string `json:"target_name"`
	TargetNodeKey *string `json:"target_node_key"`
	Command       *string `json:"command"`
	Status        int     `json:"status"`
	Detail        *string `json:"detail"`
}

type auditListResponse struct {
	Rows  []auditRow `json:"rows"`
	Total int64      `json:"total"`
}

// auditListHandler serves GET /api/audit-log?limit=&offset=, newest rows
// first. limit is clamped to (0, maxAuditPageLimit]; offset below 0 is
// treated as 0.
func auditListHandler(audit *auditLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit", defaultAuditPageLimit)
		if limit <= 0 || limit > maxAuditPageLimit {
			limit = defaultAuditPageLimit
		}
		offset := queryInt(r, "offset", 0)
		if offset < 0 {
			offset = 0
		}

		rows, err := audit.List(r.Context(), limit, offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total, err := audit.Count(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(auditListResponse{Rows: rows, Total: total})
	}
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// List returns up to limit rows, newest first, starting at offset.
func (a *auditLog) List(ctx context.Context, limit, offset int) ([]auditRow, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, timestamp, source, tool, actor_name, actor_version, mcp_session_id,
		       target_name, target_node_key, command, status, detail
		FROM audit_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(
			&r.ID, &r.Timestamp, &r.Source, &r.Tool, &r.ActorName, &r.ActorVersion, &r.MCPSessionID,
			&r.TargetName, &r.TargetNodeKey, &r.Command, &r.Status, &r.Detail,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if out == nil {
		out = []auditRow{}
	}
	return out, rows.Err()
}

// Count returns the total number of audit_log rows.
func (a *auditLog) Count(ctx context.Context) (int64, error) {
	var n int64
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&n)
	return n, err
}

// uiServer builds the HTTP handler for the UI port: the audit-log JSON
// API plus the built React app's static files.
func uiServer(audit *auditLog, uiDir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/audit-log", auditListHandler(audit))
	mux.Handle("/", http.FileServer(http.Dir(uiDir)))
	return mux
}
