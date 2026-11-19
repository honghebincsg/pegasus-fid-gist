#!/usr/bin/env python3
"""抓取 atis.guru 的机场 ATIS 页面并发布到 GitHub Gist。"""

import datetime as dt
import html as html_lib
import json
import os
import re
import sys
import time
import urllib.request
from pathlib import Path

DEFAULT_URL = "https://atis.guru/atis/ZGGG"
API_ROOT = "https://api.github.com"
OUT_DIR = Path("out")


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")


def fetch(url: str, attempts: int = 3, timeout: int = 30) -> str:
    headers = {
        "User-Agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/124.0.0.0 Safari/537.36"
        ),
        "Accept": "text/html,application/xhtml+xml",
    }
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
                time.sleep(3 * attempt)
    raise last_error


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


def extract(text: str) -> dict:
    one = re.sub(r"\s+", " ", text)

    def grab(pattern: str) -> str:
        match = re.search(pattern, one, re.S)
        return clean(match.group(1)) if match else ""

    return {
        "arrival": {
            "updated_at": grab(r"Arrival ATIS ([\d-]+ [\d:]+) UTC"),
            "text": grab(r"ZGGG ARR ATIS (.*?) Departure ATIS"),
        },
        "departure": {
            "updated_at": grab(r"Departure ATIS ([\d-]+ [\d:]+) UTC"),
            "text": grab(r"ZGGG DEP ATIS (.*?) METAR"),
        },
    }


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
    url = (os.environ.get("ATIS_URL") or DEFAULT_URL).strip()
    token = os.environ.get("GIST_TOKEN", "").strip()
    gist_id = os.environ.get("GIST_ID", "").strip()
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    status = {"ok": False, "checked_at": utc_now(), "source_url": url}
    files: dict[str, str] = {}
    exit_code = 0

    try:
        html = fetch(url)
        text = page_text(html)
        data = extract(text)
        snapshot = {"source_url": url, "fetched_at": utc_now(), **data}
        files["atis_zggg.json"] = json.dumps(snapshot, ensure_ascii=False, indent=2)
        files["atis_zggg.txt"] = (
            f"ARRIVAL ATIS {data['arrival']['updated_at']} UTC\n{data['arrival']['text']}\n\n"
            f"DEPARTURE ATIS {data['departure']['updated_at']} UTC\n{data['departure']['text']}\n"
        )
        status.update(
            ok=True,
            arrival_updated=data["arrival"]["updated_at"],
            departure_updated=data["departure"]["updated_at"],
            arrival_text=data["arrival"]["text"][:200],
        )
    except Exception as exc:
        status["error"] = f"{type(exc).__name__}: {exc}"
        exit_code = 1
        print(f"::error::Fetch failed: {status['error']}")

    files["atis_zggg_status.json"] = json.dumps(status, ensure_ascii=False, indent=2)
    for name, content in files.items():
        (OUT_DIR / name).write_text(content, encoding="utf-8", newline="\n")

    lines = [
        "## ATIS 抓取结果",
        "",
        f"- 状态：{'成功' if status['ok'] else '失败'}",
        f"- 时间（UTC）：{status['checked_at']}",
    ]
    if status["ok"]:
        lines.append(f"- 到达 ATIS 更新：{status['arrival_updated'] or '—'}")
        lines.append(f"- 起飞 ATIS 更新：{status['departure_updated'] or '—'}")
        lines.append(f"- 到达文本：{status['arrival_text']}")
    else:
        lines.append(f"- 错误：`{status['error']}`")

    if token and gist_id:
        try:
            gist = publish(token, gist_id, files, f"ATIS ZGGG, last success {status['checked_at']}")
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
