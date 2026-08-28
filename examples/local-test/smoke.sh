#!/bin/sh
# End-to-end smoke test against the sandbox daemon started by start.sh.
# Exercises: validate, list, trigger+wait, tagged logs, failure path,
# timeout+grace kill, reload, status. Exits non-zero on any failure.
set -eu

. "$(dirname -- "$0")/common.sh"

load_env >/dev/null 2>&1
resolve_bin

if ! daemon_pid >/dev/null; then
	die "daemon is not running; run ./start.sh first"
fi

PASS=0
FAIL=0

ok() {
	echo "PASS: $1"
	PASS=$((PASS + 1))
}
bad() {
	echo "FAIL: $1" >&2
	FAIL=$((FAIL + 1))
}

# Extract a string field from the CLI's indented JSON output.
json_str() {
	grep -o "\"$1\": *\"[^\"]*\"" | head -n1 | sed 's/.*: *"//;s/"$//'
}
# Extract a numeric field.
json_num() {
	grep -o "\"$1\": *[0-9-]*" | head -n1 | sed 's/.*: *//'
}

echo "== 1. config validates"
if "$BIN" validate "$MINICRON_CONFIG" >/dev/null; then
	ok "validate"
else
	bad "validate"
fi

echo "== 2. all example definitions are loaded"
list=$("$BIN" list)
missing=
for name in hello heartbeat flaky slow ticker; do
	echo "$list" | grep -q "\"$name\"" || missing="$missing $name"
done
if [ -z "$missing" ]; then
	ok "list shows hello, heartbeat, flaky, slow, ticker"
else
	bad "list missing:$missing"
fi

echo "== 3. run hello to completion and read its tagged logs"
out=$("$BIN" run hello --wait)
run_id=$(printf '%s' "$out" | json_str run_id)
status=$(printf '%s' "$out" | json_str status)
if [ "$status" = "succeeded" ] && [ -n "$run_id" ]; then
	ok "run hello --wait -> succeeded ($run_id)"
else
	bad "run hello --wait -> status=${status:-?} run_id=${run_id:-?}"
fi
logs=$("$BIN" logs "$run_id")
if echo "$logs" | grep -q "hello from minicron example" && echo "$logs" | grep -q "run_id=$run_id"; then
	ok "logs contain greeting + injected MINICRON_RUN_ID"
else
	bad "logs missing expected output"
fi

echo "== 4. failure path: flaky exits 42 and is recorded as failed"
out=$("$BIN" run flaky --wait)
status=$(printf '%s' "$out" | json_str status)
code=$(printf '%s' "$out" | json_num exit_code)
if [ "$status" = "failed" ] && [ "$code" = "42" ]; then
	ok "run flaky --wait -> failed, exit_code=42"
else
	bad "run flaky --wait -> status=${status:-?} exit_code=${code:-?}"
fi

echo "== 5. timeout path: slow is killed after 3s (+2s grace)"
out=$("$BIN" run slow --wait)
status=$(printf '%s' "$out" | json_str status)
if [ "$status" = "timeout" ]; then
	ok "run slow --wait -> timeout"
else
	bad "run slow --wait -> status=${status:-?} (expected timeout)"
fi

echo "== 6. hot reload and daemon status"
if "$BIN" reload >/dev/null; then
	ok "reload"
else
	bad "reload"
fi
if "$BIN" status | grep -q '"version"'; then
	ok "status"
else
	bad "status"
fi

echo
echo "smoke: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
