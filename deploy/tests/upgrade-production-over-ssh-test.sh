#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
script="$repo_root/deploy/upgrade-production-over-ssh.sh"

[ -f "$script" ]
[ -x "$script" ]
bash -n "$script"

help_output=$("$script" --help)
printf '%s\n' "$help_output" | grep -F -- '--host' >/dev/null
printf '%s\n' "$help_output" | grep -F -- '--install-dir' >/dev/null
printf '%s\n' "$help_output" | grep -F -- '--commit' >/dev/null
printf '%s\n' "$help_output" | grep -F -- '--pool-file' >/dev/null
printf '%s\n' "$help_output" | grep -F -- '--dry-run' >/dev/null

for forbidden in \
  'docker compose down -v' \
  'docker volume prune' \
  'docker system prune --volumes'
do
  if grep -Fq "$forbidden" "$script"; then
    printf 'forbidden destructive command found: %s\n' "$forbidden" >&2
    exit 1
  fi
done

for required in \
  'docker-compose.local.yml' \
  'postgres_data' \
  'redis_data' \
  'pg_dump' \
  'VPNGATE_MASTER_SECRET' \
  'VPNGATE_API_TOKEN' \
  'docker-compose.upgrade.yml' \
  'docker compose'
do
  grep -Fq "$required" "$script" || {
    printf 'required safety anchor missing: %s\n' "$required" >&2
    exit 1
  }
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-upgrade-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
printf 'not a pool\n' > "$tmp/pool.yaml"

if "$script" \
  --install-dir "$tmp" \
  --commit 14dbe8219e8369ff5377c1ffef16443d8908f3d2 \
  --pool-file "$tmp/pool.yaml" \
  --dry-run >/dev/null 2>&1; then
  printf '%s\n' 'missing --host must fail' >&2
  exit 1
fi

printf 'upgrade script safety test passed\n'
