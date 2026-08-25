# lab-connect-depot

The **Gateway** image: a single Docker container bundling [Headscale](https://github.com/juanfont/headscale),
its [admin UI](https://github.com/GoodiesHQ/headscale-admin), and an embedded
[lab-connect](https://github.com/dphbfs/lab-connect) Runner + `lab-connect-mcp` MCP server
(`machines` / `execute` / `transport` — see "MCP tools" below).

Normally, using lab-connect means standing up a Headscale (or Tailscale) mesh
yourself first, then installing lab-connect on top of it. The Gateway collapses
that into one image: point AI agents at it and get a working Overlay Network,
Pairing, and MCP tool exposure with nothing pre-existing. It's just an ordinary
lab-connect Node under the hood — it joins its own bundled Headscale through the
same unmodified join/pairing flow every Node uses — that also happens to own
Headscale's process lifecycle. See `lab-connect`'s
`docs/adr/0002-gateway-embeds-headscale.md` for why.

## Relation to `lab-connect`

This is a separate repo from `lab-connect` on purpose: the Gateway is a
deployment/packaging concern, not a change to lab-connect itself. No source
dependency between the two repos:

- The **Runner** (`lab-connect` binary) is installed the same way any operator
  installs it — the Dockerfile runs lab-connect's own `scripts/install.sh`
  against a pinned `LAB_CONNECT_VERSION` release. Bump that build arg and
  rebuild to pick up a new lab-connect version.
- **`lab-connect-mcp`** is built from source that lives in *this* repo
  (`cmd/lab-connect-mcp`) — lab-connect's own release pipeline never ships
  that binary, only the Runner. It talks to the Runner's local control socket
  over the same small HTTP-over-Unix-socket protocol lab-connect's
  `internal/control` defines; this repo keeps its own minimal client rather
  than importing that package (Go `internal/` packages aren't importable
  across repos/modules), documented in `cmd/lab-connect-mcp/main.go`.

## MCP tools

`lab-connect-mcp` exposes a fixed three-tool surface — none of these are
added or removed at runtime; they just act on whatever is currently Paired:

- **`machines`** — lists the Paired Nodes currently reachable by `execute`
  and `transport`. An unpaired Node never shows up here, even if it exists
  on the Overlay Network.
- **`execute`** — `{machine, argv}`: runs a command on a paired machine.
  **Unscoped full-shell** — no allow-list, no command-class restriction.
  Every call is recorded as an Audit Entry on the target Node.
- **`transport`** — `{machine, direction, path, content?}`: copies a file
  onto (`upload`) or off of (`download`) a paired machine. There's no
  dedicated file-transfer RPC on the Runner, so this reuses `execute`'s
  RPC underneath (`content` travels as base64 in a single command
  argument) — capped at roughly 1MiB, fine for configs/scripts, not for
  large binaries.

All three re-resolve the machine name against the live Peer list on every
call — a machine that's unpaired or never existed is never actionable,
regardless of what an agent cached from an earlier `machines` call.

## ⚠️ No auth on the MCP port (v1)

The bundled `lab-connect-mcp` server listens on **8091 with no
authentication** — matches lab-connect's own v1 posture of auto-approving every
Pairing Request with no human gate (a single-operator POC trade-off, not
a production security model). Whatever reaches port 8091 can run arbitrary
commands (and read/write files up to the transport cap) on every Node this
Gateway is Paired with.

**Firewall or VPN this port. Do not expose it to the open internet.**

## Quickstart

```sh
git clone <this-repo>
cd lab-connect-depot
docker compose up -d
```

The compose file uses `network_mode: host` — the embedded Runner's overlay
node needs to be reachable by external Peers over WireGuard (UDP, real NAT
traversal), which a bridge network's TCP-only port-forwards don't provide.
This means Linux only for now; host networking doesn't work the same way
under Docker Desktop on Mac/Windows.

This builds the image, boots Headscale, self-mints a Headscale API Key,
joins the Gateway's own embedded Runner to it, and starts everything under
supervisord. On success:

- `http://localhost:8080/health` — Headscale
- `http://localhost:8092` — headscale-admin UI
- `http://localhost:8091` — `lab-connect-mcp`, SSE/HTTP transport (see the
  no-auth warning above)

Point another machine's `lab-connect init` at this Gateway's published
Headscale URL to join it as a Peer, then `lab-connect pair <name>` (v1
auto-approves) to give an AI agent access to it through this Gateway's MCP
server.

Restarting the container is idempotent: it resumes the existing Overlay
Network connection and reuses the persisted Headscale API Key instead of
re-registering or minting a new one.
