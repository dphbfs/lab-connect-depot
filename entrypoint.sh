#!/bin/sh
# Gateway first-boot bootstrap, then hands off to supervisord. See
# depot-plan.md Phase C.
set -eu

CONFIG_DIR="${LAB_CONNECT_CONFIG_DIR:-/data/lab-connect}"
APIKEY_FILE="$CONFIG_DIR/gateway-headscale-apikey"
mkdir -p "$CONFIG_DIR"
export LAB_CONNECT_CONFIG_DIR="$CONFIG_DIR"

echo "==> starting headscale"
/usr/local/bin/headscale serve -c /etc/headscale/config.yaml &
HEADSCALE_PID=$!

echo "==> waiting for headscale /health"
i=0
until curl -fsS http://127.0.0.1:8080/health >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "headscale did not become healthy within 60s" >&2
		exit 1
	fi
	sleep 1
done

# apikeys create isn't idempotent — repeated calls on every restart would
# pile up unused keys — and apikeys list never returns the secret itself
# (only its prefix/expiration), so reusability can't be checked from
# headscale's own state alone. Persist the minted key to the same volume
# backing LAB_CONNECT_CONFIG_DIR and check there first; only fall back to
# minting a new one if no persisted key exists or headscale no longer
# lists its prefix as valid (revoked/expired).
API_KEY=""
if [ -f "$APIKEY_FILE" ]; then
	CANDIDATE="$(cat "$APIKEY_FILE")"
	# A Headscale API key is "hskey-api-<12char-id>-<secret>"; `apikeys
	# list` reports the prefix as "hskey-api-<12char-id>" (the first 3
	# hyphen-separated fields), not the whole key up to the first hyphen —
	# the secret itself may contain hyphens too.
	PREFIX="$(printf '%s' "$CANDIDATE" | cut -d'-' -f1-3)"
	# apikeys list renders each key as `"prefix": "<prefix>-***"` (the
	# "-***" is a literal masked-secret placeholder, not part of the id).
	if headscale apikeys list -o json 2>/dev/null | grep -q "\"prefix\": \"$PREFIX-\*\*\*\""; then
		API_KEY="$CANDIDATE"
		echo "==> reusing persisted Headscale API key"
	fi
fi
if [ -z "$API_KEY" ]; then
	echo "==> minting a new Headscale API key"
	API_KEY="$(headscale apikeys create --expiration 8760h)"
	printf '%s' "$API_KEY" >"$APIKEY_FILE"
	chmod 600 "$APIKEY_FILE"
fi
export LAB_CONNECT_HEADSCALE_API_KEY="$API_KEY"

# lab-connect's existing Stale-Join-repair path (ADR 0001) makes this
# idempotent on restart: a warm boot finds cfg.Joined() true and resumes
# instead of re-registering. Reuses lab-connect's own
# substrate.DefaultCandidates() loopback:8080 entry — no wizard code
# change needed for this container to discover its own bundled Headscale.
echo "==> running lab-connect init --non-interactive"
LAB_CONNECT_NONINTERACTIVE=1 /usr/local/bin/lab-connect init --non-interactive

echo "==> stopping standalone headscale (supervisord takes over)"
kill "$HEADSCALE_PID"
wait "$HEADSCALE_PID" 2>/dev/null || true

mkdir -p /var/log/supervisor
echo "==> handing off to supervisord"
exec supervisord -c /etc/supervisord.conf -n
