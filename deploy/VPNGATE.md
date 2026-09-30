# VPN Gate sidecar

Optional sidecar that lets an upstream API account egress through a VPN Gate
OpenVPN node. Sub2API itself is unchanged: each **slot** of the sidecar is an
ordinary `http` proxy, and you point an account at a slot the same way you
point it at any other proxy.

## How it behaves

- One Mihomo process, one authenticated HTTP listener per slot
  (`slot01` on port 20001, `slot02` on 20002, …).
- Each slot starts on a random node and **keeps it** while it passes health
  checks. After `VPNGATE_FAIL_THRESHOLD` consecutive failed probes it moves to
  another random node that is healthy, not cooling down after a failure, and not
  used by any other slot. A node that recovers later does **not** pull the slot
  back.
- There is no DIRECT fallback anywhere. If a slot's node is dead and no healthy
  free node exists, requests through that slot fail; they never leave from the
  host IP.
- The selected node per slot survives restarts (`/data/state/cache.db`).
- Target hostnames are resolved inside the tunnel (`VPNGATE_DNS`), not by the
  host resolver.
- No TUN device, no `NET_ADMIN`, no root: the compose service runs with
  `cap_drop: ALL` and a read-only root filesystem.

## 1. Node pool

The sidecar does not download VPN Gate itself. It reads a Mihomo YAML file of
`type: openvpn` proxies, the format produced by any-auto-register's exporter:

```bash
# in the any-auto-register checkout
python tools/vpngate_openvpn_export.py   # writes mihomo-openvpn.yaml
```

Copy the result to `deploy/vpngate/pool/mihomo-openvpn.yaml` (next to this
file). Entries that are not `type: openvpn`, lack `ca`, carry unknown fields
(for example `dialer-proxy`) or repeat a name are skipped and logged; the rest
are used. To refresh the pool, replace the file and restart the sidecar.
Restarting drops in-flight connections through the slots.

## 2. Start

```bash
cd deploy
# 16..100 characters, none of : @ / ? # % or space
echo "VPNGATE_PROXY_PASSWORD=$(openssl rand -hex 24)" >> .env
docker compose -f docker-compose.yml -f docker-compose.vpngate.yml up -d --build vpngate
docker compose -f docker-compose.yml -f docker-compose.vpngate.yml logs -f vpngate
```

The log line `vpngate: N nodes, M slots on 0.0.0.0 ports 20001-…` means it is
up. Lines `slotNN: switched "A" -> "B"` record every node change.

## 3. Register slots in Sub2API

In **Proxies → Add**, create one proxy per slot:

| Field | Value |
|---|---|
| Name | `VPN Gate slot01` |
| Protocol | `http` |
| Host | `vpngate` (the compose service name) |
| Port | `20001` |
| Username | `slot01` |
| Password | the value of `VPNGATE_PROXY_PASSWORD` |

Then pick that proxy on the account. Give each account its own slot: two
accounts on one slot share one exit IP.

The exit IP is **not** necessarily the node's server address (VPN Gate
`public-vpn-*` servers NAT to a neighbouring address). Use the proxy test in
Sub2API to see the actual exit IP and country.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `VPNGATE_PROXY_PASSWORD` | (required) | password of every slot listener |
| `VPNGATE_SLOTS` | `10` | number of slots (1..200) |
| `VPNGATE_BASE_PORT` | `20001` | port of `slot01` |
| `VPNGATE_PROBE_URL` | `https://www.gstatic.com/generate_204` | fetched through a node to check it |
| `VPNGATE_PROBE_TIMEOUT` | `10s` | per probe |
| `VPNGATE_PROBE_INTERVAL` | `60s` | how often every slot's node is probed |
| `VPNGATE_FAIL_THRESHOLD` | `3` | consecutive failures before a slot moves |
| `VPNGATE_COOLDOWN` | `30m` | a failed node is not picked again for this long |
| `VPNGATE_MAX_ATTEMPTS` | `5` | candidates probed per move |
| `VPNGATE_MAX_NODES` | `200` | nodes read from the pool file |
| `VPNGATE_DNS` | `1.1.1.1,8.8.8.8` | nameservers used inside each tunnel |
| `VPNGATE_CONTROLLER_ADDR` | `127.0.0.1:19090` | Mihomo controller, loopback only |

## Caveats

- VPN Gate nodes are volunteer-run. Their exit IPs are often already flagged by
  upstream providers, and latency is high (1.5–5 s per probe in testing).
- Long streaming responses through a node have not been load-tested.
- Networks that interfere with OpenVPN (connection resets during the handshake)
  make every probe fail; the sidecar then logs failures and never falls back to
  direct.
