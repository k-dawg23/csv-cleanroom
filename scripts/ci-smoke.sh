#!/usr/bin/env bash
set -euo pipefail
bin="$(mktemp -d)"
pid=''
cleanup() {
  status=$?
  trap - EXIT
  if [[ -n "$pid" ]]; then
    kill -TERM "$pid" 2>/dev/null || true
    for _ in {1..100}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
      echo 'Smoke process did not stop' >&2
      kill -KILL "$pid" 2>/dev/null || true
      status=1
    fi
    wait "$pid" || status=1
  fi
  rm -rf "$bin"
  exit "$status"
}
trap cleanup EXIT
go build -buildvcs=false -o "$bin/csv-cleanroom" ./cmd/csv-cleanroom
"$bin/csv-cleanroom" >"$bin/stdout" 2>"$bin/stderr" &
pid=$!
ready=0
for _ in {1..100}; do
  if ! kill -0 "$pid" 2>/dev/null; then cat "$bin/stderr" >&2; exit 1; fi
  if grep -Fxq "CSV Cleanroom: http://127.0.0.1:8080" "$bin/stdout" && curl --fail --silent --max-time 1 http://127.0.0.1:8080/ >"$bin/page"; then ready=1; break; fi
  sleep 0.1
done
[[ "$ready" == 1 ]] || { echo 'Smoke readiness timed out' >&2; exit 1; }
grep -q 'CSV Cleanroom' "$bin/page"
kill -0 "$pid"
echo 'HTTP homepage smoke passed'
