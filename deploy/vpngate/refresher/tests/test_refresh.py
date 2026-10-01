from __future__ import annotations

import datetime as dt
import json
from pathlib import Path
from zoneinfo import ZoneInfo

import pytest

import refresh

SHANGHAI = ZoneInfo("Asia/Shanghai")
CA = "-----BEGIN CERTIFICATE-----\\nAAAA\\n-----END CERTIFICATE-----"


def pool_yaml(count: int) -> str:
    lines = ["proxies:"]
    for i in range(count):
        lines += [
            f"  - name: VPNGate-JP-203.0.113.{i}-tcp",
            "    type: openvpn",
            f"    server: 203.0.113.{i}",
            "    port: 443",
            f'    ca: "{CA}"',
        ]
    return "\n".join(lines) + "\n"


def make_settings(pool_dir: Path, **overrides) -> refresh.Settings:
    values = dict(
        pool_dir=pool_dir,
        at=dt.time(4, 0),
        tz=SHANGHAI,
        min_nodes=3,
        retries=2,
        timeout=60,
        source=refresh.DEFAULT_SOURCE,
    )
    values.update(overrides)
    return refresh.Settings(**values)


def exporter_writing(text: str | None, code: int = 0):
    calls: list[Path] = []

    def export(out: Path) -> int:
        calls.append(out)
        if text is not None:
            (out / refresh.POOL_NAME).write_text(text, encoding="utf-8")
        return code

    export.calls = calls
    return export


def fixed_now() -> dt.datetime:
    return dt.datetime(2026, 9, 30, 20, 0, tzinfo=dt.timezone.utc)


def leftovers(pool_dir: Path) -> list[str]:
    return sorted(p.name for p in pool_dir.iterdir() if p.name.startswith("."))


def test_load_settings_defaults():
    s = refresh.load_settings({})
    assert s.pool_dir == Path("/data/pool")
    assert s.at == dt.time(4, 0)
    assert s.tz.key == "Asia/Shanghai"
    assert (s.min_nodes, s.retries, s.timeout) == (20, 3, 60)
    assert s.source == "https://www.vpngate.net/api/iphone/"


@pytest.mark.parametrize(
    ("env", "message"),
    [
        ({"VPNGATE_REFRESH_TIME": "4:00"}, "HH:MM"),
        ({"VPNGATE_REFRESH_TIME": "24:00"}, "HH:MM"),
        ({"VPNGATE_REFRESH_TIMEZONE": "Mars/Olympus"}, "time zone"),
        ({"VPNGATE_REFRESH_MIN_NODES": "0"}, "at least 1"),
        ({"VPNGATE_REFRESH_RETRIES": "-1"}, "at least 0"),
        ({"VPNGATE_REFRESH_RETRIES": "two"}, "integer"),
        ({"VPNGATE_REFRESH_TIMEOUT": "0s"}, "positive"),
        ({"VPNGATE_REFRESH_SOURCE": "ftp://example.test/"}, "http"),
    ],
)
def test_load_settings_rejects_bad_values(env, message):
    with pytest.raises(ValueError, match=message):
        refresh.load_settings(env)


@pytest.mark.parametrize(
    ("now", "expected"),
    [
        ("2026-10-01T03:00:00+08:00", "2026-10-01T04:00:00+08:00"),
        ("2026-10-01T05:00:00+08:00", "2026-10-02T04:00:00+08:00"),
        ("2026-10-01T04:00:00+08:00", "2026-10-02T04:00:00+08:00"),
        ("2026-09-30T19:30:00+00:00", "2026-10-01T04:00:00+08:00"),
    ],
)
def test_next_run(now, expected):
    got = refresh.next_run(dt.datetime.fromisoformat(now), dt.time(4, 0), SHANGHAI)
    assert got == dt.datetime.fromisoformat(expected)


def test_publishes_a_pool_with_enough_nodes(tmp_path: Path):
    (tmp_path / refresh.POOL_NAME).write_text("old", encoding="utf-8")
    status = refresh.refresh_once(make_settings(tmp_path), exporter_writing(pool_yaml(3)), fixed_now)
    assert status["result"] == "published" and status["nodes"] == 3
    assert (tmp_path / refresh.POOL_NAME).read_text(encoding="utf-8") == pool_yaml(3)
    assert leftovers(tmp_path) == []


@pytest.mark.parametrize(
    ("export", "reason"),
    [
        (exporter_writing(pool_yaml(2)), "only 2 nodes"),
        (exporter_writing(pool_yaml(5), code=1), "exited with 1"),
        (exporter_writing(None), "no pool file"),
        (exporter_writing("proxies: [\n"), "unreadable"),
    ],
)
def test_keeps_the_old_pool_when_a_refresh_fails(tmp_path: Path, export, reason):
    (tmp_path / refresh.POOL_NAME).write_text("old", encoding="utf-8")
    status = refresh.refresh_once(make_settings(tmp_path), export, fixed_now)
    assert status["result"] == "failed"
    assert reason in status["reason"]
    assert (tmp_path / refresh.POOL_NAME).read_text(encoding="utf-8") == "old"
    assert leftovers(tmp_path) == []


def test_keeps_the_old_pool_when_the_exporter_raises(tmp_path: Path):
    (tmp_path / refresh.POOL_NAME).write_text("old", encoding="utf-8")

    def export(out: Path) -> int:
        raise RuntimeError("network unreachable")

    status = refresh.refresh_once(make_settings(tmp_path), export, fixed_now)
    assert status["result"] == "failed" and "RuntimeError" in status["reason"]
    assert (tmp_path / refresh.POOL_NAME).read_text(encoding="utf-8") == "old"


def test_retries_up_to_the_limit_and_records_status(tmp_path: Path):
    sleeps: list[float] = []
    export = exporter_writing(None)
    status = refresh.run_with_retries(make_settings(tmp_path, retries=2), export, fixed_now, sleeps.append)
    assert len(export.calls) == 3
    assert sleeps == [3600.0, 3600.0]
    assert status["result"] == "failed" and status["attempt"] == 3
    saved = json.loads((tmp_path / refresh.STATUS_NAME).read_text(encoding="utf-8"))
    assert saved["result"] == "failed" and saved["attempt"] == 3
    assert leftovers(tmp_path) == []


def test_stops_retrying_after_a_publish(tmp_path: Path):
    sleeps: list[float] = []
    export = exporter_writing(pool_yaml(4))
    status = refresh.run_with_retries(make_settings(tmp_path), export, fixed_now, sleeps.append)
    assert status["result"] == "published" and len(export.calls) == 1 and sleeps == []
