# VPN Gate sidecar

Optional sidecar that backs Sub2API's **proxy pool mode**: an upstream API
account set to "proxy pool" gets its own proxy endpoint, leased from this
sidecar, that egresses through a VPN Gate OpenVPN node.

## How it behaves

- One Mihomo process with a fixed number of **slots**. Each slot is one
  listener (`slot01` on port 20001, `slot02` on 20002, …) that speaks both
  HTTP and SOCKS5 and has its own password.
- Sub2API **leases** a slot through the lease API. The lease's endpoint
  (host, port, username, password) never changes while the lease exists.
- A new lease starts on a random healthy node that no other lease is using,
  and never on the node the slot's previous lease had.
- Each leased slot keeps its node while it passes health checks. After
  `VPNGATE_FAIL_THRESHOLD` consecutive failed probes it moves to another
  random node that is healthy, not cooling down after a failure, and not used
  by any other lease. A node that recovers later does **not** pull the slot
  back. "Rotate" (the *change proxy* button in Sub2API) does the same move on
  demand; the old node cools down so it is not handed to the next lease.
- The node pool refreshes itself once a day (see below). Applying a new pool
  restarts Mihomo: connections in flight through the slots drop, leases are
  kept, and every slot goes back to its node. A leased slot whose node left
  the VPN Gate list keeps that node until its health checks fail; no other
  lease is ever given it.
- There is no DIRECT fallback anywhere. If a leased slot's node is dead and no
  healthy free node exists, requests through it fail; they never leave from
  the host IP.
- Leases and each slot's node survive restarts (`/data/state/leases.json`,
  `/data/state/cache.db`).
- Target hostnames are resolved inside the tunnel (`VPNGATE_DNS`), not by the
  host resolver.
- No TUN device, no `NET_ADMIN`, no root: both services run with
  `cap_drop: ALL` and a read-only root filesystem.

## 1. Node pool

The pool is `mihomo-openvpn.yaml` in the `vpngate_pool` volume. The
`vpngate-refresh` service maintains it:

- On its first start, if the volume holds no pool, it fetches one right away
  and keeps retrying hourly until it has one. Until then the sidecar logs
  `waiting for the pool file` and does not start.
- Every day at `VPNGATE_REFRESH_TIME` (`VPNGATE_REFRESH_TIMEZONE`) it fetches
  `https://www.vpngate.net/api/iphone/`, converts it with the exporter vendored
  from any-auto-register, and replaces the pool only when the result has at
  least `VPNGATE_REFRESH_MIN_NODES` OpenVPN nodes. Otherwise the pool in use
  stays, and it retries hourly up to `VPNGATE_REFRESH_RETRIES` times that day.
- Each attempt is recorded in `refresh-status.json` next to the pool:

      docker compose -f docker-compose.yml -f docker-compose.vpngate.yml \
        exec vpngate-refresh cat /data/pool/refresh-status.json

The sidecar checks the file every `VPNGATE_POOL_POLL_INTERVAL` and applies a
changed one. Entries that are not `type: openvpn`, lack `ca`, carry unknown
fields (for example `dialer-proxy`) or repeat a name are skipped and logged; a
file with no usable entry is rejected and the running pool stays.

To put a pool in place by hand (for example on a host that cannot reach
www.vpngate.net), copy it into the volume through the refresher container:

    docker compose -f docker-compose.yml -f docker-compose.vpngate.yml \
      cp ./mihomo-openvpn.yaml vpngate-refresh:/data/pool/mihomo-openvpn.yaml

The next scheduled refresh replaces it again if www.vpngate.net is reachable.

## 2. Start

```bash
cd deploy
echo "VPNGATE_MASTER_SECRET=$(openssl rand -hex 24)" >> .env
echo "VPNGATE_API_TOKEN=$(openssl rand -hex 24)" >> .env
docker compose -f docker-compose.yml -f docker-compose.vpngate.yml up -d --build vpngate vpngate-refresh
docker compose -f docker-compose.yml -f docker-compose.vpngate.yml logs -f vpngate vpngate-refresh
```

`VPNGATE_MASTER_SECRET` derives every slot's password. **Changing it
invalidates the password of every existing lease** (the proxies Sub2API
stored stop authenticating); keep it stable.

The log line `vpngate: N nodes, M slots on 0.0.0.0 ports 20001-…, K leased;
lease API on 0.0.0.0:20000` means it is up. Lines `slotNN: switched "A" -> "B"`
record every node change, and `pool … applied: N nodes (+a -r), k kept for
leased slots` every pool change.

## 3. Connect Sub2API

In Sub2API, open **Proxies → Proxy pool settings** and set

| Field | Value |
|---|---|
| Provider URL | `http://vpngate:20000` |
| Token | the value of `VPNGATE_API_TOKEN` |

(or start Sub2API with `PROXY_POOL_URL` / `PROXY_POOL_TOKEN`; values saved in
the UI take precedence). Then, in an account's proxy selector, choose
**Proxy pool (auto)**. Sub2API leases a slot and stores it as a proxy marked
as pool-managed; each account gets its own. Pool-managed proxies that no
account uses any more are released automatically.

## Lease API (v1)

Every request needs `Authorization: Bearer <VPNGATE_API_TOKEN>`.

| Request | Result |
|---|---|
| `POST /v1/leases` `{"client_ref":"…","protocol":"http"\|"socks5"}` | `200` lease (with password). The same `client_ref` always gets the same lease back. `503 {"error":"pool_exhausted"}` when every slot is leased, `503 {"error":"no_healthy_node"}` when no node is usable |
| `POST /v1/leases/{id}/rotate` | `200` lease on a new node; `503 no_healthy_node` keeps the old node; `404` unknown lease |
| `DELETE /v1/leases/{id}` | `204`; `404` unknown lease |
| `GET /v1/leases` | `{"leases":[…]}` without passwords |
| `GET /healthz` | `{"status":"ok","nodes":N,"slots":M,"leased":K,"candidates":N,"retained":R,"pool_sha256":"…","pool_loaded_at":"…"}` |

A lease: `{"lease_id","client_ref","protocol","host","port","username","password","node","created_at"}`.
`host` is `VPNGATE_PUBLIC_HOST`. In `/healthz`, `candidates` (also `nodes`) are
the nodes of the current pool, `retained` the nodes kept for leased slots
after they left the list, and `pool_sha256` the first 12 hex digits of the
pool file's sha256.

## Settings

Sidecar (`vpngate`):

| Variable | Default | Meaning |
|---|---|---|
| `VPNGATE_MASTER_SECRET` | (required) | derives each slot's password; at least 16 characters |
| `VPNGATE_API_TOKEN` | (required) | bearer token of the lease API; at least 16 characters |
| `VPNGATE_API_LISTEN` | `0.0.0.0:20000` | lease API address |
| `VPNGATE_PUBLIC_HOST` | `vpngate` | host name put into leases (the compose service name) |
| `VPNGATE_SLOTS` | `10` | number of slots = maximum number of leases (1..200) |
| `VPNGATE_BASE_PORT` | `20001` | port of `slot01` |
| `VPNGATE_PROBE_URL` | `https://www.gstatic.com/generate_204` | fetched through a node to check it |
| `VPNGATE_PROBE_TIMEOUT` | `10s` | per probe |
| `VPNGATE_PROBE_INTERVAL` | `60s` | how often every leased slot's node is probed |
| `VPNGATE_FAIL_THRESHOLD` | `3` | consecutive failures before a slot moves |
| `VPNGATE_COOLDOWN` | `30m` | a failed or rotated-away node is not picked again for this long |
| `VPNGATE_MAX_ATTEMPTS` | `5` | candidates probed per move |
| `VPNGATE_MAX_NODES` | `200` | nodes read from the pool file |
| `VPNGATE_DNS` | `1.1.1.1,8.8.8.8` | nameservers used inside each tunnel |
| `VPNGATE_CONTROLLER_ADDR` | `127.0.0.1:19090` | Mihomo controller, loopback only |
| `VPNGATE_POOL_POLL_INTERVAL` | `60s` | how often the pool file is checked for changes |

Refresher (`vpngate-refresh`):

| Variable | Default | Meaning |
|---|---|---|
| `VPNGATE_REFRESH_TIME` | `04:00` | daily refresh time, `HH:MM` |
| `VPNGATE_REFRESH_TIMEZONE` | `Asia/Shanghai` | IANA time zone; an unknown one stops the service |
| `VPNGATE_REFRESH_MIN_NODES` | `20` | OpenVPN nodes a result needs before it replaces the pool |
| `VPNGATE_REFRESH_RETRIES` | `3` | hourly retries after a failed refresh, per day |
| `VPNGATE_REFRESH_TIMEOUT` | `60s` | download timeout |
| `VPNGATE_REFRESH_SOURCE` | `https://www.vpngate.net/api/iphone/` | feed URL |

## Caveats

- VPN Gate nodes are volunteer-run. Their exit IPs are often already flagged by
  upstream providers, and latency is high (1.5–5 s per probe in testing).
  Different nodes can share an exit /24 (`public-vpn-*` servers NAT to
  neighbouring addresses), so separate leases do not guarantee separate
  networks.
- The feed's size varies (97–100 servers in testing) and many listed servers
  are dead; a new pool is not health-checked before it is applied, so a lease
  request or rotate can take up to about a minute while candidates are probed.
- Hosts that cannot reach www.vpngate.net never refresh; `refresh-status.json`
  shows the failures and the pool in use stays.
- Long streaming responses through a node have not been load-tested.
- Networks that interfere with OpenVPN (connection resets during the handshake)
  make every probe fail; the sidecar then logs failures and never falls back to
  direct.
