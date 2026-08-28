#!/bin/sh
# Build minicron, prepare the sandbox, and start the daemon in the background.
# Reads .env (created from .env.example on first run). Idempotent: exits 0
# with status if the daemon is already running.
set -eu

. "$(dirname -- "$0")/common.sh"

load_env
resolve_bin
mkdir -p "$MINICRON_DATA"
write_env_local

if pid=$(daemon_pid); then
	echo "daemon already running (pid $pid)"
	echo "  ui:      http://127.0.0.1:7423"
	echo "  log:     $MINICRON_DATA/daemon.log"
	echo "  stop:    ./stop.sh"
	exit 0
fi

echo "==> validating config: $MINICRON_CONFIG"
"$BIN" validate "$MINICRON_CONFIG"

echo "==> starting daemon (data dir: $MINICRON_DATA)"
rm -f "$MINICRON_DATA/minicron.sock"
(
	cd "$EXAMPLE_DIR"
	nohup "$BIN" daemon --config "$MINICRON_CONFIG" --data-dir "$MINICRON_DATA" \
		>"$MINICRON_DATA/daemon.log" 2>&1 &
)

if ! wait_ready; then
	echo "daemon did not become ready; last log lines:" >&2
	tail -n 20 "$MINICRON_DATA/daemon.log" >&2 || true
	exit 1
fi

pid=$(daemon_pid) || true
echo "daemon ready (pid ${pid:-unknown})"
echo
echo "  web ui:   http://127.0.0.1:7423   (bearer token required; see below)"
echo "  log:      $MINICRON_DATA/daemon.log"
echo "  data:     $MINICRON_DATA"
echo "  smoke:    ./smoke.sh              (end-to-end test)"

# The initial bearer token is printed exactly once, on first boot only.
token=$(sed -n 's/.*token=\([^ ]*\).*/\1/p' "$MINICRON_DATA/daemon.log" | head -n1)
if [ -n "$token" ]; then
	echo
	echo "  initial bearer token (save it now, it cannot be recovered):"
	echo "      $token"
else
	echo
	echo "  bearer token: already issued earlier; rotate with './mc token --rotate'"
fi
echo
echo "  next: ./mc list                (CLI uses the unix socket, no token needed)"
echo "        ./mc run hello --wait"
