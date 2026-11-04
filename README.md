# UI 自动化测试平台 · 接口采集

> **在业务页面上正常点一遍**，就得到一条可回放的测试流程；
> **在业务页面上正常点一遍**，也就得到一个能定时跑的数据管道。

单文件 Go 服务端 + 浏览器插件控制台，零构建步骤（前端是原生 JS），Windows / Linux 同一份代码。

<p>
<img src="docs/screenshots/demo-gallery.png" alt="示例场景总览" width="49%">
<img src="docs/screenshots/recording-detail.png" alt="录制详情" width="49%">
</p>
<p>
<img src="docs/screenshots/run-detail-floating-log.png" alt="运行详情与悬浮日志" width="49%">
<img src="docs/screenshots/settings-assist.png" alt="设置" width="49%">
</p>

---

## 一、两条产品线

| 线 | 你做什么 | 得到什么 | 手册 |
|---|---|---|---|
| **UI 自动化** | 插件录一遍业务流程 | 可回放的步骤（稳定定位 + 自愈），一键执行、逐步截图与日志 | [docs/GUIDE.md](docs/GUIDE.md) |
| **接口采集** | 插件（或内置浏览器）录一遍接口 | 定时抓取任务：自动翻页 → 拍平成行 → 写 SQLite/MySQL/PG/Excel/JSON | [docs/COLLECT.md](docs/COLLECT.md) |

两条线共用同一套底座：录制 → 筛选 → 配置 → 执行 → 看结果；数据都落在 `data/`。

---

## 二、30 秒跑起来

```powershell
cd uitest
go build -o uitest-server.exe .      # 依赖全是纯 Go（含 SQLite/MySQL/PG 驱动、Excel 写入库）
.\uitest-server.exe                  # 或 .\start.ps1 后台启动 / .\stop.ps1 停止
```

打开 <http://127.0.0.1:8490> → 首次启动会自建 `data/`（数据文件、日志、快照、浏览器 profile）。

**UI 自动化**（5 分钟）：

1. 装插件：`edge://extensions` → 开发人员模式 → 加载解压缩的扩展 → 选 `extension/`；
2. 「录制」→ 新建录制 → 起始页填 `http://127.0.0.1:8490/demo/admin-list.html` → 开始录制；
3. 回业务页正常操作（搜一下、点行内按钮、关弹窗）→ 停止录制；
4. 录制详情右侧「数据准备」写一句 `价格 99-299，库存 50` → 「AI 生成测试数据」（没配模型自动用内置规则）；
5. 点「执行测试」→ 运行详情看逐步结果、实际定位与实时日志。

**接口采集**（5 分钟）：

1. 顶部导航「接口采集」→ 两种录法任选：
   - 插件弹窗里勾着「同时录制接口请求」，在你自己的浏览器里正常操作（推荐）；
   - 或在「录制接口」卡片里填 `http://127.0.0.1:8490/demo/api-center.html`，用内置浏览器录；
2. 停止录制 → 进「录制结果」勾选要抓的接口（可先筛选、可删掉心跳/埋点）→「下一步：配置采集任务」；
3. 定时选「每 1 小时」、落库选「Excel 表格」→ 创建 → 点「立即运行」；
4. 回首页「产出文件」下载 `.xlsx`（示例数据 23 条，会自动翻完 5 页）。

想先要演示数据？`powershell -ExecutionPolicy Bypass -File scripts\seed-demo.ps1`
（`data/` 不入库，新克隆的仓库第一次是空的，跑它就能生成 8 个示例场景）。

---

## 三、能力总览

### UI 自动化

| 能力 | 说明 |
|---|---|
| 录制即脚本 | 插件把点击/输入/选择/勾选/上传/断言实时上报，每步自动生成中文备注 |
| 稳定 ID 绑定 | 录制时算 `data-uitest-id`（确定性算法），执行优先用它；失效时多级语义兜底 + 指纹自愈 |
| 重新打包安全 | hash 类名、`data-v-*`、组件库自动 id 全部过滤，只认表单项标题/按钮文字/表格行等业务语义 |
| AI 数据准备 | OpenAI 兼容接口；未配置或失败自动退到内置规则（能听懂「价格 99-299」这类指令） |
| 一键刷新定位 | 老录制（只有 css）一次升级成全量 uid，语义优先重算，不固化有歧义的 css |
| 实时日志 | SSE 推送，悬浮窗可拖动/缩放/复制；断流自动退轮询 |
| 可配置收尾 | 运行结束按设置关标签页 / 关浏览器 / 保留现场 |
| 数据快照 | 每次启动留快照，误删可一键恢复（恢复前自动再存一份） |

### 接口采集

| 能力 | 说明 |
|---|---|
| 零侵入录制 | 插件录（你自己的浏览器，登录态/代理都不受影响）或内置浏览器 CDP 旁路录，都不装代理、不改页面 |
| 结果可裁剪 | 逐条删除 / 勾选批量删除 / 按筛选结果整体删除；删掉的不会被插件重发加回 |
| 自动识别结构 | 自动找记录数组（`data.list`）、推断字段映射（`owner.name → owner_name`），可手改 |
| 自动翻页 | 页码 / 偏移 / 游标 / 请求体内参数四种形态；按总数·has_more·短页停；连两页相同即判"不吃分页"，重复行去重 |
| 落库 6 种 | SQLite、MySQL、PostgreSQL、**Excel `.xlsx`**、**JSON `.json`**、JSONL；前两类文件可在控制台直接下载 |
| 一键清空 | 首页「清空数据」可分别清掉产出文件 / 采集任务与运行记录 / 仅运行记录 / 内存里的录制结果 |
| 自动建表补列 | 表不存在自动建，响应多字段自动 `ALTER TABLE ADD COLUMN`；`upsert` 按主键覆盖 |
| 礼貌采集 | 单任务串行 + 最小间隔 + 随机抖动 + 指数退避；429 按 `Retry-After` 等；401/403 立即停手；连续失败熔断 |
| 先看再写 | 「试抓预览」不写库看列与前几行；「测试落库」只验连接 |

> **边界**：面向**你有权访问的系统**（自家后台、测试环境、内部系统）。
> 只做"像人一样正常访问"，不做代理池 / 指纹伪造 / 验证码绕过这类对抗风控的事。

---

## 四、目录地图

```
uitest/
├─ main.go                 # 入口：配置合并、go:embed web/demo、路由装配
├─ internal/
│  ├─ api/                 # HTTP 接口（录制/步骤/运行/元素/设置/备份/SSE/采集/产出文件）
│  ├─ browser/             # 内置浏览器：启动自愈、CDP 页面操作、netcap.go（旁路录请求）
│  ├─ cdp/                 # 极简 CDP 客户端（WebSocket + 请求/事件）
│  ├─ engine/              # 执行器：步骤执行、变量替换、自愈回写、一键刷新定位
│  ├─ collect/             # ★ 接口采集：筛选/分页/调度/执行/sink（SQL·Excel·JSON）
│  ├─ ai/ keyname/         # AI 与规则数据生成、中文标签 → 英文字段名
│  ├─ model/ store/ config/# 数据模型、JSON 持久化（含快照）、配置
│  └─ bus/                 # 事件总线（SSE 实时日志）
├─ runtime/uitest-keys.js  # ★ 浏览器侧共享运行时（定位/动作唯一实现，go:embed）
├─ extension/              # MV3 插件（UI 录制 + 接口录制，与 runtime 共用同一份运行时）
├─ web/                    # 控制台：app.js 主控制台、collect.js 采集页（原生 JS，无构建）
├─ demo/                   # 8 个示例场景页 + api-center.html（接口采集演练页）
├─ cmd/scenarios/          # 一键跑全部示例场景的验证工具
├─ scripts/                # 自检 / 生成示例数据 / 下载内核 / 交叉编译
├─ docs/                   # GUIDE · COLLECT · API · screenshots
└─ data/                   # 运行时目录（不入库）：uitest.json、runs/、logs/、backups/、harvest/
```

要点：

- `data/` 是**运行时状态**，已在 `.gitignore` 里整目录忽略；删掉重启即重建（录制与采集配置也会没有）。
- `runtime/uitest-keys.js` 与 `extension/uitest-keys.js` 必须**保持一致**（插件现场用的就是它），改完请两边同步。
- 前端没有构建步骤：改 `web/*.js|css` 后重启服务端（文件是 `go:embed` 进去的）。

---

## 五、自检与构建

| 命令 | 作用 | 期望输出 |
|---|---|---|
| `scripts\smoke.ps1` | 单条流程 + 自愈 + 回写 | 自检通过 ✅ |
| `scripts\collect-smoke.ps1` | 采集全链路（录制→建任务→执行→校验行数） | 接口采集自检通过 ✅ |
| `scripts\collect-extension-smoke.ps1` | 插件上报 → 删接口 → 用剩下的建任务 | 插件录制自检通过 ✅ |
| `scripts\collect-paging-smoke.ps1` | 5 种分页形态（页码/偏移/游标/请求体/假分页） | 分页采集自检通过 ✅ |
| `scripts\collect-files-smoke.ps1` | Excel + JSON 落库、下载、upsert 不重复 | 文件落库自检通过 ✅ |
| `go run ./cmd/scenarios` | 7 个示例场景（63 步）跨版本回放 | 场景结果：7/7 通过 ✅ |
| `scripts\build-all.ps1` | 交叉编译 windows/amd64 + linux/amd64 + arm64 | `dist/` 三个可执行文件 |
| `scripts\seed-demo.ps1` | 生成示例录制（等价于 `cmd/scenarios -keep`） | 控制台「录制」列表有 8 项 |

自检脚本都用独立端口 + 独立数据目录，跑完自动清理，**不会动你的 `data/`**。

---

## 六、配置

`config.json` 与 `data/uitest.json` 里的设置会合并，环境变量优先级最高：

| 变量 | 作用 |
|---|---|
| `UITEST_PORT` | 监听端口（默认 8490） |
| `UITEST_HOST` | 监听地址（默认 127.0.0.1；容器里要外部访问填 `0.0.0.0`） |
| `UITEST_DATA_DIR` | 数据目录（默认 `data`） |
| `UITEST_BROWSER` | 浏览器可执行文件路径 |
| `UITEST_HEADLESS` | `1` = 无头 |
| `UITEST_AI_BASE_URL` / `UITEST_AI_KEY` / `UITEST_AI_MODEL` | 大模型数据生成 |

界面上的设置（AI / 浏览器 / 辅助功能 / 数据快照）见 [GUIDE 7.3 设置](docs/GUIDE.md#73-设置)。

---

## 七、文档

| 文档 | 内容 | 什么时候看 |
|---|---|---|
| **[docs/GUIDE.md](docs/GUIDE.md)** | 控制台使用手册：四个教程、界面导览、定位机制、数据与断言、浏览器、排错 FAQ | 想跑通 UI 自动化 |
| **[docs/COLLECT.md](docs/COLLECT.md)** | 接口采集手册：录制、筛选与裁剪、任务配置、分页、落库与下载、部署、FAQ | 想把接口数据定时入库 |
| **[docs/API.md](docs/API.md)** | HTTP 接口契约：通用约定、全部端点、数据模型、curl 示例 | 要写脚本 / 接第三方 |
| **[extension/README.md](extension/README.md)** | 插件手册：安装、权限、UI 录制、接口录制、稳定 ID 原理、FAQ | 改插件或排查插件问题 |

示例页面：<http://127.0.0.1:8490/demo/gallery.html>（8 个场景，覆盖表单/登录/列表/弹窗/向导/标签页/跨版本打包）、
<http://127.0.0.1:8490/demo/api-center.html>（四种分页形态 + 固定 23 条数据）。

---

## 八、已知限制

| # | 限制 | 说明 / 规避 |
|---|---|---|
| 1 | 内容脚本只在页面加载时注入 | 装/更新插件后刷新被测页面 |
| 2 | iframe 内操作有记录但执行以主框架为主 | 跨 iframe 步骤建议人工确认 |
| 3 | 富文本（Quill/ProseMirror）取值可能不准 | 用断言兜底，或改 `data_key` 手填 |
| 4 | 上传只录到文件名 | 执行时用数据准备里的值当本地路径，空则用内置示例 PNG |
| 5 | 画布/拖拽类组件只能定位容器 | 做不到像素级操作 |
| 6 | 运行记录默认保留 200 条（`storage.keep_runs`），采集运行 300 条 | 超出的自动清理 |
| 7 | 单机单进程，不搞多租户 | 可并行多个 run，但共享一个浏览器进程 |
| 8 | 内置浏览器只能访问本机能访问的网络 | 内网/离线环境打开外网必然失败 |
| 9 | 采集录制结果只存内存 | 服务重启即清空（任务与已入库数据不受影响） |
| 10 | `session.mode=browser` 需要本机能起浏览器 | 纯 Linux 服务器建议改用 `cookie` 模式 |
| 11 | 礼貌采集是**刻意限制** | 需要更快请拆成多个任务，而不是把间隔调到 0 |
