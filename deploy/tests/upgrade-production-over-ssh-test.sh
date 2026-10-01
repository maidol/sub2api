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
  --commit 3d308dcb6564534031cd408813f36a1ecf5d1484 \
  --pool-file "$tmp/pool.yaml" \
  --dry-run >/dev/null 2>&1; then
  printf '%s\n' 'missing --host must fail' >&2
  exit 1
fi

for required in 'vpngate_pool' 'vpngate-refresh' 'VPNGATE_POOL_POLL_INTERVAL'; do
  grep -Fq "$required" "$script" || {
    printf 'required anchor missing: %s\n' "$required" >&2
    exit 1
  }
done

# Execute the local side for real against a fake ssh that runs whatever it is
# sent. The remote procedure must start (and stop at its first preflight
# check, because the install directory does not exist), and with no
# --pool-file nothing may be uploaded as a pool.
expected_commit=$(sed -n 's/^readonly EXPECTED_COMMIT="\(.*\)"$/\1/p' "$script")
if git -C "$repo_root" cat-file -e "${expected_commit}^{commit}" 2>/dev/null; then
  clone="$tmp/clone"
  git clone -q --shared --no-checkout "$repo_root" "$clone"
  git -C "$clone" sparse-checkout set deploy >/dev/null 2>&1
  git -C "$clone" checkout -q HEAD
  cp "$script" "$clone/deploy/upgrade-production-over-ssh.sh"
  git -C "$clone" -c user.name=test -c user.email=test@example.invalid \
    commit -q -am 'test: working-tree upgrade script' >/dev/null 2>&1 || true

  fakebin="$tmp/fakebin"
  mkdir -p "$fakebin"
  cat > "$fakebin/ssh" <<'EOF'
#!/bin/sh
printf 'ssh %s\n' "$*" >> "$FAKE_SSH_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|-p|-i) shift 2 ;;
    *) break ;;
  esac
done
shift # host
if [ "${1:-}" = bash ]; then
  shift
  exec bash "$@"
fi
exit 0
EOF
  cat > "$fakebin/scp" <<'EOF'
#!/bin/sh
printf 'scp %s\n' "$*" >> "$FAKE_SSH_LOG"
exit 0
EOF
  chmod +x "$fakebin/ssh" "$fakebin/scp"

  : > "$tmp/ssh.log"
  status=0
  out=$(cd "$clone" && FAKE_SSH_LOG="$tmp/ssh.log" TMPDIR="$tmp" PATH="$fakebin:$PATH" \
    ./deploy/upgrade-production-over-ssh.sh --host test-host \
      --install-dir /nonexistent/sub2api-upgrade-test 2>&1) || status=$?
  if [ "$status" -eq 0 ]; then
    printf 'an upgrade against a missing install directory must fail\n' >&2
    exit 1
  fi
  printf '%s\n' "$out" | grep -F 'production docker-compose.local.yml is missing' >/dev/null || {
    printf 'the remote procedure never ran; output was:\n%s\n' "$out" >&2
    exit 1
  }
  grep -F 'bash -s -- --remote /nonexistent/sub2api-upgrade-test' "$tmp/ssh.log" >/dev/null || {
    printf 'the remote call does not pass --remote first:\n' >&2
    cat "$tmp/ssh.log" >&2
    exit 1
  }
  if grep -F 'mihomo-openvpn.yaml' "$tmp/ssh.log" >/dev/null; then
    printf 'no --pool-file was given, so no pool may be uploaded\n' >&2
    exit 1
  fi
else
  printf 'skipping the dispatch test: %s is not in this clone\n' "$expected_commit" >&2
fi

printf 'upgrade script safety test passed\n'
