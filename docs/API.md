# UI 自动化测试平台 —— API 契约（唯一标准）

服务端：`uitest-server`（Go，默认 `http://127.0.0.1:8490`）
客户端：浏览器插件（MV3，录制端）+ Golang 服务自带网页控制台（查看端）
所有接口前缀 `/api`，请求/响应均为 `application/json; charset=utf-8`，字段名统一 `snake_case`。

通用响应：成功直接返回对象/数组；失败返回 `{"error":"中文错误信息"}` 且 HTTP 4xx/5xx。
CORS：`/api/**` 允许任意来源（插件页面来源为 `chrome-extension://...`）。

---

## 1. 数据模型

### 1.1 Locator（元素定位）

```json
{
  "strategy": "uid",                  // uid | id | name | css | xpath | text | label | placeholder
  "value": "ut_7f3a91c2",             // uid = data-uitest-id 属性值（首选，最稳定）
  "fallbacks": [                       // 备用定位，按顺序尝试
    {"strategy": "css", "value": "#product-name"},
    {"strategy": "name", "value": "name"},
    {"strategy": "text", "value": "新建商品"}
  ],
  "frame_url": ""                      // 预留：iframe 内页面地址
}
```

**定位策略全集**（按稳定性排序，见 README「重建安全」）：

| strategy | value 形态 | 说明 |
|---|---|---|
| `uid` | `ut_1a2b3c4d` | 首选，确定性算法生成的 data-uitest-id |
| `attr` | `data-testid=submit-btn` | 团队自己写的稳定属性 |
| `id` / `name` | 原值 | 仅当"看起来稳定"（hash/自动 id 会被跳过） |
| `label` | `商品名称` | 表单项标题 → 同一表单项里的控件 |
| `near` | `商品名称>>input` | 标题文字所在最内层容器里的控件 |
| `role` | `button\|保存商品` | ARIA 角色 + 可访问名 |
| `text` | `保存商品` | 按钮/链接可见文本 |
| `cell` | `标准版服务>>编辑` | 表格行文字 + 按钮文字（行内操作） |
| `placeholder` | `请输入商品名称` | 占位文字 |
| `css` | `form > div:nth-of-type(2) > input` | 纯结构路径（不含 class） |
| `xpath` | `//*[@id="x"]` | 最后兜底 |

**定位优先级**：`uid` > `attr` > `id`/`name` > `label`/`near` > `role`/`text`/`cell` > `placeholder` > `css` > `xpath`；
全部失败时执行**自愈定位**（用 `target` 指纹做相似度匹配），自愈命中会在运行日志中标记
`healed=true`，并可通过 `POST /api/runs/{id}/heal` 把新定位回写到录制。

### 1.2 Target（元素指纹，用于自愈与 AI 理解）

```json
{
  "tag": "input",
  "type": "text",
  "label": "商品名称",            // 关联 label / 就近文本 / 表单项标题
  "text": "",                     // 元素自身可见文本（按钮类）
  "placeholder": "请输入商品名称",
  "aria": "",
  "role": "",
  "href": "",
  "page": "/admin/#/products",    // 录制时所在页面（path + hash）
  "field_kind": "text",           // text|number|price|date|datetime|email|phone|url|textarea|select|checkbox|file|password|other
  "options": ["服务类", "实物类"], // 下拉框候选文本（录制时采集，供 AI 生成"一定能选中"的值）
  // ↓ 2.0 新增：重建安全定位用的语义（Vue/React 重新打包后仍然有效）
  "name": "product_name",         // 仅在 name 看起来稳定时才有值
  "stable_id": "product-name",    // 仅在 id 不是 hash/自动生成时才有值
  "test_attr": "data-testid=submit-btn",  // 团队自定义稳定属性
  "form_item": "商品名称",         // 最近表单项的标题文字（表格行内则为行首单元格文字）
  "column": "操作",                // 表格列名
  "row_text": "标准版服务"          // 行首单元格文字
}
```

### 1.3 Step（一个操作步骤）

```json
{
  "id": "stp_9f2c1b",
  "recording_id": "rec_ab12",
  "index": 1,
  "action": "click",              // navigate|click|input|select|check|uncheck|key|hover|wait|upload|assert|script
  "annotation": "点击【新建商品】按钮",   // 人可读备注，录制时自动生成，可被人工/AI 修正
  "locator": { ... },
  "target": { ... },
  "data_key": "",                 // 输入类步骤的业务字段名，如 product_name（AI 按此生成数据）
  "value": "{{product_name}}",    // 期望输入的文本；支持 {{变量}}
  "expect": "",                   // 断言：普通文本=页面需出现的文字；url:xxx=URL 需包含；js:表达式=需为真
  "wait_ms": 0,                   // 执行后额外等待毫秒
  "optional": false,              // true=失败不中断
  "note": "",                     // 补充说明（会更少误差：备注+定位一起用）
  "created_at": "2026-02-01T10:00:00+08:00"
}
```

`value` 支持的变量：`{{字段key}}`、`{{now}}`、`{{today}}`、`{{datetime}}`、`{{timestamp}}`、`{{uuid}}`、`{{random:8}}`。

### 1.4 Recording（一条录制 = 一个业务场景）

```json
{
  "id": "rec_ab12",
  "name": "新建服务商品",
  "description": "在后台商品页新建一个服务类商品并保存",
  "start_url": "http://127.0.0.1:8480/admin/#/products",
  "tags": ["商品", "回归"],
  "status": "draft",              // draft | ready
  "step_count": 6,
  "created_at": "...", "updated_at": "...",
  "steps": [ {Step}, ... ]        // 仅详情接口返回
}
```

### 1.5 Element（元素库：稳定 ID 绑定表）

```json
{
  "key": "ut_7f3a91c2",           // data-uitest-id
  "page": "/admin/#/products",
  "selector": "input[name=name]",
  "descriptor": "商品名称 输入框",
  "tag": "input", "type": "text", "label": "商品名称",
  "hits": 3, "last_seen": "...", "broken": false
}
```

### 1.6 Run（一次任务执行）

```json
{
  "id": "run_c81d",
  "recording_id": "rec_ab12",
  "recording_name": "新建服务商品",
  "status": "queued",             // queued | running | passed | failed | canceled
  "mode": "server",               // server = 服务端内置浏览器执行
  "dataset": {"product_name": "自动化测试商品-9382", "price": "199.00"},
  "dataset_source": "ai",         // ai | rules | manual
  "dataset_fields": [ {"key":"product_name","label":"商品名称","kind":"text","note":"AI：符合 2-20 字"} ],
  "total_steps": 10, "passed_steps": 10,   // 列表页摘要用
  "step_results": [
    {
      "index": 1, "step_id": "stp_9f2c1b", "action": "click",
      "annotation": "点击【新建商品】按钮",
      "status": "passed",           // passed | failed | skipped
      "value_used": "", "locator_used": "uid=ut_7f3a91c2",
      "healed": false, "message": "", "duration_ms": 132,
      "screenshot": "data/runs/run_c81d/step_01.png"
    }
  ],
  "logs": [ {"ts":"...","level":"info","index":1,"message":"点击 新建商品"} ],
  "started_at": "...", "finished_at": "...", "error": ""
}
```

---

## 2. 接口清单

### 2.1 录制（Recordings）

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| GET | `/api/recordings?q=&tag=` | – | `{"items":[Recording 摘要（无 steps）]}` |
| POST | `/api/recordings` | `{name, description?, start_url?, tags?}` | Recording |
| GET | `/api/recordings/{id}` | – | Recording（含 steps） |
| PATCH | `/api/recordings/{id}` | `{name?,description?,tags?,status?}` | Recording |
| DELETE | `/api/recordings/{id}` | – | `{"ok":true}` |
| POST | `/api/recordings/{id}/steps` | Step 字段（除 id/recording_id/index） | Step |
| PATCH | `/api/recordings/{id}/steps/{step_id}` | `{annotation?,value?,data_key?,expect?,locator?,target?,wait_ms?,optional?,note?,action?}` | Step |
| DELETE | `/api/recordings/{id}/steps/{step_id}` | – | `{"ok":true}` |
| POST | `/api/recordings/{id}/reorder` | `{"step_ids":["stp_a","stp_b"]}` | Recording |
| POST | `/api/recordings/{id}/duplicate` | `{name?}` | Recording |
| GET | `/api/recordings/{id}/export` | – | Recording（含 steps，可直接 import） |
| POST | `/api/recordings/import` | Recording JSON | Recording |

### 2.2 元素库（Elements）

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| POST | `/api/elements/bind` | `{page,key?,selector,tag?,type?,label?,descriptor?}` | Element（key 缺省时服务端生成 `ut_xxxxxxxx`） |
| GET | `/api/elements` | – | `{"items":[Element]}` |
| DELETE | `/api/elements/{key}` | – | `{"ok":true}` |

### 2.3 AI 数据准备（AI）

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| POST | `/api/ai/dataset` | `{recording_id, instruction?, count?}` | `{"fields":[Field],"dataset":{},"notes":{},"source":"ai|rules","warning":""}` |

Field：`{"key","label","kind","value","note"}`。
`source=rules` 表示未配置大模型或调用失败时的内置规则生成（保证离线可用）。

### 2.4 执行（Runs）

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| POST | `/api/runs` | `{recording_id, dataset?, dataset_source?, ai?:{enabled,instruction}, mode?:"server", headless?:bool}` | Run（`status=queued`，异步执行） |
| GET | `/api/runs?recording_id=&limit=` | – | `{"items":[Run 摘要（无 logs/step_results）]}` |
| GET | `/api/runs/{id}` | – | Run（全量） |
| POST | `/api/runs/{id}/cancel` | – | Run |
| POST | `/api/runs/{id}/heal` | – | `{"updated":2,"steps":[...]}`（把自愈定位写回录制） |
| GET | `/api/runs/{id}/events` | – | SSE：`data: {"type":"log|step|status","...":...}`，结束发 `data: {"type":"end"}` |

### 2.5 内置浏览器（Browser）

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| GET | `/api/browser/status` | – | `{running,executable,debug_url,profile_dir,version,attached,kernel_dir,launch_flags,renderer_ok,probe}` |
| GET | `/api/browser/diagnose?url=` | – | `{running,executable,version,kernel_dir,launch_flags,debug_url,attached,probe:{ok,title,ready_state,error,duration_ms},log_file,log_tail,hint}` |
| POST | `/api/browser/launch` | `{url?}` | `{ok,debug_url}` |
| POST | `/api/browser/close` | – | `{ok}` |

### 2.6 设置 / 元信息

| 方法 | 路径 | 入参 | 返回 |
|---|---|---|---|
| GET | `/api/settings` | – | `{ai:{base_url,model,has_key,key_hint,timeout_sec},browser:{executable,headless,step_delay_ms,action_timeout_ms,no_sandbox,debug_url,kernel_dir,extra_args,resolved_flags},server:{port,version}}` |
| PUT | `/api/settings` | 同上（`ai.api_key` 只写不读） | 同上 |
| GET | `/api/health` | – | `{ok:true,version,recordings,runs,browser_running,assist:{auto_data_key,stable_id_hint,auto_rebind_uid}}` |

`assist` 三项辅助开关（缺省全开）：

```json
{ "assist": { "auto_data_key": true, "stable_id_hint": true, "auto_rebind_uid": true } }
```

`browser.after_run`（运行结束后怎么处理内置浏览器，缺省 `auto`）：

| 取值 | 行为 |
|---|---|
| `auto` | 通过就关标签页；失败/取消保留现场 |
| `close_tab` | 每次运行后关闭本次标签页（浏览器继续运行） |
| `close_browser` | 每次运行后关闭整个内置浏览器 |
| `keep` | 都不关，保留现场 |

### 2.7 批量删除 / 清空

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/recordings/batch-delete` | body `{"ids":["rec_a","rec_b"]}`；**连带删除**这些录制的运行记录与截图。返回 `{deleted,recordings,runs,message}` |
| DELETE | `/api/recordings?all=1` | 清空全部录制（连带全部运行记录）。不带 `all=1` → 400 |
| POST | `/api/runs/batch-delete` | body `{"ids":[...]}`；只删运行记录与截图，录制保留 |
| DELETE | `/api/runs?all=1` | 清空全部运行记录（录制保留）。不带 `all=1` → 400 |
| POST | `/api/elements/batch-delete` | body `{"keys":["ut_1a2b3c4d"]}` |
| DELETE | `/api/elements?all=1` | 清空元素绑定。不带 `all=1` → 400 |

> 清空类接口强制要求 `all=1`，防止脚本或误操作一键清空；误删后可用数据快照恢复（见 2.8）。

### 2.8 辅助能力：一键刷新定位 / 数据快照

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/recordings/{id}/rebind` | 打开起始页，把每步定位重算成 `data-uitest-id` 并写回（**语义优先**，旧定位降级为兜底） |
| GET | `/api/backups` | 列出 `data/backups/` 里的数据快照（名称/时间/录制数/运行数/大小） |
| POST | `/api/backups/{name}/restore` | 用某份快照覆盖当前数据（覆盖前自动再存一份，可回退） |

`POST /api/recordings/{id}/rebind` 返回：

```json
{
  "total": 7, "updated": 5, "failed": 0, "key_fixed": 0, "duration_ms": 676,
  "message": "共 7 步：刷新 5 个定位，0 个失败，修正 0 个字段名",
  "steps": [
    {"index": 2, "action": "input", "from": "css=input.el-input__inner",
     "to": "uid=ut_1932b0b6", "ok": true,
     "detail": "原定位已失效，靠 near=商品名称>>input:not([type=hidden]) 命中"}
  ]
}
```

---

## 3. 插件 → 服务端 调用顺序（录制流程）

1. 弹窗里填「录制名称」→ `POST /api/recordings` → 拿到 `recording_id`（存 `chrome.storage.local`）
2. 用户开始操作页面，每个动作：`POST /api/recordings/{id}/steps`（插件同时上报 `locator`/`target`/`annotation`/`data_key`）
3. 顺手登记元素：`POST /api/elements/bind`（key = data-uitest-id）
4. 点击「停止」→ `POST /api/recordings/{id}`（`{"status":"draft"}`）或直接结束；控制台可见

## 4. 执行流程（服务端）

`POST /api/runs` → 服务端：AI/规则生成数据 → 启动/复用内置浏览器 → 逐步执行（变量替换 + 多级定位 + 自愈）→
逐步写 `step_results`/`logs`（SSE 实时推送）→ 结束置 `passed/failed`。控制台 `#/runs/{id}` 看全过程细节。

---

## 5. 补充说明

### 5.1 文件上传

`action="upload"` 的步骤：服务端先用定位器找到元素，再用 CDP `DOM.setFileInputFiles` 注入本地文件。
文件路径取步骤 `value` 里替换后的值；该路径不存在时，服务端会生成一张示例 PNG（`data/tmp/auto-test-upload.png`）用于跑通流程。

### 5.2 自愈与回写

`step_results[].healed = true` 表示该步主定位/兜底全部失效、由元素指纹相似度匹配命中，
此时 `locator_used` 形如 `uid=ut_xxx (自愈:标签一致、文本一致)`，`heal_from` 记录失效前的定位链。
调用 `POST /api/runs/{id}/heal` 会把这些新 `uid` 写回录制步骤（旧定位降级为 `fallbacks[0]`），返回 `{updated, steps:[{index,from,to}]}`。

### 5.3 浏览器两种模式

- **自己拉起**（默认）：`browser.debug_url` 为空，服务端启动 Edge/Chrome（独立 user-data-dir）；
- **连接已有**：`browser.debug_url = "http://127.0.0.1:9400"`，服务端只连接该 CDP 端点，关闭时只断开连接。

标签页命令走**页面级端点** `ws://127.0.0.1:<port>/devtools/page/<targetId>`
（新版 Chromium 的浏览器级扁平化会话不返回会话命令响应，已实测不适用）。
### 5.4 启动自愈与诊断

`POST /api/browser/launch` 会按候选启动参数组合逐组尝试，每组启动后**真实打开页面并读 `document.title`**
判定渲染进程是否可用；可用的组合写入 `settings.browser.resolved_flags` 复用。
全部失败时返回 4xx/5xx 并附带最后一组参数与错误原因。

排查"浏览器打得开但打不开网页"请用 `GET /api/browser/diagnose?url=http://127.0.0.1:8490/demo/`，
返回体里的 `probe.ok / probe.error / log_tail` 足以定位（典型原因见 README「启动自愈」）。