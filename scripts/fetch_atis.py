#!/usr/bin/env python3
"""抓取机场 ATIS：美国机场优先 atis.info，其余使用 atis.guru，并发布到 GitHub Gist。

两种模式：
- 单机场：ATIS_CODE=ZGGG（workflow_dispatch 输入）
- 批量：ATIS_CODES=ZGGG,ZGSZ,...（定时批量缓存，额外生成 atis_all.json 汇总）
"""

import base64
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

HISTORY_DIR = "atis-history"
HISTORY_INDEX = f"{HISTORY_DIR}/index.json"

# 跑道提取规则与 desktop/runways.go 保持一致
RUNWAY_LIST = r"(?:[0-9]{1,2}\s*[LRC]?\b)(?:\s*(?:&|,|AND|/)\s*[0-9]{1,2}\s*[LRC]?\b)*"
RWY_RE = re.compile(r"(?i)\b(?:RWY|RY)S?\s*(" + RUNWAY_LIST + r")")
BARE_RWY_RE = re.compile(
    r"(?i)\b(APCH|APPROACH(?:ES)?|LDG|LANDING|ARRIVAL|ARR|DEPARTURE(?:S)?|DEPARTING|DEPG|DEP|DEPS)\s+("
    + RUNWAY_LIST + r")"
)
RWY_SPLIT_RE = re.compile(r"(?i)\s*(?:&|,|AND|/)\s*")
RWY_CODE_RE = re.compile(r"(?i)^([0-9]{1,2})\s*([LRC])?$")
LAND_KW_RE = re.compile(r"(?i)\b(?:LANDING|ARRIVAL|ARR|APPROACH|APCH|LDG|ILS|VISUAL)\b")
DEP_KW_RE = re.compile(r"(?i)\b(?:DEPARTURE|DEPARTURES|DEPARTING|DEPG|DEP|DEPS|TAKEOFF)\b")
UNAVAIL_RE = re.compile(r"(?i)\b(?:CLSD|CLOSED|OTS)\b")
INFO_RE = re.compile(r"\bINFO\s+([A-Z])\b")


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


def gh_api(method: str, url: str, token: str, payload=None):
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": USER_AGENT,
    }
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read()
        return json.loads(body) if body else {}


def read_repo_file(repo: str, path: str, token: str):
    """读取仓库文件，返回 (text, sha)；文件不存在返回 (None, None)。"""
    try:
        data = gh_api("GET", f"{API_ROOT}/repos/{repo}/contents/{path}?ref=main", token)
    except Exception:
        return None, None
    content = data.get("content") or ""
    try:
        text = base64.b64decode(content).decode("utf-8")
    except Exception:
        text = ""
    return text, data.get("sha")


def commit_repo_files(repo: str, token: str, files: dict[str, str], message: str) -> None:
    """用 Git Data API 在一次提交中写入多个文件。"""
    head = gh_api("GET", f"{API_ROOT}/repos/{repo}/git/ref/heads/main", token)
    parent = head["object"]["sha"]
    base_tree = gh_api("GET", f"{API_ROOT}/repos/{repo}/git/commits/{parent}", token)["tree"]["sha"]
    entries = []
    for path, content in files.items():
        blob = gh_api("POST", f"{API_ROOT}/repos/{repo}/git/blobs", token,
                      {"content": content, "encoding": "utf-8"})
        entries.append({"path": path, "mode": "100644", "type": "blob", "sha": blob["sha"]})
    tree = gh_api("POST", f"{API_ROOT}/repos/{repo}/git/trees", token,
                  {"base_tree": base_tree, "tree": entries})
    commit = gh_api("POST", f"{API_ROOT}/repos/{repo}/git/commits", token,
                    {"message": message, "tree": tree["sha"], "parents": [parent]})
    gh_api("PATCH", f"{API_ROOT}/repos/{repo}/git/refs/heads/main", token, {"sha": commit["sha"]})


def normalize_runway(raw: str) -> str:
    match = RWY_CODE_RE.match(raw.strip())
    if not match:
        return ""
    code = match.group(1)
    side = (match.group(2) or "").upper()
    return code + side


def nearest_direction(text: str, from_end: bool) -> str:
    best, best_dist = "", 1 << 30
    for regex, kind in ((LAND_KW_RE, "landing"), (DEP_KW_RE, "departure")):
        for match in regex.finditer(text):
            dist = len(text) - match.end() if from_end else match.start()
            if dist < best_dist:
                best_dist, best = dist, kind
    return best


def closest_direction(before: str, after: str) -> str:
    return nearest_direction(before, True) or nearest_direction(after, False)


def is_unavailable(after: str) -> bool:
    cut = len(after)
    for ch in ".;":
        idx = after.find(ch)
        if idx >= 0:
            cut = min(cut, idx)
    return bool(UNAVAIL_RE.search(after[:cut]))


def extract_runways(text: str, source: str):
    landing: list[str] = []
    departure: list[str] = []
    if not text or not text.strip():
        return landing, departure
    seen_l: set[str] = set()
    seen_d: set[str] = set()

    def add(list_text: str, kind: str) -> None:
        for part in RWY_SPLIT_RE.split(list_text):
            code = normalize_runway(part)
            if not code:
                continue
            if kind == "landing" and source != "departure" and code not in seen_l:
                seen_l.add(code)
                landing.append(code)
            elif kind == "departure" and source != "arrival" and code not in seen_d:
                seen_d.add(code)
                departure.append(code)

    for match in RWY_RE.finditer(text):
        list_start, list_end = match.start(1), match.end(1)
        before = text[max(0, list_start - 80):list_start]
        cut = max(before.rfind("."), before.rfind(";"))
        if cut >= 0:
            before = before[cut + 1:]
        after = text[list_end:list_end + 60]
        cut = len(after)
        for ch in ".;":
            idx = after.find(ch)
            if idx >= 0:
                cut = min(cut, idx)
        after = after[:cut]
        if is_unavailable(after):
            continue
        kind = closest_direction(before, after)
        if not kind:
            if source == "arrival":
                kind = "landing"
            elif source == "departure":
                kind = "departure"
        if kind:
            add(text[list_start:list_end], kind)

    for match in BARE_RWY_RE.finditer(text):
        keyword = match.group(1).upper()
        list_start, list_end = match.start(2), match.end(2)
        if is_unavailable(text[list_end:list_end + 25]):
            continue
        add(text[list_start:list_end], "landing" if LAND_KW_RE.search(keyword) else "departure")
    return landing, departure


def snapshot_runways(snapshot: dict):
    def text_of(key: str) -> str:
        block = snapshot.get(key) or {}
        return block.get("text") or "" if isinstance(block, dict) else ""

    combined = text_of("combined")
    if combined:
        return extract_runways(combined, "combined")
    landing, _ = extract_runways(text_of("arrival"), "arrival")
    _, departure = extract_runways(text_of("departure"), "departure")
    return landing, departure


def normalize_atis_time(raw: str, fetched_at: str) -> str:
    """把 '2026-11-21 15:09 UTC' / '1509Z' 统一成 '20261121T1509Z'。"""
    raw = (raw or "").strip()
    if not raw:
        return ""
    match = re.search(r"(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):?(\d{2})", raw)
    if match:
        y, mo, d, hh, mm = match.groups()
        return f"{y}{mo}{d}T{hh}{mm}Z"
    match = re.match(r"^(\d{2})(\d{2})Z$", raw)
    if not match:
        return ""
    hh, mm = int(match.group(1)), int(match.group(2))
    try:
        base = dt.datetime.strptime(fetched_at, "%Y-%m-%d %H:%M:%SZ")
    except Exception:
        base = dt.datetime.now(dt.timezone.utc)
    stamp = base.replace(hour=hh, minute=mm, second=0, microsecond=0)
    if stamp - base > dt.timedelta(hours=1):
        stamp -= dt.timedelta(days=1)
    return stamp.strftime("%Y%m%dT%H%MZ")


def history_row(snapshot: dict) -> tuple[str, str, str] | None:
    """返回 (month, 文件路径, CSV 行)；无跑道时不记录。"""
    code = snapshot.get("airport") or ""
    landing, departure = snapshot_runways(snapshot)
    if not landing and not departure:
        return None
    fetched_at = snapshot.get("fetched_at") or ""
    combined = snapshot.get("combined") or {}
    arrival = snapshot.get("arrival") or {}
    departure_block = snapshot.get("departure") or {}
    comb_time = normalize_atis_time(combined.get("updated_at", ""), fetched_at)
    arr_time = comb_time or normalize_atis_time(arrival.get("updated_at", ""), fetched_at)
    dep_time = comb_time or normalize_atis_time(departure_block.get("updated_at", ""), fetched_at)
    info = (combined.get("code") or arrival.get("code") or departure_block.get("code") or "").strip()
    if not info:
        for key in ("combined", "arrival", "departure"):
            match = INFO_RE.search((snapshot.get(key) or {}).get("text") or "")
            if match:
                info = match.group(1)
                break
    stamp = arr_time or dep_time or fetched_at.replace("-", "").replace(" ", "T").replace(":", "")
    month = f"{stamp[0:4]}-{stamp[4:6]}"
    row = ",".join([
        code,
        arr_time or "-",
        dep_time or "-",
        info or "-",
        " ".join(landing) or "-",
        " ".join(departure) or "-",
    ])
    return month, f"{HISTORY_DIR}/{code}/{month}.csv", row


def append_history(airports: list[dict], lines: list[str]) -> None:
    token = os.environ.get("HISTORY_TOKEN", "").strip()
    repo = os.environ.get("HISTORY_REPO", "").strip()
    if not token or not repo:
        lines.append("- 历史归档：跳过（缺少 HISTORY_TOKEN/HISTORY_REPO）")
        return

    try:
        index_text, _ = read_repo_file(repo, HISTORY_INDEX, token)
        index = json.loads(index_text) if index_text else {"airports": {}}
        months_by_airport = index.setdefault("airports", {})
        pending: dict[str, str] = {}
        added: dict[str, int] = {}
        for snapshot in airports:
            record = history_row(snapshot)
            if not record:
                continue
            month, path, row = record
            if path not in pending:
                existing, _ = read_repo_file(repo, path, token)
                pending[path] = existing or ""
            current = pending[path]
            rows = set(current.splitlines())
            if row in rows:
                continue
            if current and not current.endswith("\n"):
                current += "\n"
            pending[path] = current + row + "\n"
            added[path] = added.get(path, 0) + 1
            months = months_by_airport.setdefault(snapshot["airport"], [])
            if month not in months:
                months.append(month)
                months.sort()

        changed = {path: text for path, text in pending.items() if added.get(path)}
        if not changed:
            lines.append("- 历史归档：无新记录")
            return
        if months_by_airport:
            index["updated_at"] = utc_now()
            changed[HISTORY_INDEX] = json.dumps(index, ensure_ascii=False, indent=2) + "\n"
        total = sum(added.values())
        commit_repo_files(repo, token, changed,
                          f"ATIS history: +{total} records ({utc_now()})")
        lines.append(f"- 历史归档：新增 {total} 条（{len(added)} 个分片）")
    except Exception as exc:
        print(f"::error::ATIS history commit failed: {exc}")
        lines.append(f"- 历史归档失败：`{exc}`")


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

    append_history(airports, lines)

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
