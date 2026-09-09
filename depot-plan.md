# Gateway image: bundled Headscale + admin UI + lab-connect-mcp over SSE

## Context

lab-connect today assumes an operator already has a Headscale (or Tailscale) mesh
running (PRD.md, DESIGN.md Phase 0) and joins/pairs against it — `cmd/lab-connect-mcp`
is a stdio MCP server spawned fresh per client by the local MCP host process.

That's a two-step barrier to adoption: stand up Headscale yourself, *then* install
lab-connect. This plan collapses that into one deployable "Gateway" Docker image —
Headscale + its admin UI + an embedded lab-connect Runner + lab-connect-mcp, all in
one container — so a homelab operator can point AI agents at one image and get a
working overlay network, pairing, and MCP tool exposure with nothing pre-existing.

**Repo split (decided this session):** the Gateway image is a **separate repo**,
`~/code/lab-connect-depot` (currently empty — just a README). It packages and builds
against the `lab-connect` repo rather than living inside it.

- **Phases A and B are changes to the `lab-connect` repo** (`/home/userzero/code/lab-connect`)
  — the MCP transport rework and the non-interactive init flag are general-purpose
  changes to lab-connect itself, useful independent of the Gateway.
- **Phases C and D live in `lab-connect-depot`** — the Dockerfile, entrypoint,
  supervisord config, compose file, and this deployment mode's own docs.
- `lab-connect-depot`'s build needs `lab-connect`'s source (or built binaries) to
  produce the two Go binaries it bundles. **Open question, flagged below** — pick a
  build-time coupling mechanism (git submodule pinned to a ref vs. `go install
  module@version` if/when lab-connect is published vs. a build-arg'd `git clone` of
  a pinned tag) before writing the Dockerfile.

**Key architectural insight validated during exploration:** the Gateway's embedded
Runner and its MCP server live in the *same container*, so `internal/control`'s
existing Unix-socket `control.Client`/`control.Server` needs **zero changes** — this
is not a "reach a remote Node's control socket" problem. The Gateway is just an
ordinary Node (its embedded Runner joins its own bundled Headscale exactly like any
Node would) that also happens to own the Headscale process itself. It gets Paired to
every other real homelab Node through the existing, unmodified Pairing flow
(`internal/pairing`, v1 auto-approve).

**Decided:** v1 MCP port ships with no auth (operator firewalls/VPNs it — matches the
existing v1 pairing auto-approve POC posture); admin UI is `headscale-admin`
(gurucomputing); Dockerfile builder stage uses official `golang:alpine`, not
`docker/alpine.Dockerfile`'s manual-tarball pattern (that pattern exists for its
bind-mounted dev workflow, which doesn't apply to a from-scratch prod image).

---

## Phase A — MCP transport: stdio → SSE/HTTP
*(repo: `lab-connect`)*

**File:** `cmd/lab-connect-mcp/main.go` only.

go-sdk v1.7.0's `NewStreamableHTTPHandler` is explicitly designed to share **one**
`*mcp.Server` across all connections ("It is OK for getServer to return the same
server multiple times" — see SDK's own `examples/http/main.go`). So:

- Keep building the single `*mcp.Server` and the single `refreshTools` goroutine
  exactly as today — no per-connection factory, no per-session refresh loop.
- **Delete** `newSessionID`, the `crypto/rand`/`encoding/hex` imports, and the
  `clientSessionID` parameter threaded through `run`/`refreshTools`/
  `addRunCommandTool`. Each MCP session already gets a real per-connection id for
  free: `mcp.CallToolRequest` embeds `Session *ServerSession`, and
  `ServerSession.ID()` returns it. Replace the `client.Run(ctx, peerNodeKey,
  clientSessionID, in.Argv)` call with `client.Run(ctx, peerNodeKey,
  req.Session.ID(), in.Argv)`. This is strictly more correct for the network case:
  many concurrent AI-agent clients now hit the same process, and CONTEXT.md's
  "Client Session" / the audit trail genuinely want one id per connected client.
- Add `-addr` flag / `LAB_CONNECT_MCP_ADDR` env (default `:8091`, following the
  `LAB_CONNECT_CONFIG_DIR` naming precedent in `internal/config/config.go`). Keep a
  `-stdio` fallback flag (default false) so local/dev usage is unaffected.
- Network mode:
  ```go
  handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
  return http.ListenAndServe(addr, handler)
  ```
  stdio mode keeps `server.Run(ctx, &mcp.StdioTransport{})`.
- Verify the exact `Session` field name/type against the vendored SDK source
  (`$(go env GOPATH)/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.7.0/mcp/`)
  before writing the diff — don't guess field spelling.

**Verify:** `go build ./cmd/lab-connect-mcp`; run with `-addr :8091` against a real
or stubbed `control.Client`; `curl -N -H 'Accept: text/event-stream'
http://localhost:8091` opens a session and lists tools; two concurrent connections
each get distinct `Mcp-Session-Id` values, and `run-command` calls from each show
distinct session ids in the target Node's Audit Entries. Add/extend a unit test in
`cmd/lab-connect-mcp` asserting distinct session ids per connection (no
`main_test.go` coverage of this exists yet).

---

## Phase B — Non-interactive `init` bootstrap
*(repo: `lab-connect`)*

**Files:** `cmd/lab-connect/main.go` (`newInitCmd`) only — no branching added inside
`internal/wizard/init.go` itself; reuse `wizard.Init`, `ensureValidAPIKey`,
`selectOrCreateHeadscaleUser`, `repairStaleJoin` unchanged. The only thing that
changes is *how the three `Deps` callbacks are supplied* — today `tui.RunInit`
always builds interactive bubbletea-backed closures
(`internal/tui/wizard.go`). Non-interactive mode supplies a second, non-TUI
implementation of the same three closures.

- In `newInitCmd`, check `--non-interactive` flag (cobra, for parity with existing
  `--yes` on uninstall) or `LAB_CONNECT_NONINTERACTIVE=1` env (the Gateway
  entrypoint, in the other repo, is a shell script, so honor both). When set, skip
  `tui.RunInit`/`installAndOfferPairing` and call `wizard.Init` directly with:
  - `Confirm` → always `true` (no operator to ask; a non-interactive Gateway boot
    already decided to join its own local Headscale).
  - `Prompt` → returns `LAB_CONNECT_HEADSCALE_API_KEY` on its *first* call, `""` on
    any subsequent call. This makes a genuinely-rejected key fail hard (hits
    `ensureValidAPIKey`'s empty-key error path) instead of spinning forever
    re-prompting with no stdin.
  - `SelectOne` → always index 0 (the Gateway's bundled Headscale starts empty, so
    `selectOrCreateHeadscaleUser` auto-creates `default` anyway; this only fires on
    edge-case re-register/multi-user paths).
  - `Out` → `cmd.OutOrStdout()` (no bubbletea program to route through).
  - Do **not** call `service.Install`/`service.Start` in this branch — the Gateway's
    supervisord config (Phase C) declares the runner program directly; calling
    `service.Install` here would spin up a second, redundant supervisord instance.

**Verify:** unit test exercising `wizard.Init` with the non-interactive closures
against `internal/testutil`'s throwaway Headscale container fixture (already used by
`internal/wizard`/`internal/headscale` tests), asserting zero required stdin.
Manually: `LAB_CONNECT_NONINTERACTIVE=1 LAB_CONNECT_HEADSCALE_API_KEY=<key>
lab-connect init` against a local `headscale serve` exits 0 with no prompt.

---

## Phase C — Gateway image: Dockerfile + entrypoint + supervisord + compose
*(repo: `lab-connect-depot`)*

**New files, all in `lab-connect-depot`:**
- `Dockerfile`
- `entrypoint.sh`
- `supervisord.conf`
- `docker-compose.yml`

**Build-time coupling to `lab-connect` — superseded, see below.** This section
originally recommended a git submodule; that was tried and reverted (operator
feedback: it made `lab-connect-depot` look like it vendored a full copy of
`lab-connect`, and pulled in more coupling than this split actually needs).
**Decided instead:** no source dependency between the repos at all.
- The Runner binary is installed via lab-connect's own `scripts/install.sh`
  against a pinned `LAB_CONNECT_VERSION` release build-arg — the same path any
  operator uses, not a from-source build.
- `lab-connect-mcp`'s source moved into *this* repo (`cmd/lab-connect-mcp`),
  since lab-connect's release pipeline never shipped that binary anyway (only
  the Runner). It duplicates lab-connect's small `internal/control` wire
  protocol (a plain HTTP-over-Unix-socket client) rather than importing it —
  Go `internal/` packages can't be imported across repos/modules, and
  duplicating ~30 lines of client code was judged simpler than exporting a
  public package from lab-connect for one caller. See
  `cmd/lab-connect-mcp/main.go`'s doc comment.

This means: **`LAB_CONNECT_VERSION` must point at a release that includes
Phase B's `init --non-interactive`** — `entrypoint.sh` depends on that flag
existing. (v0.0.2-dev is the first release that has it.)

**Dockerfile** — two stages:
1. Builder: `golang:alpine`, `go build -o /out/lab-connect-mcp
   ./cmd/lab-connect-mcp` from this repo's own source (its own `go.mod`, no
   dependency on lab-connect's module).
2. Final: `alpine`, `apk add supervisor curl bash busybox-extras`; install the
   Runner via lab-connect's `scripts/install.sh` (pinned `LAB_CONNECT_VERSION`);
   fetch the `headscale` release binary for `$TARGETARCH` (headscale has
   server-only deps — sqlite driver etc. — so it's a prebuilt binary, not built
   from source); fetch `headscale-admin`'s static build (pinned release
   tarball) to `/srv/headscale-admin`, served by `busybox-extras httpd -f -p
   8092 -h /srv/headscale-admin` (Alpine's base busybox omits the `httpd`
   applet — `busybox-extras` adds it as a separate binary); copy
   `lab-connect-mcp`, `entrypoint.sh`, `supervisord.conf`; `ENTRYPOINT
   ["/entrypoint.sh"]`.

**`entrypoint.sh`** — first-boot bootstrap, then hands off to supervisord:
1. Start `headscale serve` in the background, wait for `/health`.
2. Check `headscale apikeys list` for a still-valid key before minting a new one
   (`apikeys create` isn't idempotent — repeated calls on every restart would pile
   up unused keys). If listing can't cleanly detect reusability, persist the minted
   key to the same volume backing `LAB_CONNECT_CONFIG_DIR` and check there first.
3. Export it as `LAB_CONNECT_HEADSCALE_API_KEY`, run non-interactive `lab-connect
   init` (Phase B, the install.sh-installed Runner binary) against `http://localhost:8080`
   (reuses lab-connect's `substrate.DefaultCandidates()` existing loopback:8080
   entry — zero wizard code change needed for discovery). lab-connect's existing
   Stale-Join-repair path (ADR 0001) makes this idempotent on restart: warm boot
   finds `cfg.Joined()` true and resumes instead of re-registering.
4. `exec supervisord -c supervisord.conf -n` as the final foreground process —
   headscale keeps running under supervisord's own `[program:headscale]` block
   (same config/db path, not a second instance).

**`supervisord.conf`** — four `[program:]` blocks styled after
`supervisordConfContent` in lab-connect's `internal/service/service.go`:
`headscale` (`command=headscale serve`), `admin-ui` (`command=busybox httpd -f -p
8092 -h /srv/headscale-admin`), `lab-connect-runner` (`command=/usr/local/bin/
lab-connect runner`), `lab-connect-mcp` (`command=/usr/local/bin/lab-connect-mcp
-addr :8091`); each `autostart=true autorestart=true stopsignal=TERM` with
per-program log files, plus standard `[supervisord]`/`[unix_http_server]`/
`[rpcinterface:supervisor]`/`[supervisorctl]` sections.

**`docker-compose.yml`** (in `lab-connect-depot`, this repo's own top-level compose
— not to be confused with `lab-connect`'s dev-only one) — one `gateway` service,
`build: {context: .}`, publishing 8080 (headscale), 8092 (admin UI), 8091 (MCP SSE),
named volumes for headscale's sqlite state dir and for `LAB_CONNECT_CONFIG_DIR`
(reusing lab-connect's existing env var) so state survives recreation.

**Verify:** `docker compose up -d`; `curl localhost:8080/health` healthy; `curl -s
localhost:8092` serves the admin UI; `curl -N -H 'Accept: text/event-stream'
localhost:8091` opens an MCP session; restart the container and confirm no
duplicate Headscale users/keys and the Runner resumes (not re-registers); join one
real external lab-connect Node against this Gateway's published Headscale URL and
confirm it appears as a Peer, and a Pairing completes end to end exactly as two
ordinary Runners pair today.

---

## Phase D — Docs
*(split across both repos)*

In `lab-connect`:
- `docs/adr/0002-gateway-embeds-headscale.md`, styled like ADR 0001: what changed
  (a new "Gateway" deployment mode, built in the separate `lab-connect-depot` repo,
  owns Headscale's process lifecycle), why (turnkey homelab entry point), and
  explicit that this does **not** reverse "wrap Headscale, don't rebuild it"
  (DESIGN.md Phase 0) — only changes *who runs the `headscale` process*, for this
  one deployment mode.
- `PRD.md`: amend the "Headscale/Tailscale setup — out of scope" row to note the
  Gateway image (in `lab-connect-depot`) as the scoped exception, pointing at ADR
  0002 — don't rewrite the row, the per-Node path's decision is still correct.
- `DESIGN.md` Phase 0: add a note (not a rewrite) that the Gateway image manages
  Headscale's process lifecycle, pointing at ADR 0002.
- `CONTEXT.md`: new **Gateway** term (terse Term/Avoid style matching existing
  entries): "A deployable container (built in the separate `lab-connect-depot`
  repo) bundling a Headscale instance, its admin UI, and an embedded lab-connect
  Runner + MCP server, for homelabs with no Headscale already running. A Gateway
  *is* a Node — its embedded Runner joins its own bundled Headscale like any other
  — but it also owns the Overlay Network's control plane. _Avoid_: Server
  (ambiguous with the MCP server specifically)."

In `lab-connect-depot`:
- `README.md`: what this image is, how it relates to `lab-connect` (submodule
  dependency), quickstart (`docker compose up`), and the same "no MCP auth in v1,
  firewall/VPN this port" note stated plainly rather than buried — the bundled
  MCP's `run-command` tool is unscoped full-shell (inherited from lab-connect's own
  tool description) and is now network-reachable at 8091, not just local-socket
  reachable.

**Verify:** the doc files read consistently across both repos — nobody reading
`lab-connect`'s PRD.md alone still believes Headscale setup is unconditionally out
of scope; ADR cross-referenced from PRD.md and DESIGN.md; `lab-connect-depot`'s
README states the no-auth posture up front.

---

## Phase E — Audit log (SQLite, `lab-connect-mcp`)
*(repo: `lab-connect-depot`, decided via grilling session)*

**Why:** lab-connect's own per-Node Audit Entry (JSON-lines, `internal/rpc/audit.go`)
only ever covers one machine at a time and has no notion of *which AI agent* made
the call. `lab-connect-mcp` sees every call across every paired machine and the
MCP handshake's `ClientInfo`, so it's the natural place for a unified log — see
`CONTEXT.md`'s "Audit log" / "Actor" / "Target" entries (this repo) for the settled
vocabulary.

**New file:** `cmd/lab-connect-mcp/audit.go`. **New dependency:**
`modernc.org/sqlite` (pure-Go, no cgo — keeps the Dockerfile's `CGO_ENABLED=0`
alpine builder stage unchanged).

**Table `audit_log`:**

| column | type | notes |
|---|---|---|
| `id` | `INTEGER PRIMARY KEY` | autoincrement rowid |
| `timestamp` | `TEXT NOT NULL` | RFC 3339, written at call time |
| `source` | `TEXT NOT NULL DEFAULT 'gateway'` | reserved value for a future push of lab-connect's own per-Node Audit Entries into this same table |
| `tool` | `TEXT NOT NULL` | `machines` / `execute` / `transport` |
| `actor_name` | `TEXT` | `ClientInfo.Name` from the MCP handshake — self-reported, not authenticated |
| `actor_version` | `TEXT` | `ClientInfo.Version` |
| `mcp_session_id` | `TEXT` | `req.Session.ID()` — same id threaded into the Node's own Audit Entry via `client.Run`, for future cross-referencing |
| `target_name` | `TEXT` | `NULL` for `machines` (lists all Nodes, acts on none) |
| `target_node_key` | `TEXT` | `NULL` for `machines`; resolved value from `pairedMachine`, not just the requested name |
| `command` | `TEXT` | the literal argv sent to the Runner (same value passed to `client.Run`); `NULL` for `machines` |
| `status` | `INTEGER NOT NULL` | the command's real exit code if it ran; `-1` if it never got one — connection error and pre-dispatch validation rejection (empty argv, unpaired machine, bad `direction`, oversized/invalid-base64 content) are both `-1`, distinguished via `detail`/`command`/`target_name`, not a second status code |
| `detail` | `TEXT` | the error/rejection message only — never command output (output can be large and may carry secrets, e.g. a downloaded file's content) |

**Behavior:**
- Every call to all three tools (`machines`, `execute`, `transport`) gets exactly
  one row, including calls rejected before any Runner dispatch.
- Row is written synchronously, right before the tool returns its `CallToolResult`.
- **Fail-open:** a write error is never surfaced to the caller and never fails the
  tool call — log to the process's own stderr and move on. An audit log that can
  block or fail the thing it's watching is worse than an occasional missed row.
- DB file path: new env var `LAB_CONNECT_MCP_DATA_DIR` (default
  `~/.config/lab-connect-mcp/audit.db`), **not** `LAB_CONNECT_CONFIG_DIR` — that
  dir belongs to the Runner; this process owns its own data file even though both
  happen to run in the same Gateway container.

**Verify:** unit test in `cmd/lab-connect-mcp` covering: a successful `execute` row
(`status` = real exit code), a non-zero-exit `execute` row (`status` = that exit
code, not `-1`), a rejected `execute` row (unpaired machine → `status = -1`,
`detail` set), a `machines` row (`target_name`/`target_node_key`/`command` all
`NULL`), and that a broken/unwritable DB path doesn't fail the tool call itself.

---

## Phase F — Audit log UI + API (React, port 4224)
*(repo: `lab-connect-depot`)*

**Why:** Phase E's `audit_log` table is only queryable by hand (sqlite3
against the running container). This phase adds an operator-facing viewer.

**New directory:** `ui/` — a Vite + React app (`ui/src/App.jsx`), built to
static files at `ui/dist/` (`npm run build`).

**New file:** `cmd/lab-connect-mcp/audit_api.go` — `GET /api/audit-log`
(`?limit=&offset=`, defaults 25 / max 500, newest-first) returning
`{rows: [...], total: N}`; every `audit_log` column round-trips as JSON,
`NULL` columns as JSON `null`. Served, same-origin, alongside the built
`ui/dist` static files (`http.FileServer`) by a second HTTP listener the
`lab-connect-mcp` process itself owns — not a separate binary or busybox
httpd instance like `headscale-admin`'s, because the API needs the same
process's open `*sql.DB` handle. New flags/env: `-ui-addr` /
`LAB_CONNECT_MCP_UI_ADDR` (default `:4224`), `-ui-dir` /
`LAB_CONNECT_MCP_UI_DIR` (default `/srv/audit-ui`, matching the
`/srv/headscale-admin` convention already in the Dockerfile). Runs via
`golang.org/x/sync/errgroup` alongside the MCP listener in both stdio and
HTTP transport modes.

**UI:** page-size dropdown (25/50/100/200), prev/next pagination, each row
expandable (click) to show the full row — id, source, actor_version,
mcp_session_id, target_node_key, command, detail — while the collapsed row
shows only timestamp/tool/target/actor/status; dark/light toggle
(`prefers-color-scheme` default, persisted to the viewer's own
`localStorage`, not shared server state).

**Dockerfile:** new `ui-builder` stage (`node:22-alpine`, `npm ci && npm
run build`), `COPY --from=ui-builder /src/ui/dist /srv/audit-ui` in the
final stage; `EXPOSE` gains `4224`. `supervisord.conf`'s existing
`[program:lab-connect-mcp]` block needs no change — the UI/API listener is
the same binary, same process, just a second port.

**Security note carried into README:** 4224 has the same v1 no-auth
posture as 8091 — it can read every audit row, including command argv and
rejection `detail`. Same firewall/VPN guidance applies to both ports.

**Verify:** Go test covering `/api/audit-log` pagination (newest-first
ordering, limit clamping, `NULL` → JSON `null`); `npm run build` succeeds
with no errors; manually load `http://localhost:4224`, confirm the table
renders, page-size/pagination/expand/dark-mode all work against a
container with seeded rows.

---

## Order

A → B → C → D. A and B (in `lab-connect`) are independently testable without
Docker. C (in `lab-connect-depot`) depends on both being done and merged, since the
submodule pin needs to point at a commit that has them. D can be written alongside
C once the shape is real. E (audit log) is independent of A/B/C/D — it only touches
`cmd/lab-connect-mcp` in this repo — and can land any time after this repo has that
source (i.e. any time now). F (audit log UI) depends on E's `audit_log` table
existing, but is otherwise independent of A/B/C/D too.
