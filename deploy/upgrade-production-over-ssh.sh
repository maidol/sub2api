#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly EXPECTED_COMMIT="14dbe8219e8369ff5377c1ffef16443d8908f3d2"
readonly EXPECTED_TREE="7198ad3e2c02050d37a508cfeb08d91e85d02ab5"
readonly BASE_COMPOSE_NAME="docker-compose.local.yml"

log() {
  printf '[upgrade] %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Usage:
  deploy/upgrade-production-over-ssh.sh --host deploy-prod \
    --install-dir /home/ubuntu/data/sub2api \
    --pool-file /secure/path/mihomo-openvpn.yaml \
    [--commit 14dbe8219e8369ff5377c1ffef16443d8908f3d2] \
    [--project-name deploy] [--ssh-port 22] [--identity ~/.ssh/id_ed25519] \
    [--server-port 8080] [--dry-run]

The script deploys only the verified proxy-pool commit, requires a clean local
worktree, never deletes data volumes, and never prints secrets.
USAGE
}

remote_die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

remote_log() {
  printf '[remote-upgrade] %s\n' "$*"
}

remote_main() {
  set -Eeuo pipefail
  umask 077

  local root=$1
  local commit=$2
  local archive_sha=$3
  local pool_sha=$4
  local remote_tmp=$5
  local requested_project=${6:-}
  local server_port=${7:-8080}
  local prod_deploy="$root/deploy"
  local env_file="$prod_deploy/.env"
  local base_compose="$prod_deploy/$BASE_COMPOSE_NAME"
  local release="$root/releases/$commit"
  local backup="$root/backups/$(date -u +%Y%m%dT%H%M%SZ)-$commit"
  local upgrade_compose="$release/deploy/docker-compose.upgrade.yml"
  local project=""
  local token=""
  local lease_id=""
  local maintenance_started=0
  local rollback_done=0

  [ "${root#/}" != "$root" ] || remote_die 'install directory must be absolute'
  case "$root" in
    *[!A-Za-z0-9_./-]*) remote_die 'install directory contains unsafe characters' ;;
  esac
  case "$server_port" in
    ''|*[!0-9]*) remote_die 'server port must be numeric' ;;
  esac
  [ "$server_port" -ge 1 ] && [ "$server_port" -le 65535 ] \
    || remote_die 'server port must be between 1 and 65535'
  [ -f "$base_compose" ] || remote_die "production $BASE_COMPOSE_NAME is missing: $base_compose"
  [ -f "$env_file" ] || remote_die "production .env is missing: $env_file"
  [ -d "$prod_deploy/data" ] || remote_die 'production data directory is missing'
  [ -d "$prod_deploy/postgres_data" ] || remote_die 'production postgres_data directory is missing'
  [ -d "$prod_deploy/redis_data" ] || remote_die 'production redis_data directory is missing'
  [ -s "$remote_tmp/source.tar.gz" ] || remote_die 'uploaded source archive is missing'
  [ -s "$remote_tmp/mihomo-openvpn.yaml" ] || remote_die 'uploaded pool file is missing'

  command -v docker >/dev/null 2>&1 || remote_die 'remote docker is required'
  docker compose version >/dev/null 2>&1 || remote_die 'remote docker compose v2 is required'
  command -v jq >/dev/null 2>&1 || remote_die 'remote jq is required for lease smoke tests'
  command -v sha256sum >/dev/null 2>&1 || remote_die 'remote sha256sum is required'
  command -v stat >/dev/null 2>&1 || remote_die 'remote stat is required'
  command -v awk >/dev/null 2>&1 || remote_die 'remote awk is required'

  env_mode=$(stat -c '%a' "$env_file")
  case "$env_mode" in
    400|500|600|700) ;;
    *) remote_die ".env must not be group/world accessible (mode is $env_mode)" ;;
  esac

  has_env_value() {
    local key=$1
    awk -F= -v key="$key" \
      '$1 == key && length(substr($0, index($0, "=") + 1)) > 0 { found=1 }
       END { exit(found ? 0 : 1) }' "$env_file"
  }

  has_env_value VPNGATE_MASTER_SECRET \
    || remote_die 'VPNGATE_MASTER_SECRET is missing from remote .env'
  has_env_value VPNGATE_API_TOKEN \
    || remote_die 'VPNGATE_API_TOKEN is missing from remote .env'

  if [ -n "$requested_project" ]; then
    project=$requested_project
  else
    project=$(docker inspect sub2api \
      --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null || true)
  fi
  [ -n "$project" ] \
    || remote_die 'could not determine the existing Compose project name; pass --project-name'
  case "$project" in
    *[!A-Za-z0-9_-]*) remote_die 'Compose project name contains unsafe characters' ;;
  esac

  dc() {
    docker compose \
      --project-name "$project" \
      --env-file "$env_file" \
      -f "$base_compose" \
      -f "$upgrade_compose" \
      "$@"
  }

  dc_base() {
    docker compose \
      --project-name "$project" \
      --env-file "$env_file" \
      -f "$base_compose" \
      "$@"
  }

  dc_base config --quiet || remote_die 'existing production Compose configuration is invalid'

  mkdir -p "$root/releases" "$root/backups"
  mkdir -p "$backup"
  cp -p "$env_file" "$backup/.env"
  cp -p "$base_compose" "$backup/$BASE_COMPOSE_NAME"
  docker inspect sub2api --format '{{.Image}}' > "$backup/old-sub2api-image-id" 2>/dev/null || true
  docker inspect sub2api --format '{{index .Config.Labels "com.docker.compose.project"}}' \
    > "$backup/compose-project" 2>/dev/null || true
  docker ps --format '{{.Names}} {{.Image}} {{.Status}}' \
    | grep -E '^(sub2api|sub2api-postgres|sub2api-redis)( |$)' \
    > "$backup/old-container-status" || true

  remote_log "project=$project backup=$backup"
  remote_log 'creating PostgreSQL backup before the maintenance window'
  dc_base exec -T postgres sh -ceu \
    'pg_dump -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
    > "$backup/postgres.dump"
  [ -s "$backup/postgres.dump" ] || remote_die 'PostgreSQL backup is empty'

  test "$(sha256sum "$remote_tmp/source.tar.gz" | awk '{print $1}')" = "$archive_sha" \
    || remote_die 'source archive checksum mismatch'
  test "$(sha256sum "$remote_tmp/mihomo-openvpn.yaml" | awk '{print $1}')" = "$pool_sha" \
    || remote_die 'VPN Gate pool checksum mismatch'

  rm -rf "$release"
  mkdir -p "$release"
  tar -xzf "$remote_tmp/source.tar.gz" -C "$release" --strip-components=1
  mkdir -p "$release/deploy/vpngate/pool"
  install -m 0644 "$remote_tmp/mihomo-openvpn.yaml" \
    "$release/deploy/vpngate/pool/mihomo-openvpn.yaml"
  [ -s "$release/deploy/vpngate/pool/mihomo-openvpn.yaml" ] \
    || remote_die 'staged VPN Gate pool file is empty'

  cat > "$upgrade_compose" <<EOF
services:
  sub2api:
    image: sub2api:source-$commit
    build:
      context: "$release"
      dockerfile: Dockerfile
      args:
        COMMIT: $commit

  vpngate:
    image: sub2api-vpngate:source-$commit
    build:
      context: "$release"
      dockerfile: deploy/vpngate/Dockerfile
    container_name: sub2api-vpngate
    restart: unless-stopped
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
    environment:
      VPNGATE_MASTER_SECRET: "\${VPNGATE_MASTER_SECRET:?VPNGATE_MASTER_SECRET is required}"
      VPNGATE_API_TOKEN: "\${VPNGATE_API_TOKEN:?VPNGATE_API_TOKEN is required}"
      VPNGATE_API_LISTEN: "\${VPNGATE_API_LISTEN:-0.0.0.0:20000}"
      VPNGATE_PUBLIC_HOST: "\${VPNGATE_PUBLIC_HOST:-vpngate}"
      VPNGATE_SLOTS: "\${VPNGATE_SLOTS:-2}"
      VPNGATE_BASE_PORT: "\${VPNGATE_BASE_PORT:-20001}"
      VPNGATE_PROBE_URL: "\${VPNGATE_PROBE_URL:-https://www.gstatic.com/generate_204}"
      VPNGATE_PROBE_TIMEOUT: "\${VPNGATE_PROBE_TIMEOUT:-10s}"
      VPNGATE_PROBE_INTERVAL: "\${VPNGATE_PROBE_INTERVAL:-60s}"
      VPNGATE_FAIL_THRESHOLD: "\${VPNGATE_FAIL_THRESHOLD:-3}"
      VPNGATE_COOLDOWN: "\${VPNGATE_COOLDOWN:-30m}"
      VPNGATE_MAX_ATTEMPTS: "\${VPNGATE_MAX_ATTEMPTS:-5}"
      VPNGATE_MAX_NODES: "\${VPNGATE_MAX_NODES:-200}"
      VPNGATE_DNS: "\${VPNGATE_DNS:-1.1.1.1,8.8.8.8}"
      VPNGATE_CONTROLLER_ADDR: "\${VPNGATE_CONTROLLER_ADDR:-127.0.0.1:19090}"
    volumes:
      - "$release/deploy/vpngate/pool:/data/pool:ro"
      - vpngate_state:/data/state
    networks:
      - sub2api-network

volumes:
  vpngate_state:
    driver: local
EOF
  chmod 0600 "$upgrade_compose"

  dc config --quiet || remote_die 'merged upgrade Compose configuration is invalid'
  for service in sub2api postgres redis vpngate; do
    dc config --services | grep -Fx "$service" >/dev/null \
      || remote_die "$service service missing from merged Compose"
  done
  if dc config | awk '
    /^  vpngate:/ { seen=1; next }
    seen && /^  [A-Za-z0-9_-]+:/ { exit }
    seen && /ports:/ { found=1 }
    END { exit found ? 0 : 1 }
  '; then
    remote_die 'vpngate must not publish host ports'
  fi

  rollback() {
    local rollback_status=0
    local old_image=""
    local current_image=""
    [ "$rollback_done" -eq 0 ] || return 0
    rollback_done=1
    remote_log 'attempting application rollback; database will not be restored automatically'
    dc stop sub2api vpngate >/dev/null 2>&1 || rollback_status=1
    dc_base up -d --no-build sub2api >/dev/null 2>&1 || rollback_status=1
    if [ -s "$backup/old-sub2api-image-id" ]; then
      old_image=$(cat "$backup/old-sub2api-image-id")
      current_image=$(docker inspect sub2api --format '{{.Image}}' 2>/dev/null || true)
      [ "$old_image" = "$current_image" ] || rollback_status=1
    fi
    if [ "$rollback_status" -eq 0 ]; then
      remote_log 'application rollback completed'
    else
      remote_log 'application rollback needs manual attention; see backup receipt'
    fi
    return "$rollback_status"
  }

  cleanup_manual_lease() {
    [ -n "$lease_id" ] || return 0
    dc exec -T \
      -e VPNGATE_API_TOKEN="$token" \
      -e LEASE_ID="$lease_id" \
      sub2api sh -ceu \
      'curl -fsS -o /dev/null -X DELETE \
        -H "Authorization: Bearer $VPNGATE_API_TOKEN" \
        "http://vpngate:20000/v1/leases/$LEASE_ID"' \
      || remote_log 'manual smoke lease could not be released automatically'
  }

  write_receipt() {
    local status=$1
    {
      printf 'status=%s\n' "$status"
      printf 'commit=%s\n' "$commit"
      printf 'project=%s\n' "$project"
      printf 'release=%s\n' "$release"
      printf 'backup=%s\n' "$backup"
      printf 'maintenance_started=%s\n' "$maintenance_started"
      [ -f "$backup/old-sub2api-image-id" ] && printf 'old_image=%s\n' "$(cat "$backup/old-sub2api-image-id")"
      [ -f "$backup/new-sub2api-image-id" ] && printf 'new_image=%s\n' "$(cat "$backup/new-sub2api-image-id")"
      [ -f "$backup/new-vpngate-image-id" ] && printf 'new_vpngate_image=%s\n' "$(cat "$backup/new-vpngate-image-id")"
    } > "$backup/receipt.txt"
    chmod 0600 "$backup/receipt.txt"
  }

  on_exit() {
    local status=$?
    trap - EXIT
    cleanup_manual_lease || true
    if [ "$status" -ne 0 ] && [ "$maintenance_started" -eq 1 ]; then
      rollback || true
    fi
    write_receipt "$status" || true
    rm -rf "$remote_tmp"
    if [ "$status" -eq 0 ]; then
      remote_log "upgrade succeeded; release=$release backup=$backup"
    else
      remote_log "upgrade failed; release=$release backup=$backup"
    fi
    exit "$status"
  }
  trap on_exit EXIT

  remote_log 'building application and sidecar before stopping production'
  dc build --pull --progress=plain sub2api vpngate
  docker image inspect "sub2api:source-$commit" --format '{{.Id}}' \
    > "$backup/new-sub2api-image-id"
  docker image inspect "sub2api-vpngate:source-$commit" --format '{{.Id}}' \
    > "$backup/new-vpngate-image-id"

  maintenance_started=1
  remote_log 'entering maintenance window'
  dc stop sub2api vpngate
  dc up -d --no-build sub2api vpngate

  container_id=$(dc ps -q sub2api)
  [ -n "$container_id" ] || remote_die 'new sub2api container was not created'
  deadline=$((SECONDS + 180))
  health_status=starting
  while [ "$health_status" != healthy ]; do
    [ "$SECONDS" -lt "$deadline" ] || {
      dc logs --tail=100 sub2api >&2 || true
      remote_die 'sub2api healthcheck timed out'
    }
    health_status=$(docker inspect "$container_id" \
      --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' \
      2>/dev/null || printf 'missing')
    case "$health_status" in
      healthy) break ;;
      starting|missing) sleep 5 ;;
      *) dc logs --tail=100 sub2api >&2 || true; remote_die "sub2api healthcheck status: $health_status" ;;
    esac
  done

  dc exec -T postgres sh -ceu '
    result=$(psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "
      SELECT column_name
      FROM information_schema.columns
      WHERE table_schema = '\''public'\''
        AND table_name = '\''proxies'\''
        AND column_name IN ('\''managed_by'\'', '\''external_ref'\'')
      ORDER BY column_name;
    ")
    printf "%s\n" "$result" | grep -Fx external_ref >/dev/null
    printf "%s\n" "$result" | grep -Fx managed_by >/dev/null
  '

  if ! dc exec -T sub2api sh -ceu \
    "curl -fsS --max-time 10 http://127.0.0.1:$server_port/health >/dev/null"; then
    remote_die 'application HTTP health endpoint failed'
  fi

  token=$(awk -F= '$1 == "VPNGATE_API_TOKEN" { print substr($0, index($0, "=") + 1) }' "$env_file")
  client_ref="upgrade-smoke-$(date -u +%Y%m%dT%H%M%SZ)-$$"

  health_json=$(dc exec -T -e VPNGATE_API_TOKEN="$token" sub2api sh -ceu \
    'curl -fsS -H "Authorization: Bearer $VPNGATE_API_TOKEN" http://vpngate:20000/healthz')
  printf '%s\n' "$health_json" \
    | jq -e '.status == "ok" and .nodes >= 1 and .slots >= 1' >/dev/null \
    || remote_die 'sidecar health did not report a usable node and slot'

  unauthorized_code=$(dc exec -T sub2api sh -ceu \
    'curl -sS -o /dev/null -w "%{http_code}" http://vpngate:20000/healthz')
  [ "$unauthorized_code" = 401 ] \
    || remote_die "sidecar unauthorized status was $unauthorized_code"

  lease_json=$(dc exec -T \
    -e VPNGATE_API_TOKEN="$token" \
    -e CLIENT_REF="$client_ref" \
    sub2api sh -ceu \
    'curl -fsS -X POST \
      -H "Authorization: Bearer $VPNGATE_API_TOKEN" \
      -H "Content-Type: application/json" \
      --data "{\"client_ref\":\"$CLIENT_REF\",\"protocol\":\"http\"}" \
      http://vpngate:20000/v1/leases')
  lease_id=$(printf '%s' "$lease_json" | jq -er '.lease_id')
  node_before=$(printf '%s' "$lease_json" | jq -er '.node')

  lease_again=$(dc exec -T \
    -e VPNGATE_API_TOKEN="$token" \
    -e CLIENT_REF="$client_ref" \
    sub2api sh -ceu \
    'curl -fsS -X POST \
      -H "Authorization: Bearer $VPNGATE_API_TOKEN" \
      -H "Content-Type: application/json" \
      --data "{\"client_ref\":\"$CLIENT_REF\",\"protocol\":\"http\"}" \
      http://vpngate:20000/v1/leases')
  [ "$lease_id" = "$(printf '%s' "$lease_again" | jq -er '.lease_id')" ] \
    || remote_die 'sidecar lease was not idempotent for the same client_ref'

  list_json=$(dc exec -T -e VPNGATE_API_TOKEN="$token" sub2api sh -ceu \
    'curl -fsS -H "Authorization: Bearer $VPNGATE_API_TOKEN" http://vpngate:20000/v1/leases')
  printf '%s' "$list_json" \
    | jq -e 'all(.leases[]?; has("password") | not)' >/dev/null \
    || remote_die 'sidecar lease list exposed a password'

  node_count=$(printf '%s' "$health_json" | jq -er '.nodes')
  if [ "$node_count" -ge 2 ]; then
    rotate_json=
    if rotate_json=$(dc exec -T \
      -e VPNGATE_API_TOKEN="$token" \
      -e LEASE_ID="$lease_id" \
      sub2api sh -ceu \
      'curl -fsS -X POST \
        -H "Authorization: Bearer $VPNGATE_API_TOKEN" \
        "http://vpngate:20000/v1/leases/$LEASE_ID/rotate"'); then
      :
    fi
    if [ -n "$rotate_json" ]; then
      node_after=$(printf '%s' "$rotate_json" | jq -er '.node')
      [ "$node_before" != "$node_after" ] \
        || remote_die 'sidecar rotate returned the original node'
    else
      remote_log 'rotate smoke returned no healthy candidate; continuing with lease checks'
    fi
  else
    remote_log 'rotate smoke skipped because sidecar reported fewer than two nodes'
  fi

  cleanup_manual_lease
  lease_id=
}

# cleanup_local runs from an EXIT trap after run_local has returned, so the
# variables it reads must live at script scope, not as run_local locals.
host=""
dry_run=0
tmp_dir=""
remote_tmp=""
ssh_args=()

run_local() {
  local script_path=$1
  shift
  local install_dir=""
  local commit="$EXPECTED_COMMIT"
  local pool_file=""
  local ssh_port=22
  local identity_file=""
  local project_name=""
  local server_port=8080
  local archive=""
  local archive_sha=""
  local pool_sha=""
  local repo_root=""
  local -a scp_args

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --host) [ "$#" -ge 2 ] || die '--host requires a value'; host=$2; shift 2 ;;
      --install-dir) [ "$#" -ge 2 ] || die '--install-dir requires a value'; install_dir=$2; shift 2 ;;
      --commit) [ "$#" -ge 2 ] || die '--commit requires a value'; commit=$2; shift 2 ;;
      --pool-file) [ "$#" -ge 2 ] || die '--pool-file requires a value'; pool_file=$2; shift 2 ;;
      --project-name) [ "$#" -ge 2 ] || die '--project-name requires a value'; project_name=$2; shift 2 ;;
      --ssh-port) [ "$#" -ge 2 ] || die '--ssh-port requires a value'; ssh_port=$2; shift 2 ;;
      --identity) [ "$#" -ge 2 ] || die '--identity requires a value'; identity_file=$2; shift 2 ;;
      --server-port) [ "$#" -ge 2 ] || die '--server-port requires a value'; server_port=$2; shift 2 ;;
      --dry-run) dry_run=1; shift ;;
      -h|--help) usage; return 0 ;;
      *) die "unknown option: $1" ;;
    esac
  done

  [ -n "$host" ] || die '--host is required'
  [ -n "$install_dir" ] || die '--install-dir is required'
  [ -n "$pool_file" ] || die '--pool-file is required'
  [ "${install_dir#/}" != "$install_dir" ] || die 'install directory must be absolute'
  case "$install_dir" in
    *[!A-Za-z0-9_./-]*) die 'install directory contains unsafe characters' ;;
  esac
  case "$project_name" in
    *[!A-Za-z0-9_-]*) die 'project name contains unsafe characters' ;;
  esac
  case "$server_port" in
    ''|*[!0-9]*) die 'server port must be numeric' ;;
  esac
  [ "$server_port" -ge 1 ] && [ "$server_port" -le 65535 ] \
    || die 'server port must be between 1 and 65535'
  [ -f "$pool_file" ] || die "pool file does not exist: $pool_file"
  [ -s "$pool_file" ] || die 'pool file is empty'
  [ "$commit" = "$EXPECTED_COMMIT" ] \
    || die "only verified commit $EXPECTED_COMMIT is allowed"

  command -v git >/dev/null 2>&1 || die 'git is required'
  command -v ssh >/dev/null 2>&1 || die 'ssh is required'
  command -v scp >/dev/null 2>&1 || die 'scp is required'
  command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required'

  repo_root=$(CDPATH= cd -- "$(dirname -- "$script_path")/.." && pwd)
  cd "$repo_root"
  [ -z "$(git status --porcelain)" ] || die 'local worktree is not clean'
  [ "$(git rev-parse "${commit}^{tree}")" = "$EXPECTED_TREE" ] \
    || die 'commit tree is not the verified tree'

  tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-upgrade.XXXXXX")
  remote_tmp="/tmp/sub2api-upgrade-${commit}-$$"
  cleanup_local() {
    rm -rf "$tmp_dir"
    if [ -n "$host" ] && [ -n "$remote_tmp" ] && [ "$dry_run" -eq 0 ]; then
      ssh "${ssh_args[@]}" "$host" "rm -rf -- '$remote_tmp'" >/dev/null 2>&1 || true
    fi
  }
  trap cleanup_local EXIT HUP INT TERM

  archive="$tmp_dir/sub2api-$commit.tar.gz"
  git archive --format=tar.gz --prefix="sub2api-$commit/" "$commit" > "$archive"
  archive_sha=$(sha256sum "$archive" | awk '{print $1}')
  pool_sha=$(sha256sum "$pool_file" | awk '{print $1}')
  log "commit=$commit tree=$EXPECTED_TREE archive_sha=$archive_sha pool_sha=$pool_sha"

  ssh_args=(-o BatchMode=yes -o ConnectTimeout=15 -p "$ssh_port")
  [ -n "$identity_file" ] && ssh_args+=(-i "$identity_file")
  scp_args=(-P "$ssh_port")
  [ -n "$identity_file" ] && scp_args+=(-i "$identity_file")

  if [ "$dry_run" -eq 1 ]; then
    log 'dry-run: local validation passed; no SSH, SCP, Docker, or remote mutation performed'
    return 0
  fi

  ssh "${ssh_args[@]}" "$host" true || die 'SSH preflight failed'
  ssh "${ssh_args[@]}" "$host" "umask 077; mkdir -p '$remote_tmp'"
  scp "${scp_args[@]}" "$archive" "$host:$remote_tmp/source.tar.gz"
  scp "${scp_args[@]}" "$pool_file" "$host:$remote_tmp/mihomo-openvpn.yaml"
  ssh "${ssh_args[@]}" "$host" \
    "test \"\$(sha256sum '$remote_tmp/source.tar.gz' | awk '{print \\\$1}')\" = '$archive_sha' && test \"\$(sha256sum '$remote_tmp/mihomo-openvpn.yaml' | awk '{print \\\$1}')\" = '$pool_sha'" \
    || die 'remote upload checksum verification failed'

  log 'starting remote preflight, backup, build, migration, and smoke checks'
  ssh "${ssh_args[@]}" "$host" bash -s -- \
    "$install_dir" "$commit" "$archive_sha" "$pool_sha" "$remote_tmp" "$project_name" "$server_port" \
    < "$script_path"
}

if [ "${1:-}" = '--remote' ]; then
  shift
  [ "$#" -eq 7 ] || remote_die 'remote mode requires seven positional arguments'
  remote_main "$@"
  exit 0
fi

run_local "$0" "$@"
