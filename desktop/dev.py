#!/usr/bin/env python3
"""开发启动器：前端热刷新 + Go 代码自动重编译重启。

用法：双击 dev.bat，或在 desktop 目录执行 python dev.py

工作方式：
- 在 http://127.0.0.1:5173 提供 desktop/frontend，并注入 1 秒轮询的自动刷新脚本
- 以 MYGO_DEV_URL 指向该地址运行应用（窗口加载开发服务器）
- 监听 Go 文件变化，重新 go build 并重启应用
"""

import hashlib
import http.server
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
FRONTEND = ROOT / "frontend"
DEV_HOST = "127.0.0.1"
DEV_PORT = 5173
BUILD_DIR = ROOT / ".dev"
EXE = BUILD_DIR / "anc-fid-dev.exe"
WATCH_FILES = ["main.go", "atis.go", "token_default.go", "token_local.go", "go.mod", "go.sum"]

LIVE_RELOAD = """
<script>
(function () {
  var current = '';
  async function poll() {
    try {
      var res = await fetch('/__ver', { cache: 'no-store' });
      var v = await res.text();
      if (current && v !== current) { location.reload(); return; }
      current = v;
    } catch (e) {}
  }
  poll();
  setInterval(poll, 1000);
})();
</script>
"""


def frontend_version() -> str:
    digest = hashlib.sha1()
    for path in sorted(FRONTEND.rglob("*")):
        if path.is_file():
            stat = path.stat()
            digest.update(str(path.relative_to(FRONTEND)).encode())
            digest.update(str(stat.st_mtime_ns).encode())
    return digest.hexdigest()


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(FRONTEND), **kwargs)

    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path.split("?")[0] == "/__ver":
            body = frontend_version().encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        requested = self.translate_path(self.path)
        if os.path.isdir(requested):
            requested = os.path.join(requested, "index.html")
        if os.path.isfile(requested) and requested.endswith(".html"):
            text = Path(requested).read_text(encoding="utf-8", errors="replace")
            idx = text.lower().rfind("</body>")
            if idx >= 0:
                text = text[:idx] + LIVE_RELOAD + text[idx:]
            body = text.encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        super().do_GET()

    def end_headers(self):
        self.send_header("Cache-Control", "no-store")
        super().end_headers()


def go_env():
    env = os.environ.copy()
    env.setdefault("GOPROXY", "https://goproxy.cn,direct")
    env.setdefault("GOTOOLCHAIN", "auto")
    return env


def build() -> bool:
    BUILD_DIR.mkdir(exist_ok=True)
    print("[dev] go build ...", flush=True)
    result = subprocess.run(["go", "build", "-o", str(EXE), "."], cwd=ROOT, env=go_env())
    if result.returncode != 0:
        print("[dev] build failed, fix errors and save again", flush=True)
        return False
    return True


def start_app():
    env = go_env()
    env["MYGO_DEV_URL"] = f"http://{DEV_HOST}:{DEV_PORT}"
    print("[dev] starting app ...", flush=True)
    return subprocess.Popen([str(EXE)], cwd=ROOT, env=env)


def stop_app(proc):
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()


def snapshot():
    values = {}
    for name in WATCH_FILES:
        path = ROOT / name
        if path.is_file():
            values[name] = path.stat().st_mtime_ns
    return values


def main():
    if not FRONTEND.is_dir():
        sys.exit("frontend directory not found")

    server = http.server.ThreadingHTTPServer((DEV_HOST, DEV_PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print(f"[dev] frontend: http://{DEV_HOST}:{DEV_PORT}", flush=True)
    print("[dev] 修改 frontend/ 会自动刷新；修改 Go 文件会自动重编译重启", flush=True)

    if not build():
        server.shutdown()
        sys.exit(1)
    proc = start_app()
    last = snapshot()

    try:
        while True:
            time.sleep(1)
            current = snapshot()
            if current != last:
                last = current
                print("[dev] Go files changed, rebuilding ...", flush=True)
                stop_app(proc)
                if build():
                    proc = start_app()
                else:
                    proc = None
            if proc is None or proc.poll() is not None:
                print("[dev] app exited; Ctrl+C to stop the dev server", flush=True)
                proc = None
    except KeyboardInterrupt:
        pass
    finally:
        stop_app(proc)
        server.shutdown()
        print("[dev] stopped", flush=True)


if __name__ == "__main__":
    main()
