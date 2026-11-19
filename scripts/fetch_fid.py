#!/usr/bin/env python3
"""抓取 CSN FID 航班数据并发布到 GitHub Gist。"""

import csv
import datetime as dt
import io
import json
import os
import sys
import time
import urllib.parse
from pathlib import Path

import requests
from bs4 import BeautifulSoup

DEFAULT_URL = "https://www.pegasusintranet.com/F/C/CSN/fid.php"
FID_CODE = "CSN"
API_ROOT = "https://api.github.com"
OUT_DIR = Path("out")
MAX_RAW_BYTES = 8 * 1024 * 1024


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")


def build_headers() -> dict:
    headers = {
        "User-Agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/124.0.0.0 Safari/537.36"
        ),
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Cache-Control": "no-cache",
    }
    cookie = os.environ.get("FID_COOKIE", "").strip()
    if cookie:
        headers["Cookie"] = cookie
    return headers


def fetch_page(session: requests.Session, url: str, headers: dict, timeout: int, attempts: int = 3) -> requests.Response:
    last_error = None
    for attempt in range(1, attempts + 1):
        try:
            resp = session.get(url, headers=headers, timeout=timeout)
            if resp.status_code >= 500 and attempt < attempts:
                raise requests.HTTPError(f"HTTP {resp.status_code}", response=resp)
            resp.raise_for_status()
            return resp
        except Exception as exc:
            last_error = exc
            print(f"[page] attempt {attempt}/{attempts} failed: {exc}", flush=True)
            if attempt < attempts:
                time.sleep(5 * attempt)
    raise last_error


def fetch_flights(session: requests.Session, page_url: str, headers: dict, timeout: int, attempts: int = 3):
    api_url = urllib.parse.urljoin(page_url, "../ajaxgetflights.php")
    ajax_headers = {**headers, "Referer": page_url, "X-Requested-With": "XMLHttpRequest"}
    last_error = None
    for attempt in range(1, attempts + 1):
        try:
            resp = session.post(
                api_url,
                data={"fid": FID_CODE, "cust": FID_CODE},
                headers=ajax_headers,
                timeout=timeout,
            )
            resp.raise_for_status()
            text = resp.text.strip()
            parts = text.split("|", 2)
            if len(parts) < 2 or parts[0] != "OK":
                raise RuntimeError(f"接口返回异常：{text[:300]}")
            flights = [] if parts[1] == "none" else json.loads(parts[1])
            if not isinstance(flights, list):
                raise RuntimeError(f"航班数据不是数组：{str(flights)[:200]}")
            advisory = parts[2] if len(parts) > 2 else ""
            return api_url, flights, advisory
        except Exception as exc:
            last_error = exc
            print(f"[ajax] attempt {attempt}/{attempts} failed: {exc}", flush=True)
            if attempt < attempts:
                time.sleep(5 * attempt)
    raise last_error


def decode_html(resp: requests.Response) -> tuple[str, str]:
    raw = resp.content
    candidates = []
    if resp.encoding:
        candidates.append(resp.encoding)
    candidates.extend(["utf-8", "gb18030"])
    for enc in candidates:
        if enc.lower().replace("_", "-").startswith("iso-8859"):
            continue
        try:
            return raw.decode(enc), enc
        except (LookupError, UnicodeDecodeError):
            continue
    return raw.decode("utf-8", errors="replace"), "utf-8 (replace)"


def make_soup(html: str) -> BeautifulSoup:
    try:
        return BeautifulSoup(html, "lxml")
    except Exception:
        return BeautifulSoup(html, "html.parser")


def hidden_value(soup: BeautifulSoup, element_id: str) -> str:
    node = soup.find("input", id=element_id)
    return node.get("value", "") if node else ""


def flights_to_csv(flights: list[dict]) -> str:
    if not flights:
        return ""
    fields: list[str] = []
    for row in flights:
        for key in row:
            if key not in fields:
                fields.append(key)
    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=fields, extrasaction="ignore", lineterminator="\n")
    writer.writeheader()
    for row in flights:
        writer.writerow({key: "" if row.get(key) is None else row.get(key) for key in fields})
    return buf.getvalue()


def publish_gist(token: str, gist_id: str, files: dict[str, str], description: str) -> tuple[dict, bool]:
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    payload = {
        "description": description,
        "files": {name: {"content": content} for name, content in files.items()},
    }
    if gist_id:
        resp = requests.patch(f"{API_ROOT}/gists/{gist_id}", headers=headers, json=payload, timeout=30)
        created = False
    else:
        payload["public"] = False
        resp = requests.post(f"{API_ROOT}/gists", headers=headers, json=payload, timeout=30)
        created = True
    if resp.status_code >= 300:
        raise requests.HTTPError(f"Gist API {resp.status_code}: {resp.text[:500]}", response=resp)
    return resp.json(), created


def raw_file_url(gist: dict, filename: str) -> str:
    owner = (gist.get("owner") or {}).get("login", "")
    gist_id = gist.get("id", "")
    if not owner or not gist_id:
        return ""
    return f"https://gist.githubusercontent.com/{owner}/{gist_id}/raw/{filename}"


def write_summary(lines: list[str]) -> None:
    text = "\n".join(lines) + "\n"
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(text)
    else:
        print(text)


def main() -> int:
    url = (os.environ.get("FID_URL") or DEFAULT_URL).strip()
    timeout = int(os.environ.get("FID_TIMEOUT") or "30")
    token = os.environ.get("FID_GIST_TOKEN", "").strip()
    gist_id = os.environ.get("FID_GIST_ID", "").strip()

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    status = {"ok": False, "checked_at": utc_now(), "source_url": url}
    files: dict[str, str] = {}
    exit_code = 0
    session = requests.Session()
    headers = build_headers()

    try:
        page = fetch_page(session, url, headers, timeout)
        html, encoding = decode_html(page)
        soup = make_soup(html)
        api_url, flights, advisory = fetch_flights(session, url, headers, timeout)
        snapshot = {
            "source_url": url,
            "api_url": api_url,
            "fetched_at": utc_now(),
            "page_title": soup.title.get_text(" ", strip=True) if soup.title else "",
            "as_of_local": hidden_value(soup, "hid_fidtimestamp"),
            "times": hidden_value(soup, "hid_times"),
            "http_status": page.status_code,
            "encoding": encoding,
            "flight_count": len(flights),
            "advisory": advisory,
            "flights": flights,
        }
        if len(html.encode("utf-8")) > MAX_RAW_BYTES:
            html = html[: MAX_RAW_BYTES // 4] + "\n<truncated/>"
        files["fid_latest.json"] = json.dumps(snapshot, ensure_ascii=False, indent=2)
        files["fid_flights.csv"] = flights_to_csv(flights)
        files["fid_raw.html"] = html
        status.update(
            ok=True,
            http_status=page.status_code,
            flight_count=len(flights),
            as_of_local=snapshot["as_of_local"],
            advisory=advisory,
        )
    except Exception as exc:
        status["error"] = f"{type(exc).__name__}: {exc}"
        exit_code = 1
        print(f"::error::Fetch failed: {status['error']}")

    files["fid_status.json"] = json.dumps(status, ensure_ascii=False, indent=2)
    for name, content in files.items():
        (OUT_DIR / name).write_text(content, encoding="utf-8", newline="\n")

    summary = [
        "## FID 抓取结果",
        "",
        f"- 状态：{'成功' if status['ok'] else '失败'}",
        f"- 时间（UTC）：{status['checked_at']}",
    ]
    if status["ok"]:
        summary.append(f"- 航班数：{status['flight_count']}")
        if status.get("as_of_local"):
            summary.append(f"- 页面时间：{status['as_of_local']}（UTC {status.get('times', '')}）")
        if status.get("advisory") and status["advisory"] != "None":
            summary.append(f"- 通告：{status['advisory'][:200]}")
    else:
        summary.append(f"- 错误：`{status['error']}`")

    if not token:
        print("::warning::FID_GIST_TOKEN 未配置，仅生成本地快照（见 artifact）。")
        summary.append("- 未发布到 Gist：缺少 `FID_GIST_TOKEN` secret。")
    else:
        description = (
            f"FID snapshot, last success {status['checked_at']}"
            if status["ok"]
            else f"FID fetch FAILED at {status['checked_at']}"
        )
        try:
            gist, created = publish_gist(token, gist_id, files, description)
            owner = (gist.get("owner") or {}).get("login", "")
            summary.append(f"- Gist：{gist.get('html_url', '')}")
            if gist.get("id"):
                summary.append(f"- GIST_ID：`{gist['id']}`")
                if owner:
                    summary.append(f"- Gist 页面：https://gist.github.com/{owner}/{gist['id']}")
            for name in ("fid_latest.json", "fid_flights.csv", "fid_status.json"):
                link = raw_file_url(gist, name)
                if link:
                    summary.append(f"- raw `{name}`：{link}")
            if created:
                summary.append(
                    "- 这是新建的 Gist。把上面的 GIST_ID 保存为仓库 secret `FID_GIST_ID`，"
                    "之后每次运行都会更新同一个 Gist。"
                )
                print("::warning::Gist created. Save its id as secret FID_GIST_ID.")
        except Exception as exc:
            exit_code = 1
            print(f"::error::Gist publish failed: {exc}")
            summary.append(f"- Gist 发布失败：`{exc}`")

    write_summary(summary)
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
