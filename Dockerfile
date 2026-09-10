# Gateway image: bundled Headscale + admin UI + the lab-connect Runner +
# lab-connect-mcp over SSE, all in one container. See depot-plan.md
# Phase C.
#
# lab-connect's own release pipeline (scripts/install.sh,
# .github/workflows/release.yml) only ever builds and ships the Runner
# binary (cmd/lab-connect) — never lab-connect-mcp. So this image installs
# the Runner via lab-connect's own install.sh from its GitHub Releases,
# and builds lab-connect-mcp from source that lives in this repo
# (cmd/lab-connect-mcp) — no cross-repo Go dependency, no submodule.
#
# LAB_CONNECT_VERSION must be a release that includes `init
# --non-interactive` (this Dockerfile's own requirement — the Gateway's
# entrypoint.sh depends on that flag existing) — check
# https://github.com/dphbfs/lab-connect/releases before bumping "latest".

ARG LAB_CONNECT_VERSION=v0.0.6-dev
ARG HEADSCALE_VERSION=0.29.3
ARG HEADSCALE_ADMIN_VERSION=0.25.6

# ---- ui-builder: compile the audit log UI (React/Vite) to static files -
FROM node:22-alpine AS ui-builder
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY ui/ ./
RUN npm run build

# ---- builder: compile lab-connect-mcp from this repo's own source -----
FROM golang:alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/lab-connect-mcp/ cmd/lab-connect-mcp/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/lab-connect-mcp ./cmd/lab-connect-mcp

# ---- final: alpine + supervisor + prebuilt headscale + admin UI -------
FROM alpine:3.20
ARG TARGETARCH
ARG LAB_CONNECT_VERSION
ARG HEADSCALE_VERSION
ARG HEADSCALE_ADMIN_VERSION

RUN apk add --no-cache ca-certificates supervisor curl bash busybox-extras

# The Runner binary (cmd/lab-connect in the lab-connect repo) — installed
# the same way any operator installs it, not built from source here.
RUN curl -fsSL https://raw.githubusercontent.com/dphbfs/lab-connect/main/scripts/install.sh \
      | LAB_CONNECT_VERSION="${LAB_CONNECT_VERSION}" LAB_CONNECT_INSTALL_DIR=/usr/local/bin bash

# headscale has server-only deps (sqlite driver, etc.) not relevant to
# either Go binary above, so it's a prebuilt release binary.
RUN curl -fsSL -o /usr/local/bin/headscale \
      "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${TARGETARCH}" && \
    chmod +x /usr/local/bin/headscale

# headscale-admin's static build, served by busybox-extras' httpd applet
# (Alpine's base busybox omits httpd; busybox-extras adds it as a separate
# /bin/busybox-extras binary) — no other runtime needed for a static SPA.
RUN mkdir -p /srv/headscale-admin && \
    curl -fsSL "https://github.com/GoodiesHQ/headscale-admin/releases/download/v${HEADSCALE_ADMIN_VERSION}/admin.tar.gz" \
      | tar -xz -C /srv/headscale-admin

# Baked in (not just exported by entrypoint.sh) so `docker exec`/`compose
# exec` sessions — which don't inherit entrypoint.sh's own shell-exported
# vars, only supervisord and its forked children do — also resolve the
# Runner's real control socket at /data/lab-connect/control.sock instead
# of falling back to ~/.config/lab-connect (empty, no Runner there) and
# wrongly reporting "lab-connect runner is not running". Still overridable
# via docker-compose.yml's environment: like any other ENV default.
ENV LAB_CONNECT_CONFIG_DIR=/data/lab-connect

COPY --from=builder /out/lab-connect-mcp /usr/local/bin/lab-connect-mcp
# Audit log UI's static build — served directly by lab-connect-mcp itself
# on its own port (default :4224, see cmd/lab-connect-mcp/audit_api.go's
# uiServer), not by busybox-extras httpd like headscale-admin above: it
# needs the audit_log JSON API served same-origin, so the same process
# that owns the SQLite file serves both.
COPY --from=ui-builder /src/ui/dist /srv/audit-ui
# Rendered into /etc/headscale/config.yaml by entrypoint.sh at container
# start, not baked in here — see the template's own header comment.
COPY headscale-config.yaml.template /etc/headscale/config.yaml.template
COPY entrypoint.sh /entrypoint.sh
COPY supervisord.conf /etc/supervisord.conf
RUN chmod +x /entrypoint.sh && mkdir -p /var/lib/headscale /var/log/supervisor

# 3478/udp is Headscale's embedded DERP server's STUN listener
# (headscale-config.yaml.template's derp.server) — without it, Peers that can't
# reach each other directly (NAT, or no direct UDP path at all, e.g.
# sibling containers on a plain Docker bridge network) can never pair or
# run commands: there's no rendezvous/relay path for the WireGuard
# handshake, and it fails silently as a connection timeout, not a clear
# error. See PRD.md ("still reachable... via DERP relay") in the
# lab-connect repo.
EXPOSE 8080 8091 8092 4224 3478/udp

ENTRYPOINT ["/entrypoint.sh"]
