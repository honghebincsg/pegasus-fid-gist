#!/usr/bin/env python3
"""抓取机场 ATIS：美国机场优先 atis.info，其余使用 atis.guru，并发布到 GitHub Gist。

两种模式：
- 单机场：ATIS_CODE=ZGGG（workflow_dispatch 输入）
- 批量：ATIS_CODES=ZGGG,ZGSZ,...（定时批量缓存，额外生成 atis_all.json 汇总）
"""

import datetime as dt
import html as html_lib
import json
import os
import re
import sys
import time
import urllib.request
from pathlib import Path

DEFAULT_CODE = "ZGGG"
API_ROOT = "https://api.github.com"
OUT_DIR = Path("out")
USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/124.0.0.0 Safari/537.36"
)

# 常用机场批量缓存顺序：国内 -> 美国 -> 欧洲
COMMON_AIRPORTS = [
    ("ZGGG", "cn"),
    ("ZGSZ", "cn"),
    ("ZUCK", "cn"),
    ("ZSPD", "cn"),
    ("PANC", "us"),
    ("KJFK", "us"),
    ("KORD", "us"),
    ("KLAX", "us"),
    ("KMIA", "us"),
    ("EHAM", "eu"),
]
REGION_BY_CODE = dict(COMMON_AIRPORTS)


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")


def fetch(url: str, attempts: int = 3, timeout: int = 30) -> str:
    headers = {"User-Agent": USER_AGENT, "Accept": "text/html,application/json"}
    last_error = None
    for attempt in range(1, attempts + 1):
        try:
            req = urllib.request.Request(url, headers=headers)
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.read().decode("utf-8", "replace")
        except Exception as exc:
            last_error = exc
            print(f"[fetch] attempt {attempt}/{attempts} failed: {exc}", flush=True)
            if attempt < attempts:
                time.sleep(2 * attempt)
    raise last_error


def extract_atis_time(text: str) -> str:
    match = re.search(r"\b(\d{4})Z\b", text or "")
    return match.group(0) if match else ""


def fetch_atisinfo(code: str):
    """美国机场：atis.info/api/<ICAO> 返回 combined 或 arrival/departure。"""
    url = f"https://atis.info/api/{code}"
    try:
        payload = json.loads(fetch(url, attempts=1, timeout=20))
    except Exception:
        return None
    if not isinstance(payload, list) or not payload:
        return None
    combined = None
    arrival = {"updated_at": "", "text": ""}
    departure = {"updated_at": "", "text": ""}
    for item in payload:
        if not isinstance(item, dict):
            continue
        text = item.get("datis") or ""
        if not text:
            continue
        block = {
            "code": item.get("code", ""),
            "updated_at": extract_atis_time(text),
            "text": text,
        }
        kind = (item.get("type") or "").lower()
        if kind == "combined":
            combined = block
        elif kind == "arrival":
            arrival = block
        elif kind == "departure":
            departure = block
    if combined is None and not arrival["text"] and not departure["text"]:
        return None
    return {
        "source": "atis.info",
        "source_url": url,
        "combined": combined,
        "arrival": arrival,
        "departure": departure,
    }


def page_text(html: str) -> str:
    html = re.sub(r"<script.*?</script>", " ", html, flags=re.S | re.I)
    html = re.sub(r"<style.*?</style>", " ", html, flags=re.S | re.I)
    text = re.sub(r"<[^>]+>", " ", html)
    text = html_lib.unescape(text).replace("\r", "")
    text = re.sub(r"[ \t]+", " ", text)
    text = re.sub(r" *\n *", "\n", text)
    text = re.sub(r"\n{2,}", "\n", text)
    return text.strip()


def clean(block: str) -> str:
    block = re.sub(r"\s+", " ", block).strip()
    return re.sub(r"An unhandled error.*$", "", block).strip()


def extract(text: str, code: str) -> dict:
    one = re.sub(r"\s+", " ", text)

    def grab(pattern: str) -> str:
        match = re.search(pattern, one, re.S)
        return clean(match.group(1)) if match else ""

    return {
        "arrival": {
            "updated_at": grab(r"Arrival ATIS ([\d-]+ [\d:]+) UTC"),
            "text": grab(rf"{code} ARR ATIS (.*?) Departure ATIS"),
        },
        "departure": {
            "updated_at": grab(r"Departure ATIS ([\d-]+ [\d:]+) UTC"),
            "text": grab(rf"{code} DEP ATIS (.*?) METAR"),
        },
    }


def build_snapshot(code: str):
    """抓取单个机场，返回 (snapshot, status)。失败时 snapshot 带 ok=false。"""
    status = {"ok": False, "checked_at": utc_now(), "airport": code}
    try:
        info = fetch_atisinfo(code)
        if info:
            snapshot = {
                "source_url": info["source_url"],
                "airport": code,
                "source": "atis.info",
                "fetched_at": utc_now(),
                "combined": info["combined"],
                "arrival": info["arrival"],
                "departure": info["departure"],
                "ok": True,
            }
            status.update(
                ok=True,
                source="atis.info",
                combined_code=(info["combined"] or {}).get("code", ""),
                arrival_updated=info["arrival"]["updated_at"],
                departure_updated=info["departure"]["updated_at"],
            )
            return snapshot, status

        url = f"https://atis.guru/atis/{code}"
        page = fetch(url)
        data = extract(page_text(page), code)
        if data["arrival"]["text"] or data["departure"]["text"]:
            snapshot = {
                "source_url": url,
                "airport": code,
                "source": "atis.guru",
                "fetched_at": utc_now(),
                "combined": None,
                "arrival": data["arrival"],
                "departure": data["departure"],
                "ok": True,
            }
            status.update(
                ok=True,
                source="atis.guru",
                arrival_updated=data["arrival"]["updated_at"],
                departure_updated=data["departure"]["updated_at"],
            )
            return snapshot, status

        status.update(ok=False, source="atis.guru", error="该机场暂无 ATIS 数据")
        print(f"::warning::no ATIS data found for {code}")
        return {
            "airport": code,
            "ok": False,
            "error": "该机场暂无 ATIS 数据",
            "checked_at": utc_now(),
            "source": "atis.guru",
        }, status
    except Exception as exc:
        status["error"] = f"{type(exc).__name__}: {exc}"
        print(f"::error::Fetch failed for {code}: {status['error']}")
        return {
            "airport": code,
            "ok": False,
            "error": status["error"],
            "checked_at": utc_now(),
            "source": "",
        }, status


def publish(token: str, gist_id: str, files: dict[str, str | None], description: str) -> dict:
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "Content-Type": "application/json",
    }
    existing: set[str] = set()
    try:
        req = urllib.request.Request(f"{API_ROOT}/gists/{gist_id}", headers=headers)
        with urllib.request.urlopen(req, timeout=20) as resp:
            existing = set(json.load(resp).get("files", {}).keys())
    except Exception:
        pass
    payload = {
        "description": description,
        "files": {
            name: ({"content": content} if content is not None else None)
            for name, content in files.items()
            if content is not None or name in existing
        },
    }
    req = urllib.request.Request(
        f"{API_ROOT}/gists/{gist_id}",
        data=json.dumps(payload).encode(),
        method="PATCH",
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def legacy_files(code: str) -> dict[str, None]:
    return {
        f"atis_{code.lower()}.json": None,
        f"atis_{code.lower()}.txt": None,
        f"atis_{code.lower()}_status.json": None,
    }


def publish_gist(token: str, gist_id: str, files: dict, description: str, lines: list[str]) -> int:
    if not token or not gist_id:
        lines.append("- 未发布到 Gist（缺少 GIST_TOKEN/GIST_ID）")
        return 0
    try:
        gist = publish(token, gist_id, files, description)
        lines.append(f"- Gist：{gist.get('html_url', '')}")
        return 0
    except Exception as exc:
        print(f"::error::Gist publish failed: {exc}")
        lines.append(f"- Gist 发布失败：`{exc}`")
        return 1


def emit_summary(lines: list[str]) -> None:
    summary = "\n".join(lines) + "\n"
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as fh:
            fh.write(summary)
    else:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
        print(summary)


def run_single(token: str, gist_id: str) -> int:
    code = (os.environ.get("ATIS_CODE") or DEFAULT_CODE).strip().upper()
    if not re.fullmatch(r"[A-Z]{4}", code):
        print(f"::error::invalid ICAO code: {code!r}")
        return 2
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    snapshot, status = build_snapshot(code)
    files: dict[str, str | None] = {f"{code}.json": json.dumps(snapshot, ensure_ascii=False, indent=2)}
    files.update(legacy_files(code))
    for name, content in files.items():
        if content is not None:
            (OUT_DIR / name).write_text(content, encoding="utf-8", newline="\n")

    lines = [
        "## ATIS 抓取结果",
        "",
        f"- 机场：{code}",
        f"- 状态：{'成功' if status['ok'] else '失败'}",
        f"- 时间（UTC）：{status['checked_at']}",
        f"- 数据源：{status.get('source', '—')}",
    ]
    if status["ok"]:
        if status.get("combined_code"):
            lines.append(f"- Combined ATIS 代号：{status['combined_code']}")
        if status.get("arrival_updated"):
            lines.append(f"- 到达 ATIS 更新：{status['arrival_updated']}")
        if status.get("departure_updated"):
            lines.append(f"- 起飞 ATIS 更新：{status['departure_updated']}")
    else:
        lines.append(f"- 错误：`{status.get('error', '')}`")

    exit_code = 0 if status["ok"] else 1
    exit_code |= publish_gist(
        token, gist_id, files, f"ATIS {code}, last success {status['checked_at']}", lines
    )
    emit_summary(lines)
    return exit_code


def run_batch(raw_codes: str, token: str, gist_id: str) -> int:
    codes: list[tuple[str, str]] = []
    for part in raw_codes.split(","):
        code = part.strip().upper()
        if not code:
            continue
        if not re.fullmatch(r"[A-Z]{4}", code):
            print(f"::error::invalid ICAO code: {code!r}")
            return 2
        codes.append((code, REGION_BY_CODE.get(code, "other")))
    if not codes:
        print("::error::ATIS_CODES is empty")
        return 2

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    files: dict[str, str | None] = {}
    airports: list[dict] = []
    ok_count = 0
    started = time.time()

    for code, region in codes:
        snapshot, status = build_snapshot(code)
        if status["ok"]:
            ok_count += 1
        item = dict(snapshot)
        item["region"] = region
        airports.append(item)
        files[f"{code}.json"] = json.dumps(snapshot, ensure_ascii=False, indent=2)
        print(f"[batch] {code}: {'ok' if status['ok'] else 'fail'}"
              f" ({status.get('source', '—')})", flush=True)
        time.sleep(0.5)

    aggregate = {
        "ok": ok_count > 0,
        "fetched_at": utc_now(),
        "count": len(airports),
        "ok_count": ok_count,
        "airports": airports,
    }
    files["atis_all.json"] = json.dumps(aggregate, ensure_ascii=False, indent=2)
    for code, _ in codes:
        files.update(legacy_files(code))
    for name, content in files.items():
        if content is not None:
            (OUT_DIR / name).write_text(content, encoding="utf-8", newline="\n")

    lines = [
        "## ATIS 批量抓取结果",
        "",
        f"- 机场数：{len(airports)}，成功：{ok_count}",
        f"- 时间（UTC）：{aggregate['fetched_at']}",
        f"- 耗时：{time.time() - started:.1f} 秒",
        "",
        "| 机场 | 区域 | 状态 | 数据源 |",
        "| --- | --- | --- | --- |",
    ]
    for item in airports:
        lines.append(
            f"| {item['airport']} | {item['region']} | "
            f"{'成功' if item.get('ok') else '失败'} | {item.get('source') or '—'} |"
        )

    exit_code = 0 if ok_count > 0 else 1
    exit_code |= publish_gist(
        token, gist_id, files,
        f"ATIS batch {len(airports)} airports, {ok_count} ok, {aggregate['fetched_at']}",
        lines,
    )
    emit_summary(lines)
    return exit_code


def main() -> int:
    token = os.environ.get("GIST_TOKEN", "").strip()
    gist_id = os.environ.get("GIST_ID", "").strip()
    batch_codes = os.environ.get("ATIS_CODES", "").strip()
    if batch_codes:
        return run_batch(batch_codes, token, gist_id)
    return run_single(token, gist_id)


if __name__ == "__main__":
    sys.exit(main())
