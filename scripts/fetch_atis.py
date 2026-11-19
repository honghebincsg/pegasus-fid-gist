#!/usr/bin/env python3
"""抓取机场 ATIS：美国机场优先 atis.info，其余使用 atis.guru，并发布到 GitHub Gist。"""

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


def build_text(data: dict) -> str:
    parts = []
    combined = data.get("combined")
    if combined:
        parts.append(
            f"COMBINED ATIS {combined.get('code', '')} {combined.get('updated_at', '')}\n"
            f"{combined.get('text', '')}"
        )
    else:
        arrival = data.get("arrival") or {}
        departure = data.get("departure") or {}
        parts.append(f"ARRIVAL ATIS {arrival.get('updated_at', '')}\n{arrival.get('text', '')}")
        parts.append(f"DEPARTURE ATIS {departure.get('updated_at', '')}\n{departure.get('text', '')}")
    return "\n\n".join(parts).strip() + "\n"


def publish(token: str, gist_id: str, files: dict[str, str], description: str) -> dict:
    payload = {
        "description": description,
        "files": {name: {"content": content} for name, content in files.items()},
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


def main() -> int:
    code = (os.environ.get("ATIS_CODE") or DEFAULT_CODE).strip().upper()
    if not re.fullmatch(r"[A-Z]{4}", code):
        print(f"::error::invalid ICAO code: {code!r}")
        return 2
    token = os.environ.get("GIST_TOKEN", "").strip()
    gist_id = os.environ.get("GIST_ID", "").strip()
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    status = {"ok": False, "checked_at": utc_now(), "airport": code}
    files: dict[str, str] = {}
    exit_code = 0
    snapshot = None

    try:
        info = fetch_atisinfo(code)
        if info:
            data = {
                "combined": info["combined"],
                "arrival": info["arrival"],
                "departure": info["departure"],
            }
            snapshot = {
                "source_url": info["source_url"],
                "airport": code,
                "source": "atis.info",
                "fetched_at": utc_now(),
                **data,
            }
            status.update(
                ok=True,
                source="atis.info",
                combined_code=(info["combined"] or {}).get("code", ""),
                arrival_updated=info["arrival"]["updated_at"],
                departure_updated=info["departure"]["updated_at"],
            )
            files[f"atis_{code.lower()}.json"] = json.dumps(snapshot, ensure_ascii=False, indent=2)
            files[f"atis_{code.lower()}.txt"] = build_text(data)
        else:
            url = f"https://atis.guru/atis/{code}"
            html = fetch(url)
            text = page_text(html)
            data = extract(text, code)
            if data["arrival"]["text"] or data["departure"]["text"]:
                data["combined"] = None
                snapshot = {
                    "source_url": url,
                    "airport": code,
                    "source": "atis.guru",
                    "fetched_at": utc_now(),
                    **data,
                }
                status.update(
                    ok=True,
                    source="atis.guru",
                    arrival_updated=data["arrival"]["updated_at"],
                    departure_updated=data["departure"]["updated_at"],
                )
                files[f"atis_{code.lower()}.json"] = json.dumps(snapshot, ensure_ascii=False, indent=2)
                files[f"atis_{code.lower()}.txt"] = build_text(data)
            else:
                status.update(ok=False, source="atis.guru", error="该机场暂无 ATIS 数据")
                print(f"::warning::no ATIS data found for {code}")
    except Exception as exc:
        status["error"] = f"{type(exc).__name__}: {exc}"
        exit_code = 1
        print(f"::error::Fetch failed: {status['error']}")

    files[f"atis_{code.lower()}_status.json"] = json.dumps(status, ensure_ascii=False, indent=2)
    for name, content in files.items():
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
        lines.append(f"- 错误：`{status['error']}`")

    if token and gist_id:
        try:
            gist = publish(token, gist_id, files, f"ATIS {code}, last success {status['checked_at']}")
            lines.append(f"- Gist：{gist.get('html_url', '')}")
        except Exception as exc:
            exit_code = 1
            print(f"::error::Gist publish failed: {exc}")
            lines.append(f"- Gist 发布失败：`{exc}`")
    else:
        lines.append("- 未发布到 Gist（缺少 GIST_TOKEN/GIST_ID）")

    summary = "\n".join(lines) + "\n"
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as fh:
            fh.write(summary)
    else:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
        print(summary)
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
