#!/usr/bin/env bash
# Drives dibs end to end against hack/stub_github.py and prints the alerts the
# console transport produced, plus the final issue states. Deterministic, so
# two runs either side of a refactor diff to nothing.
#
#   hack/e2e.sh > before.txt
#   ...change code...
#   hack/e2e.sh > after.txt
#   diff before.txt after.txt
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'kill "${stub_pid:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
python3 hack/stub_github.py "$port" &
stub_pid=$!

cat > "$work/dibs.yaml" <<'YAML'
profile:
  github_login: testuser
  timezone: UTC
polling:
  default_interval_sec: 1
  min_interval_sec: 1
  freshness_cutoff_min: 60
slack:
  deliver_to: dm
YAML
cat > "$work/repos.yaml" <<'YAML'
repos:
  - slug: acme/widget
    notes: fixture
YAML

go build -o "$work/dibs" ./cmd/dibs

until curl -sf "http://127.0.0.1:$port/user" >/dev/null; do sleep 0.1; done

DIBS_CONFIG="$work/dibs.yaml" DIBS_DB="$work/dibs.db" \
DIBS_GITHUB_TOKEN=stub DIBS_GITHUB_API="http://127.0.0.1:$port" \
  timeout 12s "$work/dibs" run --dry-run > "$work/out.txt" 2> "$work/log.txt" || true

echo "=== alerts ==="
cat "$work/out.txt"

echo "=== states ==="
# The waterline is a wall-clock stamp, so blank it or every run differs.
DIBS_CONFIG="$work/dibs.yaml" DIBS_DB="$work/dibs.db" "$work/dibs" status --repos |
  sed -E 's/waterline [0-9T:Z-]+/waterline <t>/'

echo "=== rejections ==="
python3 hack/dump_db.py "$work/dibs.db"
