# shellcheck shell=bash
# Shared helpers for start.sh / stop.sh / smoke.sh / mc.
# Not executable on its own; sourced by the other scripts.

EXAMPLE_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(cd -- "$EXAMPLE_DIR/../.." && pwd)

die() {
	echo "error: $*" >&2
	exit 1
}

notice() {
	echo "notice: $*" >&2
}

# Load .env (creating it from .env.example on first run), then absolutize
# path-valued variables relative to EXAMPLE_DIR and export them.
load_env() {
	if [ ! -f "$EXAMPLE_DIR/.env" ]; then
		cp "$EXAMPLE_DIR/.env.example" "$EXAMPLE_DIR/.env"
		notice "created .env from .env.example (edit it to change bind/data paths)"
	fi
	set -a
	# shellcheck disable=SC1091
	. "$EXAMPLE_DIR/.env"
	set +a
	case ${MINICRON_CONFIG:-} in
	"") die "MINICRON_CONFIG is empty in .env" ;;
	/*) ;;
	*) MINICRON_CONFIG="$EXAMPLE_DIR/$MINICRON_CONFIG" ;;
	esac
	case ${MINICRON_DATA:-} in
	"") die "MINICRON_DATA is empty in .env" ;;
	/*) ;;
	*) MINICRON_DATA="$EXAMPLE_DIR/$MINICRON_DATA" ;;
	esac
	export MINICRON_CONFIG MINICRON_DATA
}

# Resolve the minicron binary to use and set $BIN.
# Order: $MINICRON_BIN override, then build from source, then repo bin/.
resolve_bin() {
	if [ -n "${MINICRON_BIN:-}" ]; then
		[ -x "$MINICRON_BIN" ] || die "MINICRON_BIN=$MINICRON_BIN is not executable"
		BIN=$MINICRON_BIN
		return
	fi
	if command -v go >/dev/null 2>&1; then
		mkdir -p "$EXAMPLE_DIR/.bin"
		if [ ! -x "$EXAMPLE_DIR/.bin/minicron" ]; then
			notice "building minicron from source (override with MINICRON_BIN in .env)"
		fi
		(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -o "$EXAMPLE_DIR/.bin/minicron" ./cmd/minicron) \
			|| die "go build failed"
		BIN=$EXAMPLE_DIR/.bin/minicron
		return
	fi
	if [ -x "$REPO_ROOT/bin/minicron" ]; then
		BIN=$REPO_ROOT/bin/minicron
		return
	fi
	die "no usable binary: install go, or set MINICRON_BIN in .env, or run 'make build' first"
}

# Echo the running daemon's pid, or fail if it is not running.
# The daemon writes its pid to $MINICRON_DATA/minicron.lock while holding flock.
daemon_pid() {
	[ -f "$MINICRON_DATA/minicron.lock" ] || return 1
	pid=$(head -n1 "$MINICRON_DATA/minicron.lock" 2>/dev/null)
	case $pid in
	'' | *[!0-9]*) return 1 ;;
	esac
	[ -r "/proc/$pid/comm" ] || return 1
	grep -qx minicron "/proc/$pid/comm" 2>/dev/null || return 1
	echo "$pid"
}

# Write env.local, consumed by the example jobs via `env_file` so they can
# locate their scripts with an absolute path ($EXAMPLE_DIR) without baking
# one into the checked-in TOML. The daemon reads env_file relative to its
# own working directory, and start.sh always starts it from EXAMPLE_DIR.
write_env_local() {
	printf 'EXAMPLE_DIR=%s\n' "$EXAMPLE_DIR" >"$EXAMPLE_DIR/env.local"
}

wait_ready() {
	i=0
	while [ "$i" -lt 15 ]; do
		if "$BIN" status >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}
