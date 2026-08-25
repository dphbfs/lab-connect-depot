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

ARG LAB_CONNECT_VERSION=latest
ARG HEADSCALE_VERSION=0.29.3
ARG HEADSCALE_ADMIN_VERSION=0.25.6

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

COPY --from=builder /out/lab-connect-mcp /usr/local/bin/lab-connect-mcp
COPY headscale-config.yaml /etc/headscale/config.yaml
COPY headscale-derp.yaml /etc/headscale/derp.yaml
COPY entrypoint.sh /entrypoint.sh
COPY supervisord.conf /etc/supervisord.conf
RUN chmod +x /entrypoint.sh && mkdir -p /var/lib/headscale /var/log/supervisor

EXPOSE 8080 8091 8092

ENTRYPOINT ["/entrypoint.sh"]
