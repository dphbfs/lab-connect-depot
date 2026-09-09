package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type lastAuditRowResult struct {
	tool          string
	targetName    sql.NullString
	targetNodeKey sql.NullString
	command       sql.NullString
	status        int
	detail        sql.NullString
}

func lastAuditRow(t *testing.T, audit *auditLog, tool string) lastAuditRowResult {
	t.Helper()
	var r lastAuditRowResult
	err := audit.db.QueryRow(
		`SELECT tool, target_name, target_node_key, command, status, detail
		 FROM audit_log WHERE tool = ? ORDER BY id DESC LIMIT 1`, tool,
	).Scan(&r.tool, &r.targetName, &r.targetNodeKey, &r.command, &r.status, &r.detail)
	if err != nil {
		t.Fatalf("query last %q audit row: %v", tool, err)
	}
	return r
}

func TestAudit_ExecuteSuccess_RecordsRealExitCode(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs, audit := connectTestClientWithAudit(t, client)
	defer cs.Close()

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"machine": "sandbox-ubuntu", "argv": []string{"echo", "hi"}},
	}); err != nil {
		t.Fatalf("CallTool(execute) error: %v", err)
	}

	row := lastAuditRow(t, audit, "execute")
	if row.status != 0 {
		t.Fatalf("status = %d, want 0 (fakeRunner's generic echo exits 0)", row.status)
	}
	if !row.targetNodeKey.Valid || row.targetNodeKey.String != "a" {
		t.Fatalf("target_node_key = %+v, want resolved NodeKey %q", row.targetNodeKey, "a")
	}
	if !row.command.Valid {
		t.Fatalf("command must be recorded for execute, got NULL")
	}
}

func TestAudit_ExecuteNonZeroExit_RecordsRealExitCodeNotNoExitCode(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs, audit := connectTestClientWithAudit(t, client)
	defer cs.Close()

	// doDownload against a path fakeRunner doesn't have returns a real
	// exit code of 1 — a command that ran and failed, not a rejection.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "transport",
		Arguments: map[string]any{
			"machine": "sandbox-ubuntu", "direction": "download", "path": "/no/such/file",
		},
	}); err != nil {
		t.Fatalf("CallTool(transport download) error: %v", err)
	}

	row := lastAuditRow(t, audit, "transport")
	if row.status != 1 {
		t.Fatalf("status = %d, want 1 (real exit code from a command that ran and failed), not %d", row.status, noExitCode)
	}
}

func TestAudit_ExecuteRejectedUnpairedMachine_RecordsNoExitCode(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: "unpaired"}})

	cs, audit := connectTestClientWithAudit(t, client)
	defer cs.Close()

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"machine": "sandbox-ubuntu", "argv": []string{"echo", "hi"}},
	}); err != nil {
		t.Fatalf("CallTool(execute) error: %v", err)
	}

	row := lastAuditRow(t, audit, "execute")
	if row.status != noExitCode {
		t.Fatalf("status = %d, want %d for a call rejected before dispatch", row.status, noExitCode)
	}
	if !row.detail.Valid || row.detail.String == "" {
		t.Fatalf("detail must explain the rejection, got %+v", row.detail)
	}
}

func TestAudit_MachinesCall_HasNullTargetAndCommand(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs, audit := connectTestClientWithAudit(t, client)
	defer cs.Close()

	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "machines"}); err != nil {
		t.Fatalf("CallTool(machines) error: %v", err)
	}

	row := lastAuditRow(t, audit, "machines")
	if row.targetName.Valid || row.targetNodeKey.Valid || row.command.Valid {
		t.Fatalf("machines row should have NULL target/command, got %+v", row)
	}
	if row.status != 0 {
		t.Fatalf("status = %d, want 0 for a successful machines listing", row.status)
	}
}

func TestAudit_BrokenDataDir_DoesNotFailToolCall(t *testing.T) {
	// A file where the audit DB's parent directory should be makes
	// os.MkdirAll fail — openAuditLog itself surfaces that error (it's a
	// startup-time failure, not a per-call one), but once a log is open,
	// per-call write failures must never surface to the tool caller. This
	// covers that second half directly by closing the DB out from under a
	// live auditLog and confirming Record doesn't panic and the caller
	// never sees it.
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs, audit := connectTestClientWithAudit(t, client)
	defer cs.Close()
	audit.db.Close() // subsequent Record calls will now fail to write

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"machine": "sandbox-ubuntu", "argv": []string{"echo", "hi"}},
	})
	if err != nil {
		t.Fatalf("CallTool(execute) error: %v, want the tool call to still succeed despite a broken audit log", err)
	}
	if res.IsError {
		t.Fatalf("execute result is an error: %+v, want fail-open behavior", res.Content)
	}
}

func TestOpenAuditLog_UnwritablePathErrors(t *testing.T) {
	// openAuditLog itself (startup, not per-call) is allowed to fail loudly
	// — that's before any tool call exists to protect.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := openAuditLog(filepath.Join(blocked, "audit.db")); err == nil {
		t.Fatalf("openAuditLog() with a file where the parent dir should be: want error, got nil")
	}
}
