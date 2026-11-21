# CSN ANC 航班信息与 ATIS

Windows 桌面程序 + GitHub Actions 抓取 + Gist 存储的组合方案。

- **桌面程序**：MYGO（Go）+ Vue 3（本地打包），左侧菜单切换「航班看板」和「ATIS 查询」
- **航班看板**：CSN 在 ANC（安克雷奇）的航班与停机位，每 60 秒刷新
- **ATIS 查询**：输入 4 位 ICAO 机场代码，美国机场优先 atis.info，其他机场使用 atis.guru
- **云端**：GitHub Actions 抓取，Gist 存储最新 JSON；cron-job.org 每 5 分钟触发 FID 抓取

详细说明见 [docs/交接说明.md](docs/交接说明.md)。

## 目录

```text
desktop/                 Windows 桌面程序（Go + MYGO + Vue）
scripts/                 GitHub Actions 抓取脚本
.github/workflows/       FID / ATIS 工作流
docs/                    方案说明、交接说明、界面截图
```

## 本地构建

环境：Windows 10+（WebView2）、Go 1.27+、go-winres。

```bat
cd desktop
go-winres make
go build -ldflags="-H windowsgui" -o anc-fid.exe .
```

- 前端资源（含 `vendor/vue.global.prod.js`）通过 `//go:embed` 打包进 exe
- 双击 `anc-fid.exe` 运行；无控制台窗口
- 修改前端或 Go 代码后重新执行 `go build` 即可

## 开发热更新

```bat
cd desktop
dev.bat
```

- 修改 `frontend/` 保存后窗口约 1 秒自动刷新
- 修改 Go 文件保存后自动重新编译并重启窗口
- 开发服务器只监听本机 5173（仅开发时），生产程序不监听本地端口
- 开发构建产物在 `desktop/.dev/`，不纳入仓库

## SQLite 数据持久化

程序首次运行会在 **exe 同目录**创建 `anc-fid.db`，作为独立数据文件，不打包进 exe：

| 表 | 内容 |
| --- | --- |
| `fid_snapshots` | 每次 Gist 快照（fetched_at、航班数） |
| `fid_flights` | 航班/机位**最新状态，同一航班唯一**（覆盖率见下） |
| `atis_queries` | 每次 ATIS 查询记录（机场、来源、获取方式、ATIS 代号、文本） |

- 同一航班按 Pegasus 记录号 `recno` 唯一；没有记录号时按 `航班号 + 日期 + 航线`
- 新快照会覆盖机位、时刻、备注等字段，`first_seen_at` 保留首次出现时间，`last_seen_at` 更新为最近出现时间
- `fid_snapshots` 保留每个快照的元数据（抓取时间、航班数）
- 可用 DB Browser for SQLite 或 `sqlite3 anc-fid.db` 查看
- 若程序目录不可写，会自动回退到 `%LOCALAPPDATA%\anc-fid\anc-fid.db`
- 开发模式（`dev.bat`）下数据库位于 `desktop/.dev/`
- 可通过环境变量 `ANC_FID_DB` 指定其他路径

示例：

```sql
SELECT fetched_at, flight_count FROM fid_snapshots ORDER BY id DESC LIMIT 10;
SELECT flight_number, gate, from_station, to_station, sta, eta FROM fid_flights ORDER BY id DESC LIMIT 20;
SELECT airport, updated_at, query_at FROM atis_queries ORDER BY id DESC LIMIT 20;
```

## Token 配置

程序触发 GitHub Actions 时需要 token，读取顺序：

1. 环境变量 `GH_DISPATCH_TOKEN`
2. 本地文件 `%LOCALAPPDATA%\anc-fid\dispatch_token.txt`
3. 编译时内置常量（`desktop/token_local.go`）

仓库不包含 `token_local.go`（已在 `.gitignore` 中）。新电脑 clone 后：

- 推荐：创建 `desktop/token_local.go`，内容为 `func init() { embeddedDispatchToken = "<你的token>" }`
- 或：设置环境变量 / 本地 token 文件

## 云端配置

| 组件 | 值 |
| --- | --- |
| 仓库 | `honghebincsg/pegasus-fid-gist`（public） |
| Gist | `honghebincsg/0f820895dffee67ecf897420e1e430cb` |
| FID workflow | `.github/workflows/fetch-fid.yml` |
| ATIS workflow | `.github/workflows/fetch-atis.yml`（workflow_dispatch，`inputs.airport`） |
| secrets | `FID_GIST_TOKEN`、`FID_GIST_ID` |
| 外部定时器 | cron-job.org 任务 `Trigger FID fetch`，每 5 分钟 |

Gist 文件命名：

- FID：`fid_latest.json`、`fid_flights.csv`、`fid_status.json`
- ATIS：每个机场一个 JSON，如 `KLAX.json`、`PANC.json`、`EHAM.json`

## 数据流

1. **FID**：cron-job.org → Actions 抓取 Pegasus FID 接口 → 覆盖写 Gist → 桌面程序每 60 秒读取
2. **ATIS**：
   - 本机直连 atis.info / atis.guru（1–4 秒）并显示
   - 成功后后台异步触发 Actions，把同一份结果同步到 Gist
   - 本机失败时触发 Actions 重新抓取并等待，不再读取旧的 Gist 数据
   - 界面底部标注获取方式：`本机直连源网站` 或 `GitHub Actions 抓取后从 Gist 读取`
   - 自动解析落地/起飞跑道：顶部大号跑道徽标 + 原文加粗高亮
   - 显示机场中文名、英文名、ICAO 和 IATA 代码

## 注意

- 仓库是 public，**不要提交 token、Cookie 等凭据**
- `token_local.go`、`dist/`、`*.exe`、`vibe_images/` 已在 `.gitignore` 中排除
- Gist 是 secret 但持有链接即可读取，不要存放敏感信息
- GitHub Actions 冷启动约 10 秒；本机直连通常 1–4 秒
