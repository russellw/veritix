#!/usr/bin/env bash
# Run the browser tests against a freshly built Veritix binary.
#
# Builds the web interface and the server, starts `veritix serve` on a throwaway
# data directory, waits for it, runs Playwright against it, and always stops the
# server and removes the directory afterwards.
#
# The data directory is a fresh mktemp every run and is deleted on exit. These
# tests upload fixtures and audit them, and a suite that accumulated datasets in
# somebody's real data directory would be a nasty surprise.
#
# Overridable:
#   BASE_URL   where Playwright looks (default http://localhost:8080)
#   PORT       what the server binds (default 8080, derived from BASE_URL)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

BASE_URL="${BASE_URL:-http://localhost:8080}"
host_port="${BASE_URL#*://}"
host_port="${host_port%%/*}"
host="${host_port%%:*}"
port="${host_port##*:}"

data_dir="$(mktemp -d -t veritix-e2e-XXXXXX)"

echo "==> Building the web interface and the server"
make release

echo "==> Starting veritix serve on ${host}:${port} (data dir ${data_dir})"
./bin/veritix serve --addr "${host}:${port}" --data-dir "$data_dir" &
server_pid=$!

cleanup() {
	kill "$server_pid" 2>/dev/null || true
	wait "$server_pid" 2>/dev/null || true
	rm -rf "$data_dir"
}
trap cleanup EXIT

echo "==> Waiting for the server"
for _ in $(seq 1 30); do
	if ! kill -0 "$server_pid" 2>/dev/null; then
		echo "the server exited before it was ready" >&2
		exit 1
	fi
	if curl -fsS "${BASE_URL}/api/v1/health" >/dev/null 2>&1; then
		break
	fi
	sleep 1
done

echo "==> Running the browser tests"
cd "$repo_root/e2e"
BASE_URL="$BASE_URL" corepack pnpm test
