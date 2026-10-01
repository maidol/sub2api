#!/usr/bin/env python3
"""Refresh the vpngate sidecar's VPN Gate node pool once a day.

The sidecar polls the pool file and applies a changed one itself; this
process only produces a trustworthy file. A refresh exports into a staging
directory next to the pool file, checks the result, and atomically replaces
the pool only when it holds enough nodes. A failed refresh never touches the
pool in use.
"""

from __future__ import annotations

import contextlib
import datetime as dt
import json
import os
import re
import shutil
import signal
import sys
import tempfile
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import yaml

THIRD_PARTY_DIR = Path(__file__).resolve().parent / "third_party"
if str(THIRD_PARTY_DIR) not in sys.path:
    sys.path.insert(0, str(THIRD_PARTY_DIR))

from tools import vpngate_openvpn_export as exporter  # noqa: E402

POOL_NAME = "mihomo-openvpn.yaml"
STATUS_NAME = "refresh-status.json"
DEFAULT_SOURCE = "https://www.vpngate.net/api/iphone/"
RETRY_INTERVAL = dt.timedelta(hours=1)

Exporter = Callable[[Path], int]
Clock = Callable[[], dt.datetime]


@dataclass(frozen=True)
class Settings:
    pool_dir: Path
    at: dt.time
    tz: ZoneInfo
    min_nodes: int
    retries: int
    timeout: int  # seconds
    source: str


def log(message: str) -> None:
    print(f"vpngate-refresh: {message}", flush=True)


def _int(env: Mapping[str, str], key: str, default: int, minimum: int) -> int:
    raw = env.get(key, "").strip() or str(default)
    try:
        value = int(raw)
    except ValueError:
        raise ValueError(f"{key}={raw!r} must be an integer") from None
    if value < minimum:
        raise ValueError(f"{key}={raw!r} must be at least {minimum}")
    return value


def _seconds(env: Mapping[str, str], key: str, default: str) -> int:
    raw = env.get(key, "").strip() or default
    match = re.fullmatch(r"(\d+)s?", raw)
    if not match or int(match.group(1)) <= 0:
        raise ValueError(f"{key}={raw!r} must be a positive number of seconds like 60s")
    return int(match.group(1))


def load_settings(env: Mapping[str, str]) -> Settings:
    """Read VPNGATE_* settings; an invalid value raises ValueError."""
    raw_time = env.get("VPNGATE_REFRESH_TIME", "").strip() or "04:00"
    match = re.fullmatch(r"([01]\d|2[0-3]):([0-5]\d)", raw_time)
    if not match:
        raise ValueError(f"VPNGATE_REFRESH_TIME={raw_time!r} must be HH:MM (24-hour)")
    raw_tz = env.get("VPNGATE_REFRESH_TIMEZONE", "").strip() or "Asia/Shanghai"
    try:
        tz = ZoneInfo(raw_tz)
    except (ZoneInfoNotFoundError, ValueError):
        raise ValueError(f"VPNGATE_REFRESH_TIMEZONE={raw_tz!r} is not a known IANA time zone") from None
    source = env.get("VPNGATE_REFRESH_SOURCE", "").strip() or DEFAULT_SOURCE
    if not source.startswith(("https://", "http://")):
        raise ValueError(f"VPNGATE_REFRESH_SOURCE={source!r} must be an http(s) URL")
    return Settings(
        pool_dir=Path(env.get("VPNGATE_POOL_DIR", "").strip() or "/data/pool"),
        at=dt.time(int(match.group(1)), int(match.group(2))),
        tz=tz,
        min_nodes=_int(env, "VPNGATE_REFRESH_MIN_NODES", 20, 1),
        retries=_int(env, "VPNGATE_REFRESH_RETRIES", 3, 0),
        timeout=_seconds(env, "VPNGATE_REFRESH_TIMEOUT", "60s"),
        source=source,
    )


def next_run(now: dt.datetime, at: dt.time, tz: ZoneInfo) -> dt.datetime:
    """The first moment strictly after now when the wall clock in tz shows at."""
    local = now.astimezone(tz)
    candidate = dt.datetime.combine(local.date(), at, tzinfo=tz)
    if candidate <= local:
        candidate = dt.datetime.combine(local.date() + dt.timedelta(days=1), at, tzinfo=tz)
    return candidate


def count_nodes(path: Path) -> int:
    doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    proxies = doc.get("proxies") if isinstance(doc, dict) else None
    if not isinstance(proxies, list):
        raise ValueError("pool has no proxies list")
    return sum(1 for p in proxies if isinstance(p, dict) and str(p.get("type", "")).lower() == "openvpn")


def _status(started: dt.datetime, result: str, *, nodes: int | None = None, reason: str | None = None) -> dict:
    status: dict = {"attempted_at": started.isoformat(), "result": result}
    if nodes is not None:
        status["nodes"] = nodes
    if reason:
        status["reason"] = reason
    return status


def default_exporter(settings: Settings) -> Exporter:
    def export(output_dir: Path) -> int:
        return exporter.main(
            ["--source", settings.source, "--output-dir", str(output_dir), "--timeout", str(settings.timeout)]
        )

    return export


def refresh_once(settings: Settings, export: Exporter, now: Clock) -> dict:
    """Export into a staging directory and publish only a good pool."""
    started = now()
    staging = Path(tempfile.mkdtemp(prefix=".refresh-", dir=settings.pool_dir))
    try:
        try:
            code = export(staging)
        except Exception as exc:  # the exporter reports its own failures as exit codes
            return _status(started, "failed", reason=f"exporter raised {type(exc).__name__}: {exc}")
        if code != 0:
            return _status(started, "failed", reason=f"exporter exited with {code}")
        produced = staging / POOL_NAME
        if not produced.is_file():
            return _status(started, "failed", reason="exporter produced no pool file")
        try:
            nodes = count_nodes(produced)
        except (OSError, ValueError, yaml.YAMLError) as exc:
            return _status(started, "failed", reason=f"pool file unreadable: {exc}")
        if nodes < settings.min_nodes:
            return _status(
                started, "failed", nodes=nodes, reason=f"only {nodes} nodes, need at least {settings.min_nodes}"
            )
        os.chmod(produced, 0o644)
        os.replace(produced, settings.pool_dir / POOL_NAME)
        return _status(started, "published", nodes=nodes)
    finally:
        shutil.rmtree(staging, ignore_errors=True)


def write_status(pool_dir: Path, status: dict) -> None:
    fd, tmp = tempfile.mkstemp(prefix=".status-", dir=pool_dir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(status, fh, ensure_ascii=False, indent=2)
        os.chmod(tmp, 0o644)
        os.replace(tmp, pool_dir / STATUS_NAME)
    except BaseException:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(tmp)
        raise


def run_with_retries(
    settings: Settings, export: Exporter, now: Clock, sleep: Callable[[float], None]
) -> dict:
    """One attempt plus up to settings.retries retries, an hour apart."""
    status: dict = {}
    for attempt in range(1, settings.retries + 2):
        status = refresh_once(settings, export, now)
        status["attempt"] = attempt
        write_status(settings.pool_dir, status)
        if status["result"] == "published":
            log(f"published {status['nodes']} nodes (attempt {attempt})")
            return status
        log(f"attempt {attempt} failed: {status['reason']}")
        if attempt <= settings.retries:
            sleep(RETRY_INTERVAL.total_seconds())
    return status


def main(env: Mapping[str, str] = os.environ) -> int:
    try:
        settings = load_settings(env)
    except ValueError as exc:
        print(f"vpngate-refresh: {exc}", file=sys.stderr)
        return 2
    if not settings.pool_dir.is_dir():
        print(f"vpngate-refresh: pool directory {settings.pool_dir} does not exist", file=sys.stderr)
        return 2
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    export = default_exporter(settings)

    def now() -> dt.datetime:
        return dt.datetime.now(dt.timezone.utc)

    # The sidecar waits for the first pool, so do not wait for the next
    # scheduled time while there is none.
    while not (settings.pool_dir / POOL_NAME).exists():
        log("no pool file yet; refreshing now")
        if run_with_retries(settings, export, now, time.sleep)["result"] == "published":
            break
        time.sleep(RETRY_INTERVAL.total_seconds())
    while True:
        target = next_run(now(), settings.at, settings.tz)
        log(f"next refresh at {target.isoformat()}")
        time.sleep(max(0.0, (target - now()).total_seconds()))
        run_with_retries(settings, export, now, time.sleep)


if __name__ == "__main__":
    raise SystemExit(main())
