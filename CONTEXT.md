# lab-connect-depot

Packages lab-connect's Runner, its bundled Headscale mesh, and `lab-connect-mcp` (this repo's own MCP server, built from `cmd/lab-connect-mcp`) into one deployable Gateway image.

## Language

**Audit log**:
A local SQLite store, owned by `lab-connect-mcp`, recording one row per MCP tool call (`machines`, `execute`, `transport`) — id, source, actor identity, target, tool, command, status, detail, timestamp. Named plainly "Audit log" (not "Gateway ___") because it's meant to eventually also receive rows pushed from lab-connect's own per-Node Audit Entry (see lab-connect's CONTEXT.md "Audit Entry") — the `source` column (`"gateway"` today, reserved value for Node-pushed rows later) exists for that merge.
_Avoid_: Gateway audit log, call log, audit trail (keep it as the one term, since a second name would fork the glossary right before the two logs unify).

**Audit Entry** (defined in lab-connect's own CONTEXT.md, not this repo's): a JSON-lines record on a Node's local disk — timestamp, Client Session id, command, exit code. Distinct today from this repo's "Audit log" — the Node writes its own, this repo's MCP process writes its own — until the future push work referenced above lands.

**Actor**:
The identity of the MCP client that made a call, as self-reported at MCP handshake (`ClientInfo.Name` + `ClientInfo.Version`) plus its `mcp_session_id`. Not authenticated — this server has no auth — so "actor" means "who the caller claims to be," not a verified identity.
_Avoid_: Caller, client, user (this repo's tools are called by AI agents, not human users — "actor" keeps that explicit).

**Target**:
The Node an `execute`/`transport` call ran against, stored as both `target_name` (what the actor asked for) and `target_node_key` (what actually resolved via `pairedMachine`) — `NULL` for `machines`, which lists all Nodes rather than acting on one.

**Audit log UI**:
The React app (`ui/`) plus its JSON API (`cmd/lab-connect-mcp/audit_api.go`, `GET /api/audit-log`) that reads the Audit log — an operator-facing viewer, not a write path. Served by `lab-connect-mcp` itself on its own port (default `:4224`), same-origin with its API, same no-auth v1 posture as the MCP port.
