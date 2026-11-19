# FID 抓取 → GitHub Gist

`https://www.pegasusintranet.com/F/C/CSN/fid.php` 国内直连超时，这里用 GitHub Actions 的海外节点定时抓取，把结果写入一个 Gist，供国内按需读取。

## 文件

- `.github/workflows/fetch-fid.yml`：每 30 分钟定时 + 手动触发
- `scripts/fetch_fid.py`：抓取、解析表格、发布到 Gist；失败时也会把失败状态写进 Gist，旧数据保留
- `requirements.txt`

## 配置（一次性）

1. 在 GitHub 新建一个仓库（建议 private），把本目录推到默认分支 `main`。
2. 创建 PAT：GitHub → Settings → Developer settings → Personal access tokens →
   Tokens (classic) → 勾选 `gist` 权限，有效期按需。
3. 仓库 → Settings → Secrets and variables → Actions，新建 secret：
   - `FID_GIST_TOKEN`：第 2 步的 token
   - `FID_GIST_ID`：先留空，第一次运行后再填（见第 4 步）
   - `FID_COOKIE`（可选）：页面若需要登录，填浏览器里该站点的 Cookie
4. 仓库 → Actions → `Fetch FID data to Gist` → Run workflow。首次运行会自动创建一个 secret Gist，
   运行摘要里会给出 `GIST_ID`。把它保存为 secret `FID_GIST_ID`，之后每次更新同一个 Gist。

## 输出

Gist 包含：

| 文件 | 内容 |
| --- | --- |
| `fid_latest.json` | 结构化快照：title、tables（header + rows）、text、抓取时间、最终 URL |
| `fid_raw.html` | 原始 HTML |
| `fid_status.json` | 最近一次抓取状态（成功/失败、HTTP 状态、错误信息） |

国内读取地址（`gist.githubusercontent.com` 国内是否能直连取决于当地网络）：

```text
https://gist.githubusercontent.com/<用户名>/<GIST_ID>/raw/fid_latest.json
https://gist.githubusercontent.com/<用户名>/<GIST_ID>/raw/fid_status.json
```

## 调整

- 频率：修改 workflow 里的 cron（UTC，`*/30` = 每 30 分钟）。private 仓库会消耗 Actions 分钟数，可改为每小时 `0 * * * *`。
- 目标地址：仓库 Variables 里加 `FID_URL` 可覆盖默认地址。
- 首次联调：运行后下载 run 的 `fid-snapshot` artifact，检查 `fid_raw.html` 是否抓到真实数据；
  如果抓到的是登录页，就要配置 `FID_COOKIE`。

## 注意

- GitHub 定时任务按 UTC 执行，高峰期可能延迟几分钟。
- 仓库连续 60 天无提交时，GitHub 会暂停定时工作流，需要手动重新启用。
- Gist 是 secret 而非真正私有，持有链接即可读取，不要往里放敏感信息。
