# API 契约

服务端 `uitest-server`，默认 <http://127.0.0.1:8490>。所有接口前缀 `/api`，
请求/响应均为 `application/json; charset=utf-8`，字段名统一 `snake_case`。

| 约定 | 说明 |
|---|---|
| 成功 | 直接返回对象或数组 |
| 失败 | `{"error":"中文错误信息"}` + HTTP 4xx/5xx |
| CORS | `/api/**` 允许任意来源（插件页面来源是 `chrome-extension://…`） |
| 时间 | RFC 3339 带时区，如 `2026-11-04T15:01:34+08:00` |
| 异步 | `POST /api/runs`、`POST /api/collect/tasks/{id}/run` 立即返回 `queued`，进度用 SSE 订阅 |
| SSE | `GET …/events` 推 `data: {"type":"log\|step\|status",…}`，结束时 `data: {"type":"end"}` |
| 清空类 | 一律要求 `?all=1`，否则 400（防脚本误删） |

```bash
curl -s http://127.0.0.1:8490/api/health
curl -s -X POST http://127.0.0.1:8490/api/recordings \
  -H "Content-Type: application/json" \
  -d '{"name":"试试","start_url":"http://127.0.0.1:8490/demo/"}'
```

## 目录

1. [数据模型](#1-数据模型)
2. [录制 / 步骤 / 元素](#2-录制--步骤--元素)
3. [执行与诊断](#3-执行与诊断)
4. [设置 / 快照 / 元信息](#4-设置--快照--元信息)
5. [接口采集](#5-接口采集)
6. [典型调用顺序](#6-典型调用顺序)
7. [补充说明](#7-补充说明)

---

## 1. 数据模型

### 1.1 Locator（定位）

```json
{
  "strategy": "uid",
  "value": "ut_7f3a91c2",
  "fallbacks": [
    {"strategy": "css", "value": "#product-name"},
    {"strategy": "text", "value": "新建商品"}
  ],
  "frame_url": ""
}
```

策略全集与稳定性排序见 [GUIDE 8.2 定位策略表](GUIDE.md#82-定位策略表)：
`uid` > `attr` > `id`/`name` > `label`/`near` > `role`/`cell`/`text` > `placeholder` > `css` > `xpath`。
全失败时走**自愈**（指纹相似度打分，唯一最高且 ≥5 分才认），自愈命中在结果里标 `healed=true`，
可用 `POST /api/runs/{id}/heal` 写回录制。

### 1.2 Target（元素指纹，供自愈与 AI 理解）

```json
{
  "tag": "input", "type": "text",
  "label": "商品名称", "text": "", "placeholder": "请输入商品名称",
  "aria": "", "role": "", "href": "",
  "page": "/demo/",                    // 录制时的 path + hash
  "field_kind": "text",                // text|number|price|date|datetime|email|phone|url|textarea|select|checkbox|file|password|other
  "options": ["服务类", "实物类"],      // 下拉候选（供 AI 生成一定选得中的值）
  "name": "product_name",              // 仅当 name 看起来稳定
  "stable_id": "product-name",         // 仅当 id 不是 hash/自动生成
  "test_attr": "data-testid=submit-btn",
  "form_item": "商品名称",              // 最近表单项标题（表格行内为行首单元格文字）
  "column": "操作", "row_text": "标准版服务"
}
```

### 1.3 Step（一个步骤）

```json
{
  "id": "stp_9f2c1b",
  "recording_id": "rec_ab12",
  "index": 1,
  "action": "click",                 // navigate|click|input|select|check|uncheck|hover|key|upload|assert|wait|script
  "annotation": "点击【新建商品】按钮",
  "locator": { }, "target": { },
  "data_key": "",                    // 输入类步骤的业务字段名，如 product_name
  "value": "{{product_name}}",       // 支持 {{字段}} {{now}} {{today}} {{datetime}} {{timestamp}} {{uuid}} {{random:8}}
  "expect": "",                      // 纯文本=页面需出现；url:xxx=URL 需含；js:表达式=需为真
  "wait_ms": 0,
  "optional": false,                 // true=失败不中断
  "note": "",
  "created_at": "2026-11-04T15:01:34+08:00"
}
```

### 1.4 Recording（一条录制 = 一个业务场景）

```json
{
  "id": "rec_ab12", "name": "新建服务商品",
  "description": "…", "start_url": "http://127.0.0.1:8490/demo/",
  "tags": ["商品"], "status": "draft",      // draft | ready
  "step_count": 6,
  "created_at": "…", "updated_at": "…",
  "steps": [ /* Step，仅详情返回 */ ]
}
```

### 1.5 Element（元素库）

```json
{
  "key": "ut_7f3a91c2", "page": "/demo/",
  "selector": "input[name=name]", "descriptor": "商品名称 输入框",
  "tag": "input", "type": "text", "label": "商品名称",
  "hits": 3, "last_seen": "…", "broken": false
}
```

### 1.6 Run（一次执行）

```json
{
  "id": "run_c81d", "recording_id": "rec_ab12", "recording_name": "新建服务商品",
  "status": "queued",                 // queued | running | passed | failed | canceled
  "mode": "server",
  "dataset": {"product_name": "自动化测试商品-9382"},
  "dataset_source": "ai",            // ai | rules | manual
  "dataset_fields": [{"key":"product_name","label":"商品名称","kind":"text","note":"…"}],
  "total_steps": 10, "passed_steps": 10,
  "step_results": [{
    "index": 1, "step_id": "stp_9f2c1b", "action": "click",
    "annotation": "点击【新建商品】按钮",
    "status": "passed",              // passed | failed | skipped
    "value_used": "", "locator_used": "uid=ut_7f3a91c2",
    "healed": false, "message": "", "duration_ms": 132,
    "screenshot": "data/runs/run_c81d/step_01.png"
  }],
  "logs": [{"ts":"…","level":"info","index":1,"message":"点击 新建商品"}],
  "started_at": "…", "finished_at": "…", "error": ""
}
```

---

## 2. 录制 / 步骤 / 元素

| 方法 | 路径 | 入参 / 说明 |
|---|---|---|
| GET | `/api/recordings?q=&tag=` | 录制列表（无 steps） |
| POST | `/api/recordings` | `{name, description?, start_url?, tags?}` |
| GET | `/api/recordings/{id}` | 详情（含 steps） |
| PATCH | `/api/recordings/{id}` | `{name?, description?, tags?, status?}` |
| DELETE | `/api/recordings/{id}` | 删除（连带运行记录与截图） |
| POST | `/api/recordings/batch-delete` | `{"ids":[…]}` → `{deleted, recordings, runs, message}` |
| DELETE | `/api/recordings?all=1` | 清空全部（连带运行记录） |
| POST | `/api/recordings/{id}/steps` | 追加一步（Step 除 id/recording_id/index）；同 `data_key` 的连续输入会被合并 |
| PATCH | `/api/recordings/{id}/steps/{sid}` | `annotation/value/data_key/expect/locator/target/wait_ms/optional/note/action` |
| DELETE | `/api/recordings/{id}/steps/{sid}` | 删除一步 |
| POST | `/api/recordings/{id}/reorder` | `{"step_ids":["stp_a","stp_b"]}` |
| POST | `/api/recordings/{id}/duplicate` | `{name?}` 复制为副本 |
| GET | `/api/recordings/{id}/export` | 导出（可直接 import） |
| POST | `/api/recordings/import` | 导入一份 Recording JSON |
| POST | `/api/recordings/{id}/rebind` | **一键刷新定位**：重算每步 uid 并写回（语义优先，旧定位降级为兜底） |
| POST | `/api/elements/bind` | `{page, key?, selector, tag?, type?, label?, descriptor?}`（key 缺省生成 `ut_xxxxxxxx`） |
| GET | `/api/elements` | 元素绑定列表 |
| DELETE | `/api/elements/{key}` · `POST /api/elements/batch-delete` · `DELETE /api/elements?all=1` | 删单个 / 批量 / 清空 |

`POST /api/recordings/{id}/rebind` 返回：

```json
{
  "total": 7, "updated": 5, "failed": 0, "key_fixed": 0, "duration_ms": 676,
  "message": "共 7 步：刷新 5 个定位，0 个失败，修正 0 个字段名",
  "steps": [{"index":2,"action":"input","from":"css=input.el-input__inner",
             "to":"uid=ut_1932b0b6","ok":true,"detail":"原定位已失效，靠 near=商品名称>>input 命中"}]
}
```

---

## 3. 执行与诊断

| 方法 | 路径 | 入参 / 说明 |
|---|---|---|
| POST | `/api/ai/dataset` | `{recording_id, instruction?, count?}` → `{fields[], dataset{}, notes{}, source:"ai\|rules", warning}` |
| POST | `/api/runs` | `{recording_id, dataset?, dataset_source?, ai?:{enabled,instruction}, mode?:"server", headless?}` → Run（`queued`） |
| GET | `/api/runs?recording_id=&limit=` | 运行列表（摘要） |
| GET | `/api/runs/{id}` | 运行详情（含 `step_results` / `logs`） |
| POST | `/api/runs/{id}/cancel` | 取消 |
| POST | `/api/runs/{id}/heal` | 把自愈定位写回录制 → `{updated, steps[]}` |
| GET | `/api/runs/{id}/events` | SSE 实时日志 |
| POST | `/api/runs/batch-delete` · `DELETE /api/runs?all=1` | 批量删 / 清空运行记录（录制保留） |
| GET | `/api/browser/status` | `{running, executable, debug_url, profile_dir, version, attached, kernel_dir, launch_flags, renderer_ok, probe}` |
| GET | `/api/browser/diagnose?url=` | `{…, probe:{ok,title,ready_state,error,duration_ms}, log_file, log_tail, hint}` |
| POST | `/api/browser/launch` | `{url?}` → `{ok, debug_url}`（按候选参数组合逐组试，真开页面验渲染进程） |
| POST | `/api/browser/close` | 关闭内置浏览器（接管模式只断开连接） |

`source=rules` 表示未配大模型或调用失败时的内置规则生成（离线可用）。

---

## 4. 设置 / 快照 / 元信息

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/settings` | `{ai:{base_url,model,has_key,key_hint,timeout_sec}, browser:{executable,headless,step_delay_ms,action_timeout_ms,no_sandbox,debug_url,kernel_dir,extra_args,resolved_flags,after_run}, assist:{auto_data_key,stable_id_hint,auto_rebind_uid}, server:{port,version}}` |
| PUT | `/api/settings` | 同上（`ai.api_key` 只写不读，留空表示不修改） |
| GET | `/api/health` | `{ok, version, recordings, runs, browser_running, assist{…}, harvest_tasks, harvest_runs}` |
| GET | `/api/backups` | 数据快照列表（名称/时间/录制数/运行数/大小） |
| POST | `/api/backups/{name}/restore` | 用快照覆盖当前数据（覆盖前自动再存一份，可回退） |

`browser.after_run`（运行结束后怎么处理浏览器，默认 `auto`）：

| 取值 | 行为 |
|---|---|
| `auto` | 通过就关本次标签页；失败/取消保留现场 |
| `close_tab` | 每次关本次标签页（浏览器继续跑） |
| `close_browser` | 每次关整个内置浏览器 |
| `keep` | 都不关 |

---

## 5. 接口采集

完整用法见 [COLLECT.md](COLLECT.md)，这里只列契约。

### 5.1 数据模型

`CapturedRequest`（一条真实请求）

| 字段 | 说明 |
|---|---|
| `id` | 请求 id（内置浏览器是 CDP requestId；插件上报由钩子生成 `h…`） |
| `index` | 录制顺序 |
| `method` / `url` / `host` / `path` / `query` | 请求行信息（后三个由服务端从 URL 拆出） |
| `status` / `type` / `kind` | 状态码 / CDP 类型（XHR·Fetch·Document）/ 归一类别（api·page·static·other） |
| `bytes` / `duration_ms` | 响应字节数 / 耗时 |
| `req_headers` / `post_data` | 请求头 / 请求体（截断；Cookie·UA·Referer 由会话统一处理，不放这里） |
| `has_body` / `body_preview` / `body_size` / `body_truncated` | 响应体预览（用于筛选与字段识别） |
| `page_url` / `at` | 触发页面（默认 Referer）/ 录制时刻 |

`HarvestTask`

| 字段 | 说明 |
|---|---|
| `name` / `description` / `start_url` | 元信息 |
| `endpoints[]` | 接口定义，见下 |
| `schedule` | `{mode: interval\|daily\|manual, every_seconds, times:["09:00"], weekdays:[1,3,5], jitter_percent}` |
| `throttle` | `{min_interval_ms, jitter_percent, timeout_ms, retries, backoff_ms, max_fail_streak, max_pages_cap}`；`max_pages_cap` 是**分页默认上限**，只在接口 `max_pages` 留空时生效 |
| `sink` | `{driver: sqlite\|mysql\|postgres\|xlsx\|json\|jsonl, dsn, table, mode: append\|upsert, key_column, batch_size}` |
| `session` | `{mode: browser\|cookie\|none, cookie, user_agent, referer, host}` |
| `status` | `active` / `paused` |
| `next_run_at` / `last_run_at` / `last_status` / `last_written` / `run_count` / `fail_streak` / `schedule_text` / `sink_file` | 服务端维护 |

`HarvestEndpoint`

| 字段 | 说明 |
|---|---|
| `name` / `method` / `url` / `enabled` | URL 支持 `{{page}} {{now}} {{date}} {{datetime}} {{timestamp}} {{uuid}} {{host}} {{env:NAME}}` |
| `headers` / `body` | 业务头与请求体（模板变量同样生效） |
| `pagination` | 分页策略（见下表），建任务时自动探测 |
| `records_path` | 记录数组的 JSON 路径（如 `data.list`），留空自动识别 |
| `fields[]` | `{column, path}`，`path` 支持 `a.b[0].c` 与 `@now @date @timestamp @url @endpoint @page` |
| `sample_status` / `sample_body` | 录制时的样本（仅用于预览、字段与分页识别） |
| `pages` / `page_param` / `page_start` / `page_step` | **旧版扁平字段**，仅在读取老数据时由 `Normalize` 迁移到 `pagination` |

`pagination`

| 字段 | 说明 |
|---|---|
| `mode` | `auto` / `page` / `offset` / `cursor` / `none` |
| `param` / `in` | 参数名（`page`/`offset`/`cursor`）与位置（`query` 或 `body`） |
| `start` / `step` | 起始值 / 递增量（offset 默认 = `page_size`） |
| `page_size` / `size_param` | 每页条数 / 每页条数参数名（探测用） |
| `max_pages` | 最多翻几页。填了就以填的为准（硬顶 2000）；留空（0）用任务的 `max_pages_cap` |
| `total_path` | 总数路径（如 `data.total`），有它即可精确算总页数 |
| `has_more_path` | 是否还有下一页（如 `data.has_more`） |
| `next_path` | 游标模式下下一页游标位置（如 `data.next_cursor`） |
| `stop_on_short` | 返回条数 < `page_size` 视为最后一页（默认 true） |

停止条件：本页 0 条 / 短页 / 总数已抓完 / `has_more=false` 或游标空·不变 / 达到 `max_pages`；
另外连续两页内容完全相同时立刻停（防接口忽略分页参数时原地打转），跨页重复行跳过并计入 `duplicates`。

`HarvestRun`

| 字段 | 说明 |
|---|---|
| `status` | `queued` / `running` / `passed` / `failed` / `canceled` |
| `trigger` | `manual` / `schedule` |
| `requests` / `fetched` / `written` | 请求次数（含翻页）/ 解析记录数 / 写入行数 |
| `endpoints[]` | `{endpoint_id, name, method, url, status, pages, fetched, written, duplicates, duration_ms, retries, bytes, error, hint, stop_reason}` |
| `logs[]` / `sink` | 日志（同 Run）/ 本次写入目标描述 |

### 5.2 端点清单

**录制会话**

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/collect/captures` | 内置浏览器方式：`{start_url, types:["XHR","Fetch"], capture_bodies, note}`；<br>插件方式：`{source:"extension", start_url, note, user_agent}`（只登记会话，不打开浏览器） |
| GET | `/api/collect/captures` | 会话列表（含实时条数、`source` = `browser`/`extension`） |
| GET | `/api/collect/captures/{id}` | 录制结果 + 筛选：`q / method / type / kind / min_status / max_status / with_body=1 / json=1 / include / exclude / host` → `{capture, items[], facets}` |
| POST | `/api/collect/captures/{id}/requests` | 插件上报：`{items:[CapturedRequest]}`（同 id 幂等，已删除的不会加回）→ `{added, total}` |
| DELETE | `/api/collect/captures/{id}/requests` | 删除接口：`{"ids":[…]}` 或 `?ids=a,b` 或 `?all=1` → `{deleted, total, message}` |
| POST | `/api/collect/captures/{id}/stop` | 停止录制 → `{count, items[]}` |
| POST | `/api/collect/captures/{id}/analyze` | 分析某条请求：`{request_id}` → `{records_path, records, fields[], columns[], rows[], pagination, pagination_text}` |
| DELETE | `/api/collect/captures/{id}` | 关闭标签页并丢弃这次录制结果 |
| POST | `/api/collect/analyze` | 直接分析一段响应体：`{body, records_path}` |

**任务**

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/collect/tasks?q=` | 任务列表 |
| POST | `/api/collect/tasks` | 新建。① 传 `endpoints[]`；② 传 `capture_id` + `request_ids[]`（推荐，自动带样本、字段映射与分页探测） |
| GET | `/api/collect/tasks/{id}` | 详情（含 `schedule_text`、`sink_file`、`endpoints[].sample_body`） |
| PATCH | `/api/collect/tasks/{id}` | 局部更新：`name/description/start_url/status/schedule/throttle/sink/session/endpoints` |
| DELETE | `/api/collect/tasks/{id}` | 删除任务（连带运行记录） |
| POST | `/api/collect/tasks/{id}/run` | 立即执行 → run |
| POST | `/api/collect/tasks/{id}/preview` | 试抓（只抓第一页、**不写库**），`{limit}` → `{items:[{status, records_path, columns, rows, pagination, total, estimated_pages, error, hint}]}` |
| POST | `/api/collect/tasks/{id}/analyze` | 用保存的样本重算字段**与分页**：`{endpoint_id, records_path?}` |
| POST | `/api/collect/tasks/{id}/detect-pagination` | 只重算分页：`{endpoint_id}` → `{pagination, pagination_text, hint}` |
| POST | `/api/collect/tasks/{id}/test-sink` | 试连落库目标（只建连接，不写数据） |

**运行 / 产出 / 选项**

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/collect/runs?task_id=&limit=` | 运行列表 |
| GET | `/api/collect/runs/{id}` | 运行详情（接口结果 + 日志） |
| POST | `/api/collect/runs/{id}/cancel` | 取消 |
| GET | `/api/collect/runs/{id}/events` | SSE 实时日志 |
| POST | `/api/collect/runs/batch-delete` · `DELETE /api/collect/runs?all=1` | 批量删 / 清空 |
| GET | `/api/collect/options` | 下拉选项与默认值（`drivers` / `session_modes` / `defaults`） |
| GET | `/api/collect/files` | `data/harvest/` 产出文件：`{items:[{name, size, modified, kind: xlsx\|json\|jsonl\|db\|other, download}], dir}` |
| GET | `/api/collect/files/{name}` | 下载产出文件（`Content-Disposition: attachment`，只允许纯文件名） |

**清空（都要求 `?all=1`，否则 400）**

| 方法 | 路径 | 说明 |
|---|---|---|
| DELETE | `/api/collect/files?all=1` | 删除 `data/harvest/` 下全部产出文件 → `{deleted, freed_bytes, failed[], message}`（被占用的文件跳过并列在 `failed`） |
| DELETE | `/api/collect/tasks?all=1` | 清空全部采集任务（连带运行记录）→ `{deleted_tasks, deleted_runs, message}` |
| DELETE | `/api/collect/runs?all=1` | 清空采集运行记录（任务保留） |
| DELETE | `/api/collect/captures?all=1` | 丢弃全部录制结果（内存会话 + 内置浏览器标签页）→ `{deleted, message}` |

### 5.3 建任务示例

```jsonc
POST /api/collect/tasks
{
  "name": "商品列表每小时同步",
  "start_url": "https://example.com/goods",
  "capture_id": "cap_1a2b3c",          // 用录制结果建：字段与分页自动带上
  "request_ids": ["5172.2"],
  "schedule": {"mode": "interval", "every_seconds": 3600, "jitter_percent": 20},
  "throttle": {"min_interval_ms": 1200, "jitter_percent": 30, "retries": 2,
               "backoff_ms": 1500, "max_fail_streak": 3, "max_pages_cap": 20},
  "sink": {"driver": "xlsx", "table": "goods", "mode": "upsert", "key_column": "id", "batch_size": 200},
  "session": {"mode": "browser", "host": "https://example.com"}
}
```

要显式指定分页时改为传 `endpoints[]`：

```jsonc
"endpoints": [{
  "name": "商品列表", "method": "GET", "enabled": true,
  "url": "https://example.com/api/goods?page=1&size=20",
  "records_path": "data.list",
  "pagination": {"mode": "page", "param": "page", "in": "query", "start": 1,
                 "page_size": 20, "max_pages": 50, "total_path": "data.total",
                 "stop_on_short": true},
  "fields": [{"column": "id", "path": "id"}, {"column": "name", "path": "name"}]
}]
```

---

## 6. 典型调用顺序

**插件录 UI**（弹窗 → 服务端）：

1. `POST /api/recordings` 拿 `recording_id`（存 `chrome.storage.local`）；
2. 每个动作 `POST /api/recordings/{id}/steps`（带 `locator` / `target` / `annotation` / `data_key`），
   同时 `POST /api/elements/bind` 登记元素；
3. 停止：`PATCH /api/recordings/{id}` `{"status":"draft"}`。

**插件录接口**：

1. `POST /api/collect/captures` `{source:"extension", start_url}` 拿 `capture_id`；
2. 页面钩子抓到的请求攒批 `POST /api/collect/captures/{id}/requests`（每 500ms 或满 20 条）；
3. 停止：`POST /api/collect/captures/{id}/stop`，然后打开 `#/collect/capture/{id}` 勾选建任务。

**服务端执行**：`POST /api/runs` → 生成数据 → 启动/复用内置浏览器 → 逐步执行（变量替换 + 多级定位 + 自愈）→
逐步写 `step_results`/`logs`（SSE 推送）→ 结束置 `passed`/`failed`。

---

## 7. 补充说明

### 7.1 文件上传

`action="upload"` 的步骤：服务端先定位元素，再用 CDP `DOM.setFileInputFiles` 注入本地文件；
路径取 `value` 替换后的值，不存在时生成示例 PNG（`data/tmp/auto-test-upload.png`）保证流程可跑通。

### 7.2 自愈与回写

`step_results[].healed=true` 表示主定位与兜底全挂、由指纹相似度命中，
`locator_used` 形如 `uid=ut_xxx (自愈:标签一致、文本一致)`，`heal_from` 记录失效前的定位链。
`POST /api/runs/{id}/heal` 把这些新 uid 写回录制（旧定位降级为 `fallbacks[0]`），返回 `{updated, steps:[{index,from,to}]}`。

### 7.3 浏览器两种模式

- **自己拉起**（默认）：`browser.debug_url` 为空，服务端启动 Edge/Chrome（独立 `user-data-dir`）；
- **连接已有**：`browser.debug_url = "http://127.0.0.1:9400"`，只连接该 CDP 端点，关闭时仅断开连接。

标签页命令走页面级端点 `ws://127.0.0.1:<port>/devtools/page/<targetId>`
（新版 Chromium 的浏览器级扁平化会话不返回会话命令响应，实测不适用）。

### 7.4 启动自愈与诊断

`POST /api/browser/launch` 按候选参数组合逐组尝试，每组**真开一个页面读 `document.title`** 判断渲染进程是否可用，
可用组合写入 `settings.browser.resolved_flags` 复用；全部失败时返回错误并附最后一组参数。
排查"浏览器打得开但网页打不开"用 `GET /api/browser/diagnose?url=…`，看 `probe.ok` / `probe.error` / `log_tail`。
