/* ==========================================================================
 * 接口采集控制台
 * --------------------------------------------------------------------------
 * 流程：录制（内置浏览器旁路记录真实请求）→ 筛选 → 建采集任务（定时/限速/落库）
 *       → 定时抓取 → 写入数据库或文件（SQLite / MySQL / PostgreSQL / Excel / JSON / JSONL）
 *
 * 说明：
 *   - 依赖 app.js 暴露的 window.UITest（api / toast / esc / 弹窗 / 格式化等），
 *     不再重复实现一套工具函数；
 *   - 风格与其余页面一致：原生 JS + 事件委托，无构建步骤。
 * ========================================================================== */

(function () {
  'use strict';

  var Opt = { loaded: false, data: null };

  /* ------------------------------------------------------------ 小工具 */

  function U() { return window.UITest || {}; }
  function api(method, path, body, opts) { return U().api(method, path, body, opts); }
  function esc(v) { return U().esc(v); }
  function toast(msg, kind) { U().toast(msg, kind); }
  function go(hash) { U().navigate(hash); }
  function onCleanup(fn) { U().addCleanup(fn); }
  function fmtDateTime(v) { return U().fmtDateTime(v); }
  function relTime(v) { return U().relTime(v); }
  function lsRead(k) { return U().lsRead ? U().lsRead(k) : null; }
  function lsWrite(k, v) { if (U().lsWrite) U().lsWrite(k, v); }

  function pageRoot(app) {
    var el = document.createElement('div');
    el.className = 'page';
    app.appendChild(el);
    return el;
  }

  function headHtml(title, sub, actions) {
    return '<div class="page-head">' +
      '<div><h1>' + esc(title) + '</h1>' +
      (sub ? '<p class="sub">' + sub + '</p>' : '') +
      '</div>' +
      (actions ? '<div class="head-actions">' + actions + '</div>' : '') +
      '</div>';
  }

  function fmtBytes(n) {
    if (!n || n < 0) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }

  function fmtMS(ms) {
    if (!ms || ms < 0) return '—';
    if (ms < 1000) return ms + ' ms';
    return (ms / 1000).toFixed(1) + ' s';
  }

  function methodChip(m) {
    var color = '#4f46e5';
    if (m === 'POST') color = '#0f7b3f';
    else if (m === 'PUT' || m === 'PATCH') color = '#b45309';
    else if (m === 'DELETE') color = '#b42318';
    return '<span class="chip" style="color:' + color + ';border-color:' + color + '33">' + esc(m) + '</span>';
  }

  function statusChip(s) {
    if (!s) return '<span class="muted">—</span>';
    var cls = s >= 200 && s < 300 ? 'badge--ok' : (s >= 400 && s < 500 ? 'badge--mid' : 'badge--fail');
    return '<span class="badge ' + cls + '">' + s + '</span>';
  }

  function taskStatusBadge(status, running) {
    if (running) return '<span class="badge badge--running"><i class="dot dot--blink"></i>执行中</span>';
    if (status === 'paused') return '<span class="badge badge--draft"><i class="dot"></i>已暂停</span>';
    if (status === 'error') return '<span class="badge badge--failed"><i class="dot"></i>异常</span>';
    return '<span class="badge badge--ok"><i class="dot"></i>启用</span>';
  }

  function runStatusBadge(status) {
    var map = {
      queued: ['排队中', 'badge--queued', ''],
      running: ['执行中', 'badge--running', '<i class="dot dot--blink"></i>'],
      passed: ['成功', 'badge--passed', '<i class="dot"></i>'],
      failed: ['失败', 'badge--failed', '<i class="dot"></i>'],
      canceled: ['已取消', 'badge--canceled', '<i class="dot"></i>']
    };
    var m = map[status] || [status || '未知', 'badge--queued', ''];
    return '<span class="badge ' + m[1] + '">' + m[2] + esc(m[0]) + '</span>';
  }

  function runStatusText(status) {
    var map = { queued: '排队中', running: '执行中', passed: '成功', failed: '失败', canceled: '已取消' };
    return map[status] || (status || '—');
  }

  // 时间显示：零值（0001-01-01）一律当"未设置"，避免列表里出现 1-01-01
  function fmtWhen(v) {
    if (!v) return '—';
    var d = new Date(v);
    if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
    return fmtDateTime(v);
  }

  // 两个时间点之间的耗时
  function spanText(from, to) {
    if (!from || !to) return '—';
    var a = new Date(from).getTime(), b = new Date(to).getTime();
    if (isNaN(a) || isNaN(b) || b < a) return '—';
    return fmtMS(b - a);
  }

  function splitList(s) {
    if (!s) return [];
    return String(s).split(/[,，\s]+/).map(function (x) { return x.trim(); }).filter(function (x) { return x; });
  }

  /* ------------------------------------------------------ 表单控件片段 */

  function fld(label, control, hint) {
    return '<div class="field"><label>' + esc(label) + '</label>' + control +
      (hint ? '<span class="field-hint">' + esc(hint) + '</span>' : '') + '</div>';
  }

  function inputHtml(id, value, attrs) {
    return '<input class="input" id="' + id + '" value="' +
      esc(value === null || value === undefined ? '' : value) + '" ' + (attrs || '') + '>';
  }

  function numHtml(id, value, attrs) {
    return inputHtml(id, value, 'type="number" ' + (attrs || ''));
  }

  function textareaHtml(id, value, rows, attrs) {
    return '<textarea class="input" id="' + id + '" rows="' + (rows || 2) + '" ' + (attrs || '') + '>' +
      esc(value === null || value === undefined ? '' : value) + '</textarea>';
  }

  /* --- 键值文本互转（请求头 "Name: Value" / 变量 "k=v" 共用一套） ---------------- */

  /** 对象 → "key<sep>value" 多行文本 */
  function kvText(obj, sep) {
    if (!obj) return '';
    return Object.keys(obj).map(function (k) { return k + sep + obj[k]; }).join('\n');
  }

  /** "key<sep>value" 多行文本 → 对象（空行和 # 注释跳过，按第一个分隔符切） */
  function parseKV(text, sep) {
    var out = {};
    String(text || '').split(/\r?\n/).forEach(function (line) {
      var s = line.trim();
      if (!s || s.charAt(0) === '#') return;
      var i = s.indexOf(sep);
      if (i <= 0) return;
      var k = s.slice(0, i).trim();
      if (k) out[k] = s.slice(i + sep.length).trim();
    });
    return out;
  }

  function varsToText(v) { return kvText(v, '='); }
  function headersToText(h) { return kvText(h, ': '); }
  function textToHeaders(text) { return parseKV(text, ':'); }

  /** 录制快照里的请求头：去掉由「会话」负责的 Cookie / UA / Referer / Host 等，别把过期登录态钉死 */
  function sessionlessHeaders(h) {
    var out = {};
    Object.keys(h || {}).forEach(function (k) {
      var lk = k.toLowerCase().trim();
      if (lk === 'cookie' || lk === 'user-agent' || lk === 'referer' || lk === 'host' ||
        lk === 'content-length' || lk === 'connection' || lk.charAt(0) === ':') return;
      if (lk.indexOf('sec-') === 0) return;
      out[k] = h[k];
    });
    return out;
  }

  /** 去掉后端预览用的截断标记「…（共 N 字节，已截断）」 */
  function stripTruncMark(s) {
    return String(s || '').replace(/…（共 \d+ 字节，已截断）\s*$/, '').trim();
  }

  function selectHtml(id, options, value, attrs) {
    var html = '<select class="input" id="' + id + '" ' + (attrs || '') + '>';
    options.forEach(function (o) {
      var v = Array.isArray(o) ? o[0] : o.value;
      var label = Array.isArray(o) ? o[1] : o.label;
      html += '<option value="' + esc(v) + '"' + (String(v) === String(value) ? ' selected' : '') + '>' + esc(label) + '</option>';
    });
    return html + '</select>';
  }

  function val(root, id) {
    var el = (root && root.querySelector) ? root.querySelector('#' + id) : null;
    if (!el) el = document.getElementById(id);
    return el ? el.value : '';
  }

  function numVal(root, id, def) {
    var n = parseInt(val(root, id), 10);
    return isNaN(n) ? def : n;
  }

  function driverOptions() {
    if (Opt.data && Opt.data.drivers) {
      return Opt.data.drivers.map(function (d) { return [d.value, d.label]; });
    }
    return [['sqlite', 'SQLite（本地数据库）'], ['xlsx', 'Excel 表格（.xlsx，可下载）'],
      ['json', 'JSON 文件（.json，可下载）'], ['mysql', 'MySQL'], ['postgres', 'PostgreSQL'],
      ['jsonl', 'JSON Lines（.jsonl）']];
  }

  // 定时 / 限速 / 落库 / 登录态 四段配置（建任务弹窗与任务详情共用）
  function configFormHtml(pfx, cfg) {
    cfg = cfg || {};
    var sch = cfg.schedule || { mode: 'interval', every_seconds: 300, jitter_percent: 20 };
    var th = cfg.throttle || {};
    var sink = cfg.sink || { driver: 'sqlite', table: 'harvest_records', mode: 'append', batch_size: 200 };
    var sess = cfg.session || { mode: 'browser' };
    var tk = sess.token || {};
    var lg = tk.login || {};
    var pl = cfg.prelogin || {};
    var cm = cfg.common || {};
    return '' +
      '<div class="ds-block"><div class="ds-block-title">定时计划</div><div class="form-grid">' +
      fld('触发方式', selectHtml(pfx + '_mode', [['interval', '固定间隔'], ['daily', '每天定时'], ['manual', '仅手动']], sch.mode)) +
      fld('间隔（秒，≥60）', numHtml(pfx + '_every', sch.every_seconds || 300, 'min="60"')) +
      fld('每日时刻', inputHtml(pfx + '_times', (sch.times || []).join(','), 'placeholder="09:00,21:30"')) +
      fld('每周几', inputHtml(pfx + '_weekdays', (sch.weekdays || []).join(','), 'placeholder="1,3,5（空=每天）"')) +
      fld('触发抖动 %', numHtml(pfx + '_sjitter', sch.jitter_percent || 0, 'min="0" max="50"')) +
      '</div></div>' +
      '<div class="ds-block"><div class="ds-block-title">礼貌限速与容错（默认很保守，最小间隔不低于 200ms）</div><div class="form-grid">' +
      fld('请求最小间隔 ms', numHtml(pfx + '_min', th.min_interval_ms || 1000, 'min="200"')) +
      fld('间隔抖动 %', numHtml(pfx + '_tjitter', th.jitter_percent || 0, 'min="0" max="90"')) +
      fld('单请求超时 ms', numHtml(pfx + '_timeout', th.timeout_ms || 20000)) +
      fld('失败重试次数', numHtml(pfx + '_retries', th.retries || 0, 'min="0" max="5"')) +
      fld('退避基数 ms', numHtml(pfx + '_backoff', th.backoff_ms || 1500)) +
      fld('连续失败熔断', numHtml(pfx + '_maxfail', th.max_fail_streak || 3, 'min="1"')) +
      fld('分页默认上限', numHtml(pfx + '_maxpages', th.max_pages_cap || 50, 'min="1" max="2000"'),
        '只在接口没填「最多页数」时生效；接口填了就以接口为准') +
      '</div></div>' +
      '<div class="ds-block"><div class="ds-block-title">写入数据库</div>' +
      '<div class="field-hint" style="margin:0 0 10px">连接串（DSN）在「数据写入」里统一配置，这里只选目标类型和表名。</div>' +
      // DSN 不再暴露给用户编辑，但要跟着任务走（引用「数据写入」配置时由它带过来）
      '<input type="hidden" id="' + pfx + '_dsn" value="' + esc(sink.dsn || '') + '">' +
      '<div class="form-grid">' +
      fld('数据库类型', selectHtml(pfx + '_driver', driverOptions(), sink.driver)) +
      fld('表名 / 文件名', inputHtml(pfx + '_table', sink.table || 'harvest_records')) +
      fld('写入模式', selectHtml(pfx + '_sinkmode', [['append', '追加'], ['upsert', '按主键覆盖']], sink.mode || 'append')) +
      fld('去重键列（upsert 用）', inputHtml(pfx + '_key', sink.key_column || '', 'placeholder="如 id"')) +
      fld('批量提交行数', numHtml(pfx + '_batch', sink.batch_size || 200)) +
      '</div></div>' +
      '<div class="ds-block" data-src-block="' + pfx + '"><div class="ds-block-title"><span>会话来源（登录态）</span>' +
      '<span class="muted">三选一，所有接口共用同一份会话</span></div>' +
      '<div class="form-grid">' +
      fld('会话怎么来', selectHtml(pfx + '_src',
        [['ui', 'UI 自动化登录（前置登录后从浏览器导出 Cookie / 请求头）'],
         ['http', '登录接口换 token（不用浏览器，顺带收 Cookie）'],
         ['import', '仅导入（只用公共参数里的 Cookie / 参数）']],
        sess.source || 'ui'),
        '改完点保存；同一个任务的接口全部用这一份会话，不再每个接口各配一套') +
      fld('站点（留空 = 起始页 / 第一个接口）', inputHtml(pfx + '_host', sess.host || '', 'placeholder="https://example.com"'),
        'UI 来源：从哪个站点导出 Cookie 与请求头；登录接口来源：留空即可') +
      fld('浏览器请求头', selectHtml(pfx + '_hmode',
        [['all', '全部共享（默认，除请求参数外都用浏览器的）'], ['auth', '只共享鉴权头'], ['off', '不共享（只用录制值）']],
        sess.header_mode || 'all'),
        '录制时的 Authorization 早就过期了：默认连 User-Agent / X-* 一起换成浏览器的实时值') +
      '</div>' +
      '<div class="field-hint" data-src-only="ui" style="margin:6px 0 0">' +
      '运行前先在浏览器里真点一遍登录（见下面的「前置登录」），Cookie / 请求头全部用浏览器里的最新值。' +
      '</div>' +
      '<div class="field-hint" data-src-only="import" style="margin:6px 0 0">' +
      '完全不碰浏览器：只用「公共参数」里粘贴导入的 Cookie / 参数。凭证过期时重新导入一次即可。' +
      '</div>' +
      '<div data-src-only="http" style="margin-top:10px">' +
      '<div class="form-grid">' +
      fld('请求方法', selectHtml(pfx + '_lgmethod', [['POST', 'POST'], ['GET', 'GET'], ['PUT', 'PUT']], (lg.method || 'POST'))) +
      fld('登录接口地址', inputHtml(pfx + '_lgurl', lg.url || '', 'placeholder="https://example.com/api/login"'),
        '登录成功返回 JSON，token 取出来后写进公共参数，再注入请求头') +
      '</div>' +
      fld('登录请求头（一行一个 Name: Value）',
        textareaHtml(pfx + '_lgheaders', headersToText(lg.headers), 3, 'placeholder="Content-Type: application/json"')) +
      fld('请求体（可用 {{username}} / {{password}} / {{env:XXX}}）',
        textareaHtml(pfx + '_lgbody', lg.body || '', 4, 'placeholder=\'{"username":"{{username}}","password":"{{password}}"}\''),
        '变量在下面的「登录变量」里填；密码建议写 {{env:SITE_PASS}}，别把明文存在任务里') +
      '<div class="form-grid">' +
      fld('token 取值路径', inputHtml(pfx + '_lgtokenpath', lg.token_path || 'data', 'placeholder="data.token"'),
        '响应 JSON 路径，如 data.token / data.access_token / token') +
      fld('token 写进哪个公共参数', inputHtml(pfx + '_lgtokenparam', (tk.param || 'token'), 'placeholder="token"'),
        '之后 URL / 请求体里可以写 {{token}}') +
      fld('注入哪个请求头', inputHtml(pfx + '_lgheader', (tk.header || 'Authorization'), 'placeholder="Authorization"')) +
      fld('前缀（填 - 表示不加）', inputHtml(pfx + '_lgprefix', (tk.prefix === undefined ? 'Bearer ' : tk.prefix), 'placeholder="Bearer "')) +
      '</div>' +
      fld('登录变量（一行一个 key=value，支持 {{env:XXX}}）',
        textareaHtml(pfx + '_lgvars', varsToText(lg.vars), 3, 'placeholder=\'username=admin\npassword={{env:SITE_PASS}}\'')) +
      '<label class="check-line"><input type="checkbox" id="' + pfx + '_lgcookies"' + (lg.with_cookies === false ? '' : ' checked') +
      '> 把登录响应的 Set-Cookie 也带进采集请求</label>' +
      '<div style="margin-top:8px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">' +
      (pfx === 'tk' ? '<button class="btn btn--sm" type="button" data-act="test-login">测试登录</button>' : '') +
      '<span class="muted" id="' + pfx + '_lgres"></span>' +
      '</div>' +
      '</div>' +
      '</div>' +
      // 公共参数：所有接口共用；登录后的浏览器头 / 脚本会覆盖同名项
      '<div class="ds-block" data-cm-block="' + pfx + '"><div class="ds-block-title"><span>公共参数</span>' +
      '<span class="muted">所有接口共用；URL / 请求体里的同名参数以接口自己为准</span></div>' +
      '<div class="cm-toolbar">' +
      '<button class="btn btn--sm" type="button" data-cm="import">粘贴导入（Cookie / Header / cURL）</button>' +
      '<button class="btn btn--sm" type="button" data-cm="suggest">从录制汇总</button>' +
      (pfx === 'tk' ? '<button class="btn btn--sm" type="button" data-cm="refresh">登录后刷新</button>' : '') +
      '<span class="muted" data-cm-msg></span>' +
      '</div>' +
      '<div class="form-grid">' +
      fld('公共查询参数 / 请求体字段（一行一个 key=value）',
        textareaHtml(pfx + '_cmparams', kvText(cm.params, '='), 4, 'placeholder=\'tenant_id=1001\ntoken={{env:API_TOKEN}}\''),
        'GET/DELETE：并进 URL 查询串；POST/PUT/PATCH：并进 JSON 请求体（接口自己已有同名参数时不覆盖）') +
      fld('公共请求头（一行一个 Name: Value）',
        textareaHtml(pfx + '_cmheaders', headersToText(cm.headers), 4, 'placeholder="X-Tenant: demo"'),
        '前置登录后浏览器里真实在用的同名头会盖掉这里的值；值里写了 {{变量}} 就反过来覆盖浏览器头（登录后的新 token 靠这个生效）') +
      fld('公共 Cookie（一行一个 name=value）',
        textareaHtml(pfx + '_cmcookies', kvText(cm.cookies, '='), 3, 'placeholder="SESSION=xxx"'),
        '只作默认值：前置登录后浏览器里的同名 Cookie 会覆盖它；没有登录流程时也能拿它兜底') +
      fld('参数绑定（一行一个：参数名 -> header:头名 / cookie:名 / query:名 / body:字段）',
        textareaHtml(pfx + '_cmbindings', bindingsToText(cm.bindings), 3,
          'placeholder=\'token -> header:Authorization = Bearer {{token}}\''),
        '优先级最高：登录后刷新到的 token 靠它覆盖请求头；等号后面是值模板，省略时就是 {{参数名}}') +
      '</div>' +
      '<div class="cm-refresh">' +
      '<label class="check-line"><input type="checkbox" id="' + pfx + '_cmrfon"' +
      (cm.refresh && cm.refresh.enabled ? ' checked' : '') +
      '> 登录后自动刷新公共参数（从浏览器请求头 / Cookie / 本地存储取最新值）</label>' +
      '<div class="field-hint" style="margin:2px 0 8px">运行顺序：前置登录 → 导出浏览器会话 → 本刷新写入公共参数 → 参数绑定生效。' +
      '典型用法：登录拿到的 token 写进参数 token，再配一条 token → header:Authorization 的绑定。</div>' +
      '<div class="form-grid">' +
      fld('token 写进哪个参数（留空默认 token）', inputHtml(pfx + '_cmrftoken', (cm.refresh && cm.refresh.token_param) || '', 'placeholder="token"')) +
      fld('从哪个请求头取 token（留空自动找）', inputHtml(pfx + '_cmrfheader', (cm.refresh && cm.refresh.token_header) || '', 'placeholder="Authorization"')) +
      fld('Cookie → 参数（一行一个 Cookie名=参数名）',
        textareaHtml(pfx + '_cmrfcookies', kvText(cm.refresh && cm.refresh.cookies, '='), 3, 'placeholder="SESSION=sid"')) +
      fld('浏览器存储 → 参数（一行一个 localStorage:key=参数名）',
        textareaHtml(pfx + '_cmrfstorage', kvText(cm.refresh && cm.refresh.storage, '='), 3, 'placeholder="localStorage:token=token"')) +
      '</div>' +
      '</div>' +
      fld('登录成功后执行的脚本（可选，返回 JSON）',
        textareaHtml(pfx + '_cmscript', cm.script || '', 4,
          'placeholder=\'return JSON.stringify({params:{token: localStorage.getItem("token_merchant")}})\''),
        '在登录后的内置浏览器页面里执行，可读 localStorage / cookie；返回 {"params":{…},"headers":{…}}，' +
        '返回 {"token":"…"} 会自动写成 Authorization: Bearer &lt;token&gt;') +
      '</div>' +
      '<div class="ds-block"><div class="ds-block-title"><span>前置登录（UI 自动化）</span>' +
      '<span class="muted">唯一的登录方式：运行前在浏览器里真点一遍，登录态直接共享给抓取</span></div>' +
      '<label class="check-line"><input type="checkbox" id="' + pfx + '_plon"' + (pl.enabled ? ' checked' : '') +
      '> 运行前先在浏览器里登录（Cookie / 请求头随登录状态变化就靠它）</label>' +
      '<div class="field-hint" style="margin:2px 0 8px">登录跑完立刻重新导出会话：' +
      'Cookie、User-Agent、Authorization 等一律用浏览器里的最新值，只有 URL / 请求体按接口配置。</div>' +
      fld('登录用变量（一行一个 key=value，支持 {{env:XXX}}）',
        textareaHtml(pfx + '_plvars', varsToText(pl.vars), 3, 'placeholder=\'password={{env:SITE_PASS}}\''),
        '录制器会把密码框脱敏成 {{password}}：这里必须填 password=真实密码，回放时才真的能登录（值支持 {{env:XXX}}）') +
      '<div class="form-grid">' +
      fld('登录流程录制', '<select class="input" id="' + pfx + '_plrec" data-rec>' + recOptions(pl.recording_id) + '</select>',
        '在「UI 自动化」里录一段登录：打开登录页 → 输账号密码 → 点登录；运行时真的在浏览器里走一遍') +
      fld('触发时机', selectHtml(pfx + '_plmode', [['always', '每次运行前都跑'], ['on-fail', '只在会话失效（401/403）时跑']], pl.mode || 'always')) +
      fld('超时（秒）', numHtml(pfx + '_pltimeout', pl.timeout_seconds || 180, 'min="30" max="3600"')) +
      '</div>' +
      '<label class="check-line"><input type="checkbox" id="' + pfx + '_plcontinue"' + (pl.continue_on_error ? ' checked' : '') +
      '> 登录失败也继续抓（默认：本次运行直接失败，不放行空数据）</label>' +
      '<div style="margin-top:10px;display:flex;gap:10px;align-items:center;flex-wrap:wrap">' +
      (pfx === 'tk' ? '<button class="btn btn--sm" data-act="run-prelogin">现在跑一次</button>' : '') +
      '<span class="muted" id="pl_result"></span>' +
      '</div>' +
      '</div>';
  }

  /* --- 录制列表（前置登录下拉用；只拉一次） --------------------------------- */

  var REC_CACHE = null;

  function recOptions(sel) {
    var opts = '<option value="">（选择登录流程录制）</option>';
    var found = false;
    (REC_CACHE || []).forEach(function (r) {
      if (r.id === sel) found = true;
      opts += '<option value="' + esc(r.id) + '"' + (r.id === sel ? ' selected' : '') + '>' +
        esc(r.name || r.id) + '（' + esc((r.steps || []).length || r.step_count || 0) + ' 步）</option>';
    });
    // 已保存的录制不在列表里（被删了/列表没拉到）也要留一个选项，否则一保存就把配置抹掉了
    if (sel && !found) {
      opts += '<option value="' + esc(sel) + '" selected>' + esc(sel) + '（找不到这个录制）</option>';
    }
    return opts;
  }

  function loadRecordings() {
    if (REC_CACHE) return Promise.resolve(REC_CACHE);
    return api('GET', '/api/recordings', undefined, { silent: true }).then(function (res) {
      REC_CACHE = U().apiItems(res);
      return REC_CACHE;
    }).catch(function () {
      REC_CACHE = REC_CACHE || [];
      return REC_CACHE;
    });
  }

  function refreshRecSelects(root) {
    var list = (root || document).querySelectorAll('select[data-rec]');
    Array.prototype.forEach.call(list, function (sel) {
      var cur = sel.value;
      sel.innerHTML = recOptions(cur);
    });
  }

  function readConfigForm(root, pfx) {
    var weekdays = splitList(val(root, pfx + '_weekdays')).map(function (x) { return parseInt(x, 10); })
      .filter(function (n) { return n >= 1 && n <= 7; });
    return {
      schedule: {
        mode: val(root, pfx + '_mode') || 'manual',
        every_seconds: numVal(root, pfx + '_every', 300),
        times: splitList(val(root, pfx + '_times')),
        weekdays: weekdays,
        jitter_percent: numVal(root, pfx + '_sjitter', 0)
      },
      throttle: {
        min_interval_ms: numVal(root, pfx + '_min', 1000),
        jitter_percent: numVal(root, pfx + '_tjitter', 0),
        timeout_ms: numVal(root, pfx + '_timeout', 20000),
        retries: numVal(root, pfx + '_retries', 0),
        backoff_ms: numVal(root, pfx + '_backoff', 1500),
        max_fail_streak: numVal(root, pfx + '_maxfail', 3),
        max_pages_cap: numVal(root, pfx + '_maxpages', 50)
      },
      sink: {
        driver: val(root, pfx + '_driver') || 'sqlite',
        dsn: val(root, pfx + '_dsn'),
        table: val(root, pfx + '_table') || 'harvest_records',
        mode: val(root, pfx + '_sinkmode') || 'append',
        key_column: val(root, pfx + '_key'),
        batch_size: numVal(root, pfx + '_batch', 200)
      },
      session: readSessionForm(root, pfx),
      prelogin: readPreloginForm(root, pfx),
      common: readCommonForm(root, pfx)
    };
  }

  /** 会话来源：ui（浏览器）/ http（登录接口）/ import（仅导入） */
  function readSessionForm(root, pfx) {
    var src = val(root, pfx + '_src') || 'ui';
    var out = {
      mode: 'browser',
      source: src,
      host: val(root, pfx + '_host').trim(),
      header_mode: val(root, pfx + '_hmode') || 'all'
    };
    if (src !== 'http') return out;
    var url = val(root, pfx + '_lgurl').trim();
    if (!url) return out;
    var cookiesEl = root.querySelector('#' + pfx + '_lgcookies');
    out.token = {
      source: 'login',
      param: val(root, pfx + '_lgtokenparam').trim() || 'token',
      header: val(root, pfx + '_lgheader').trim() || 'Authorization',
      prefix: val(root, pfx + '_lgprefix'),
      login: {
        method: val(root, pfx + '_lgmethod') || 'POST',
        url: url,
        headers: textToHeaders(val(root, pfx + '_lgheaders')),
        body: val(root, pfx + '_lgbody'),
        token_path: val(root, pfx + '_lgtokenpath').trim() || 'data',
        with_cookies: !!(cookiesEl && cookiesEl.checked),
        vars: parseKV(val(root, pfx + '_lgvars'), '=')
      }
    };
    return out;
  }

  /** 公共参数：全空 = 没配（后端按 null 清掉） */
  function readCommonForm(root, pfx) {
    var params = parseKV(val(root, pfx + '_cmparams'), '=');
    var headers = textToHeaders(val(root, pfx + '_cmheaders'));
    var cookies = parseKV(val(root, pfx + '_cmcookies'), '=');
    var bindings = parseBindings(val(root, pfx + '_cmbindings'));
    var script = val(root, pfx + '_cmscript');
    var refresh = readRefreshForm(root, pfx);
    if (!Object.keys(params).length && !Object.keys(headers).length && !Object.keys(cookies).length &&
      !bindings.length && !script && !refresh) return null;
    var out = { params: params, headers: headers, cookies: cookies, bindings: bindings, script: script };
    if (refresh) out.refresh = refresh;
    return out;
  }

  /** 参数绑定数组 → 多行文本：token -> header:Authorization = Bearer {{token}} */
  function bindingsToText(list) {
    if (!list || !list.length) return '';
    return list.map(function (b) {
      if (!b) return '';
      return (b.param || '') + ' -> ' + (b.target || 'header') + ':' + (b.name || b.param || '') +
        (b.template ? ' = ' + b.template : '');
    }).filter(function (x) { return x; }).join('\n');
  }

  /** 多行文本 → 参数绑定：参数名 -> 目标:名称 [= 模板] */
  function parseBindings(text) {
    var out = [];
    String(text || '').split(/\r?\n/).forEach(function (line) {
      var s = line.trim();
      if (!s || s.charAt(0) === '#') return;
      var i = s.indexOf('->');
      if (i <= 0) return;
      var param = s.slice(0, i).trim();
      var rest = s.slice(i + 2).trim();
      var tpl = '';
      var eq = rest.indexOf('=');
      if (eq >= 0) { tpl = rest.slice(eq + 1).trim(); rest = rest.slice(0, eq).trim(); }
      var j = rest.indexOf(':');
      var target = j >= 0 ? rest.slice(0, j).trim() : 'header';
      var name = j >= 0 ? rest.slice(j + 1).trim() : rest;
      if (!param || !name) return;
      out.push({ param: param, target: target || 'header', name: name, template: tpl });
    });
    return out;
  }

  /** 登录后刷新配置：没勾选且没填任何东西就当没配 */
  function readRefreshForm(root, pfx) {
    var onEl = root.querySelector('#' + pfx + '_cmrfon');
    var enabled = !!(onEl && onEl.checked);
    var token = val(root, pfx + '_cmrftoken').trim();
    var header = val(root, pfx + '_cmrfheader').trim();
    var cookies = parseKV(val(root, pfx + '_cmrfcookies'), '=');
    var storage = parseKV(val(root, pfx + '_cmrfstorage'), '=');
    // 没勾选且没填任何项 = 没配置（后端按 null 清掉）
    var rawToken = token;
    if (!enabled && !rawToken && !header && !Object.keys(cookies).length && !Object.keys(storage).length) return null;
    return { enabled: enabled, token_param: rawToken || 'token', token_header: header, cookies: cookies, storage: storage };
  }

  /** 接口返回的公共参数合并进表单（已有的值不覆盖，导入 / 汇总共用） */
  function mergeCommonIntoForm(root, pfx, common) {
    common = common || {};
    var params = parseKV(val(root, pfx + '_cmparams'), '=');
    var headers = textToHeaders(val(root, pfx + '_cmheaders'));
    var cookies = parseKV(val(root, pfx + '_cmcookies'), '=');
    var bindings = parseBindings(val(root, pfx + '_cmbindings'));
    var mergeMap = function (dst, src) {
      Object.keys(src || {}).forEach(function (k) {
        if (k && !(k in dst)) dst[k] = src[k];
      });
    };
    mergeMap(params, common.params);
    mergeMap(headers, common.headers);
    mergeMap(cookies, common.cookies);
    (common.bindings || []).forEach(function (b) {
      if (!b || !b.param) return;
      var key = (b.target || 'header').toLowerCase() + ':' + String(b.name || b.param).toLowerCase();
      var dup = bindings.some(function (x) {
        return ((x.target || 'header').toLowerCase() + ':' + String(x.name || '').toLowerCase()) === key;
      });
      if (!dup) bindings.push(b);
    });
    var set = function (id, v) { var el = root.querySelector('#' + id); if (el) el.value = v || ''; };
    set(pfx + '_cmparams', kvText(params, '='));
    set(pfx + '_cmheaders', headersToText(headers));
    set(pfx + '_cmcookies', kvText(cookies, '='));
    set(pfx + '_cmbindings', bindingsToText(bindings));
  }

  /** 公共参数工具条：粘贴导入 / 从录制汇总 / 登录后刷新（建任务弹窗与任务详情共用） */
  function bindCommonActions(root, pfx, ctx) {
    ctx = ctx || {};
    var block = root.querySelector('[data-cm-block="' + pfx + '"]');
    if (!block || block.getAttribute('data-cm-bound') === '1') return;
    block.setAttribute('data-cm-bound', '1');
    var msg = block.querySelector('[data-cm-msg]');
    function say(text, isErr) {
      if (!msg) { if (text) toast(text, isErr ? 'error' : 'info'); return; }
      msg.textContent = text || '';
      msg.style.color = isErr ? 'var(--red)' : '';
    }
    block.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-cm]') : null;
      if (!btn) return;
      var act = btn.getAttribute('data-cm');
      if (act === 'import') { openCommonImportModal(root, pfx, ctx, say); return; }
      if (act === 'suggest') { runCommonSuggest(root, pfx, ctx, say); return; }
      if (act === 'refresh') { runCommonRefresh(ctx, say); return; }
    });
    // 会话来源切换：只显示当前来源要填的东西
    var srcBlock = block.getAttribute('data-src-block') ? block : root.querySelector('[data-src-block="' + pfx + '"]');
    var srcSel = srcBlock ? srcBlock.querySelector('#' + pfx + '_src') : null;
    if (srcSel) {
      var syncSrc = function () {
        var v = srcSel.value || 'ui';
        Array.prototype.forEach.call(srcBlock.querySelectorAll('[data-src-only]'), function (el) {
          el.hidden = el.getAttribute('data-src-only') !== v;
        });
      };
      srcSel.addEventListener('change', syncSrc);
      syncSrc();
    }
    // 「测试登录」：只测登录接口，不保存、不落库
    var tbtn = srcBlock ? srcBlock.querySelector('[data-act="test-login"]') : null;
    if (tbtn) tbtn.addEventListener('click', function () { runTestLogin(root, pfx, ctx); });
  }

  /** 测试登录：按表单里当前的（未保存）登录配置真调一次，展示脱敏后的全过程 */
  function runTestLogin(root, pfx, ctx) {
    var out = root.querySelector('#' + pfx + '_lgres');
    if (!ctx.taskId) { toast('先保存任务，再测试登录', 'info'); return; }
    var sess = readSessionForm(root, pfx);
    var tk = sess.token || {};
    var lg = tk.login || {};
    if (!lg.url) { toast('先填登录接口地址', 'error'); return; }
    if (out) { out.textContent = '正在调登录接口…'; out.style.color = ''; }
    api('POST', '/api/collect/tasks/' + encodeURIComponent(ctx.taskId) + '/login/test',
      { login: lg, param: tk.param, header: tk.header, prefix: tk.prefix })
      .then(function (res) {
        var tr = res.trace || {};
        var bits = [];
        bits.push((res.ok ? '✔ ' : '✘ ') + (tr.method || 'POST') + ' ' + (tr.status || '—') + ' · ' + (res.elapsed_ms || 0) + ' ms');
        if (tr.token_preview) bits.push('token ' + tr.token_preview + '（' + (tr.token_length || 0) + ' 字符）');
        if ((tr.cookies || []).length) bits.push('Set-Cookie：' + tr.cookies.join('、'));
        if ((res.applied || []).length) bits.push('生效 ' + res.applied.join('、'));
        if (res.error) bits.push(res.error);
        if (out) { out.textContent = bits.join(' · '); out.style.color = res.ok ? '' : 'var(--red)'; }
        U().openModal({
          title: '测试登录结果（敏感值已脱敏）', width: 780,
          body: loginTraceHtml(tr, res),
          footer: '<button class="btn" data-modal-close>关闭</button>'
        });
      }).catch(function () {
        if (out) { out.textContent = '请求失败，看服务端日志'; out.style.color = 'var(--red)'; }
      });
  }

  /** 登录接口调用的详细过程（脱敏）：请求头 / 请求体 / 响应头 / 响应体 / token 解析 */
  function loginTraceHtml(tr, res) {
    tr = tr || {};
    var ok = res ? !!res.ok : !tr.error;
    var html = '<div class="alert alert--' + (ok ? 'info' : 'warn') + '" style="margin:0 0 10px">' +
      esc((tr.method || '') + ' ' + (tr.url || '')) + '<br>状态 ' + esc(tr.status || '—') +
      ' · ' + esc(tr.duration_ms || 0) + ' ms' + (tr.cached ? ' · 命中缓存（没真的再登一次）' : '') + '</div>';
    if (tr.headers) html += '<div class="pv-section-title">请求头（脱敏）</div>' + pvKVHtml(tr.headers);
    if (tr.body) html += '<div class="pv-section-title">请求体（脱敏）</div>' + pvPre(tr.body);
    if (tr.response_headers) html += pvDetailsHtml('响应头（脱敏）', pvKVHtml(tr.response_headers));
    if (tr.body_preview) html += '<div class="pv-section-title">响应体（前 2000 字节）</div>' + pvPre(tr.body_preview);
    var rows = [];
    rows.push(['token 取值路径', tr.token_path || '—']);
    rows.push(['token（脱敏）', (tr.token_preview || '—') + (tr.token_length ? '（' + tr.token_length + ' 字符）' : '')]);
    rows.push(['写进公共参数', tr.param || '—']);
    rows.push(['注入请求头', (tr.header || '—') + (tr.prefix ? ' ' + tr.prefix : '')]);
    rows.push(['Set-Cookie', (tr.cookies || []).join('、') || '无']);
    if (res && (res.applied || []).length) rows.push(['最终生效', res.applied.join('、')]);
    if (tr.note) rows.push(['说明', tr.note]);
    if (tr.error) rows.push(['错误', tr.error]);
    html += '<div class="pv-section-title">解析结果</div><div class="table-wrap"><table class="pv-kv"><tbody>' +
      rows.map(function (r) { return '<tr><th>' + esc(r[0]) + '</th><td>' + esc(r[1]) + '</td></tr>'; }).join('') +
      '</tbody></table></div>';
    return html;
  }

  /** 粘贴导入弹窗：Cookie / Header / 参数 / cURL 任填一项 */
  function openCommonImportModal(root, pfx, ctx, say) {
    var body = '<div class="alert alert--info" style="margin:0 0 10px">' +
      '把浏览器开发者工具里复制的内容直接粘进来：Cookie、请求头、参数或整条 cURL 任填一项即可。' +
      '解析结果会<strong>合并</strong>进公共参数（已有值不覆盖），点「保存配置」才真正生效。</div>' +
      '<div class="form-grid">' +
      fld('Cookie（整段 a=b; c=d 也行）', textareaHtml('ci_cookie', '', 3, 'placeholder="SESSION=xxx; token=yyy"')) +
      fld('请求头（一行一个 Name: Value）', textareaHtml('ci_headers', '', 4, 'placeholder="Authorization: Bearer xxx"')) +
      fld('参数（JSON / a=1&amp;b=2 / 一行一个 key=value）', textareaHtml('ci_params', '', 3, 'placeholder="tenant_id=1001"')) +
      fld('cURL（可选，整条粘进来）', textareaHtml('ci_curl', '', 4,
        'placeholder="curl \'https://api.example.com/x\' -H \'Authorization: Bearer yyy\'"')) +
      '</div>';
    var m = U().openModal({
      title: '粘贴导入会话参数', width: 760, body: body,
      footer: '<button class="btn" data-modal-close>取消</button>' +
        '<button class="btn btn--primary" id="ci_do">解析并填入</button>'
    });
    m.overlay.querySelector('#ci_do').addEventListener('click', function () {
      var payload = {
        cookie: val(m.overlay, 'ci_cookie'),
        headers: val(m.overlay, 'ci_headers'),
        params: val(m.overlay, 'ci_params'),
        curl: val(m.overlay, 'ci_curl')
      };
      if (!payload.cookie && !payload.headers && !payload.params && !payload.curl) {
        toast('先粘一点内容进来', 'error');
        return;
      }
      var url = ctx.taskId
        ? '/api/collect/tasks/' + encodeURIComponent(ctx.taskId) + '/common/import'
        : '/api/collect/common/import';
      api('POST', url, payload).then(function (res) {
        mergeCommonIntoForm(root, pfx, res.common || {});
        var notes = res.notes || [];
        m.close();
        say(notes.length ? notes.join('；') : '已导入，记得点「保存配置」', false);
        toast('已填入公共参数，别忘了保存', 'success');
      }).catch(function () { });
    });
  }

  /** 从录制结果汇总公共参数（建任务弹窗用 capture_id，任务详情用 created_from 反查） */
  function runCommonSuggest(root, pfx, ctx, say) {
    var payload = {};
    if (ctx.captureId) payload.capture_id = ctx.captureId;
    if (ctx.requestIds && ctx.requestIds.length) payload.request_ids = ctx.requestIds;
    var url = ctx.taskId
      ? '/api/collect/tasks/' + encodeURIComponent(ctx.taskId) + '/common/suggest'
      : '/api/collect/common/suggest';
    say('正在从录制结果汇总…', false);
    api('POST', url, payload).then(function (res) {
      mergeCommonIntoForm(root, pfx, res.common || {});
      var notes = (res.notes || []).slice();
      if (res.source) notes.unshift(res.source);
      say(notes.join('；') || '汇总完成，记得点「保存配置」', false);
      toast('已填入公共参数，别忘了保存', 'success');
    }).catch(function () { say('', true); });
  }

  /** 登录后刷新公共参数（只读，不保存配置） */
  function runCommonRefresh(ctx, say) {
    if (!ctx.taskId) return;
    say('正在按当前登录态刷新（会重新导出浏览器会话）…', false);
    api('POST', '/api/collect/tasks/' + encodeURIComponent(ctx.taskId) + '/common/refresh', {})
      .then(function (res) {
        if (!res.ok) { say(res.error || '刷新失败', true); return; }
        var rf = res.refresh || {};
        var lines = [];
        if (rf.source_text) lines.push('会话来源：' + rf.source_text);
        if (rf.session) lines.push(rf.session);
        if (rf.login && rf.login.ok !== false && rf.login.token_preview) {
          lines.push('登录接口 token ' + rf.login.token_preview);
        }
        if (rf.applied && rf.applied.length) lines.push('本次更新：' + rf.applied.join('、'));
        if (rf.params && Object.keys(rf.params).length) {
          lines.push('参数：' + Object.keys(rf.params).map(function (k) { return k + '=' + rf.params[k]; }).join('，'));
        }
        (rf.notes || []).forEach(function (n) { lines.push(n); });
        say(lines.join('；') || '刷新完成', false);
        U().openModal({
          title: '登录后刷新结果（敏感值已脱敏）', width: 760,
          body: refreshResultHtml(rf),
          footer: '<button class="btn" data-modal-close>关闭</button>'
        });
      }).catch(function () { say('', true); });
  }

  /** 「登录后刷新」结果弹窗：参数 / Cookie / 请求头 / 绑定都是脱敏值 */
  function refreshResultHtml(rf) {
    rf = rf || {};
    var html = '<div class="alert alert--info" style="margin:0 0 10px">' +
      (rf.source_text ? '<b>会话来源：' + esc(rf.source_text) + '</b><br>' : '') +
      esc(rf.session || '登录态摘要不可用') + '</div>';
    if (rf.login && (rf.login.url || rf.login.error)) {
      html += pvDetailsHtml('登录接口调用过程（脱敏）', loginTraceHtml(rf.login, { ok: !rf.login.error }));
    }
    if ((rf.applied || []).length) {
      html += '<div class="pv-section-title">本次更新</div><div class="chips">' +
        rf.applied.map(function (x) { return '<span class="chip">' + esc(x) + '</span>'; }).join('') + '</div>';
    }
    if (rf.params) html += '<div class="pv-section-title">公共参数（脱敏）</div>' + pvKVHtml(rf.params);
    if (rf.bindings && rf.bindings.length) {
      html += '<div class="pv-section-title">生效的参数绑定</div><div class="chips">' +
        rf.bindings.map(function (b) {
          return '<span class="chip">' + esc((b.param || '') + ' → ' + (b.target || 'header') + ':' + (b.name || '') +
            (b.template ? ' = ' + b.template : '')) + '</span>';
        }).join('') + '</div>';
    }
    if (rf.cookies) html += pvDetailsHtml('Cookie（' + Object.keys(rf.cookies).length + ' 个，已脱敏）', pvKVHtml(rf.cookies));
    if (rf.headers) html += pvDetailsHtml('浏览器登录态请求头（已脱敏）', pvKVHtml(rf.headers));
    (rf.notes || []).forEach(function (n) {
      html += '<div class="alert alert--warn" style="margin:6px 0">' + esc(n) + '</div>';
    });
    return html;
  }

  /** 前置登录（UI 自动化）：没选录制就当作没配 */
  function readPreloginForm(root, pfx) {
    var onEl = root.querySelector('#' + pfx + '_plon');
    var contEl = root.querySelector('#' + pfx + '_plcontinue');
    var recId = val(root, pfx + '_plrec').trim();
    if (!recId) return null;
    var name = '';
    if (REC_CACHE) {
      for (var i = 0; i < REC_CACHE.length; i++) {
        if (REC_CACHE[i].id === recId) { name = REC_CACHE[i].name || ''; break; }
      }
    }
    return {
      enabled: !!(onEl && onEl.checked),
      recording_id: recId,
      recording_name: name,
      mode: val(root, pfx + '_plmode') || 'always',
      timeout_seconds: parseInt(val(root, pfx + '_pltimeout'), 10) || 180,
      continue_on_error: !!(contEl && contEl.checked),
      vars: parseKV(val(root, pfx + '_plvars'), '=')
    };
  }

  // 分页配置的空值兜底（后端会再 Normalize 一次，这里只负责显示不炸）
  function normalizePag(p) {
    p = p || {};
    return {
      mode: p.mode || 'none',
      param: p.param || '',
      in: p.in || 'auto',
      start: p.start === undefined ? 0 : p.start,
      step: p.step || 0,
      page_size: p.page_size || 0,
      size_param: p.size_param || '',
      max_pages: p.max_pages || 0,
      total_path: p.total_path || '',
      has_more_path: p.has_more_path || '',
      next_path: p.next_path || '',
      stop_on_short: p.stop_on_short !== false
    };
  }

  // 接口块的展开状态（按 ep.id 记，默认只展开第一个）
  var EP_OPEN = {};

  function isEpOpen(id, index) {
    if (Object.prototype.hasOwnProperty.call(EP_OPEN, id)) return !!EP_OPEN[id];
    return index === 0;
  }

  /** 接口块标题行的一句话摘要 */
  function pagShort(p) {
    var e = normalizePag(p);
    if (e.mode === 'none') return '不分页';
    if (e.mode === 'cursor') return '游标分页';
    if (e.mode === 'offset') return '偏移分页';
    if (e.mode === 'auto') return '分页（自动）';
    return '页码分页';
  }

  /** "，最多 N 页" 片段；没填就是按任务默认上限 */
  function pageLimitText(maxPages) {
    return maxPages > 0 ? ('，最多 ' + maxPages + ' 页') : '，页数上限按任务默认值';
  }

  function describePag(p) {
    var e = normalizePag(p);
    if (e.mode === 'none') return '不分页：只抓第一页';
    if (e.mode === 'cursor') {
      return '游标分页：' + (e.param || 'cursor') + '，下一页取自 ' + (e.next_path || '（响应里同名游标）') +
        pageLimitText(e.max_pages);
    }
    if (e.mode === 'offset') {
      return '偏移分页：' + (e.param || 'offset') + '，从 ' + (e.start || 0) + ' 起每次 +' + (e.step || e.page_size || 1) +
        pageLimitText(e.max_pages);
    }
    var s = '页码分页：' + (e.param || 'page') + (e.in === 'body' ? '（请求体）' : '');
    if (e.page_size) s += '，每页 ' + e.page_size + ' 条';
    if (e.total_path) s += '，总数取自 ' + e.total_path;
    s += pageLimitText(e.max_pages);
    return s;
  }

  /* ---------------- 产出文件（Excel / JSON / JSONL / SQLite 可下载） ---------------- */

  function fileKindLabel(kind) {
    switch (kind) {
      case 'xlsx': return '<span class="chip" style="color:#0f7b3f;border-color:#0f7b3f33">Excel</span>';
      case 'json': return '<span class="chip" style="color:#4f46e5;border-color:#4f46e533">JSON</span>';
      case 'jsonl': return '<span class="chip">JSONL</span>';
      case 'db': return '<span class="chip">SQLite</span>';
      default: return '<span class="chip">文件</span>';
    }
  }

  /** 渲染"产出文件"卡片；filter 传表名前缀可只看某个任务的文件 */
  function renderFilesCard(host, filter) {
    if (!host) return;
    api('GET', '/api/collect/files', undefined, { silent: true }).then(function (res) {
      var items = U().apiItems(res);
      if (filter) {
        items = items.filter(function (f) { return f.name.indexOf(filter) === 0; });
      }
      var head = '<div class="card"><div class="card-head"><h2>产出文件（可直接下载）</h2>' +
        '<div class="card-tools"><span class="muted">' + esc(res.dir || '') + '</span>' +
        '<button class="btn btn--sm" data-files-refresh="1">刷新</button></div></div><div class="card-body">';
      if (!items.length) {
        host.innerHTML = head + '<div class="muted">还没有产出文件。落库类型选「Excel 表格」或「JSON 文件」跑一次就会生成，' +
          '也可以继续用 SQLite / MySQL / PostgreSQL。</div></div></div>';
        return;
      }
      host.innerHTML = head + '<div class="table-wrap"><table class="table"><thead><tr>' +
        '<th>文件</th><th style="width:80px">类型</th><th class="num" style="width:100px">大小</th>' +
        '<th style="width:170px">更新时间</th><th class="actions-cell" style="width:110px">操作</th></tr></thead><tbody>' +
        items.map(function (f) {
          return '<tr><td><b>' + esc(f.name) + '</b>' +
            (f.kind === 'db' ? '<div class="cell-sub">数据库文件，建议在任务里改成 Excel/JSON 再下载</div>' : '') + '</td>' +
            '<td>' + fileKindLabel(f.kind) + '</td>' +
            '<td class="num muted">' + esc(fmtBytes(f.size)) + '</td>' +
            '<td>' + esc(fmtDateTime(f.modified)) + '</td>' +
            '<td class="actions-cell"><a class="btn btn--sm" href="' + esc(f.download) + '" download>下载</a></td></tr>';
        }).join('') + '</tbody></table></div></div></div>';
    }).catch(function (err) {
      host.innerHTML = U().errorCard(err.message || '读取产出文件失败');
    });
  }

  function loadOptions() {
    if (Opt.loaded) return Promise.resolve(Opt.data);
    return api('GET', '/api/collect/options', undefined, { silent: true }).then(function (d) {
      Opt.data = d;
      Opt.loaded = true;
      return d;
    }).catch(function () { return null; });
  }

  /* ======================================================================
   * 页面 1a：#/collect/captures —— 录制接口
   * ====================================================================== */

  function renderCaptures(app) {
    var root = pageRoot(app);
    var st = { lastLive: null };

    root.innerHTML = headHtml('录制接口',
      '在内置浏览器或你自己的浏览器（插件）里正常操作页面 → 录下真实请求 → 勾选要抓的接口 → 配置采集任务。' +
      '采集默认单任务串行、带随机间隔抖动、遇到 401/403/429 立即停手，不对目标站点造成压力。',
      '<button class="btn btn--sm" id="cap-refresh">刷新</button>' +
      '<button class="btn btn--sm" data-goto="#/collect/tasks">采集任务 →</button>' +
      '<button class="btn btn--sm btn--danger" id="cap-clear">清空采集数据</button>') +
      '<div class="card">' +
      '<div class="card-head"><h2>录制接口</h2>' +
      '<div class="card-tools"><span class="muted" id="cap-summary">加载中…</span></div></div>' +
      '<div class="card-body">' +
      // 下面三块各自独立重绘：表单区只在"录制状态切换"时重建，避免把正在输入的内容刷掉
      '<div id="cap-live"></div>' +
      '<div id="cap-form">' + captureFormHtml() + '</div>' +
      '<div id="cap-sessions"></div>' +
      '</div></div>';

    loadOptions();
    refreshCaptures(root, st, true);

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('#cap-refresh')) { refreshCaptures(root, st, true); return; }
      if (t.closest('#cap-clear')) { openClearModal(root, st); return; }
      if (t.closest('#cap-start')) { startCapture(root); return; }
      if (t.closest('#cap-stop')) { stopCapture(root, t.closest('#cap-stop').getAttribute('data-id')); return; }
      if (t.closest('#cap-open')) { go('#/collect/capture/' + encodeURIComponent(t.closest('#cap-open').getAttribute('data-id'))); return; }
      if (t.closest('#cap-del')) { deleteCapture(root, t.closest('#cap-del').getAttribute('data-id')); return; }
      var link = t.closest('[data-goto]');
      if (link) { go(link.getAttribute('data-goto')); }
    });

    // 记住上次填的起始页：切页面 / 刷新浏览器之后还在
    root.addEventListener('input', function (e) {
      var t = e.target;
      if (!t || !t.id) return;
      if (t.id === 'cap_url') {
        clearTimeout(st.urlTimer);
        var v = t.value;
        st.urlTimer = setTimeout(function () { lsWrite(START_URL_KEY, v); }, 400);
      }
    });

    // 录制中每 4 秒刷一次计数（只刷新"录制中/会话列表"，不碰表单）
    var timer = setInterval(function () {
      if (!document.body.contains(root)) return;
      refreshCaptures(root, st, false);
    }, 4000);
    onCleanup(function () { clearInterval(timer); clearTimeout(st.urlTimer); });
  }

  /* ======================================================================
   * 页面 1b：#/collect/tasks —— 采集任务
   * ====================================================================== */

  function renderTasks(app) {
    var root = pageRoot(app);
    root.innerHTML = headHtml('采集任务',
      '一个任务可以包含多个接口、各自的定时与限速。写入目标可以统一，也可以按接口分表 / 分文件（在任务详情里设置）。',
      '<button class="btn btn--sm" data-goto="#/collect/captures">← 去录制接口</button>' +
      '<button class="btn btn--sm" data-goto="#/collect/runs">执行日志 →</button>') +
      '<div id="task-card">' + U().loadingCard('加载采集任务…') + '</div>';

    loadTasks(root);

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('[data-tasks-refresh]')) { loadTasks(root); return; }
      var act = t.closest('[data-task-act]');
      if (act) {
        var id = act.getAttribute('data-id');
        var what = act.getAttribute('data-task-act');
        if (what === 'open') { go('#/collect/task/' + encodeURIComponent(id)); return; }
        if (what === 'run') { runTask(id); return; }
        if (what === 'toggle') { toggleTask(id, act.getAttribute('data-status')); return; }
        if (what === 'del') { deleteTask(id, root); return; }
      }
      var link = t.closest('[data-goto]');
      if (link) { go(link.getAttribute('data-goto')); }
    });
  }

  /* ======================================================================
   * 页面 1c：#/collect/files —— 产出文件
   * ====================================================================== */

  function renderFiles(app) {
    var root = pageRoot(app);
    root.innerHTML = headHtml('产出文件',
      'Excel / JSON / JSONL 落在 <数据目录>/harvest 下，可以直接下载；SQLite / MySQL / PostgreSQL 的数据在数据库里。',
      '<button class="btn btn--sm" data-files-refresh="1">刷新</button>') +
      '<div id="files-card">' + U().loadingCard('读取产出文件…') + '</div>';

    renderFilesCard(root.querySelector('#files-card'), '');

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('[data-files-refresh]')) { renderFilesCard(root.querySelector('#files-card'), ''); }
    });
  }

  /* ======================================================================
   * 页面 1d：#/collect/sinks —— 数据写入（落库配置：新建 / 测试 / 保存 / 删除）
   * ====================================================================== */

  function driverLabel(driver) {
    var list = (Opt.data && Opt.data.drivers) || [];
    for (var i = 0; i < list.length; i++) if (list[i].value === driver) return list[i].label;
    return driver || '—';
  }

  function driverHint(driver) {
    var list = (Opt.data && Opt.data.drivers) || [];
    for (var i = 0; i < list.length; i++) if (list[i].value === driver) return list[i].dsn_hint || '';
    return '';
  }

  /** DSN 里的密码在列表里脱敏显示 */
  function maskDSN(dsn) {
    return String(dsn || '').replace(/\/\/([^:/@]+):([^@]*)@/g, '//$1:***@');
  }

  function sinkEditorHtml(p) {
    var drivers = ((Opt.data && Opt.data.drivers) || []).map(function (d) { return [d.value, d.label]; });
    if (!drivers.length) drivers = [['sqlite', 'SQLite'], ['xlsx', 'Excel'], ['json', 'JSON 文件']];
    return '<div class="form-grid">' +
      fld('配置名称 *', inputHtml('sk_name', p.name || '', 'placeholder="例如：商品表(MySQL)"')) +
      fld('落库类型', selectHtml('sk_driver', drivers, p.driver || 'sqlite')) +
      fld('表名 / 文件名', inputHtml('sk_table', p.table || 'harvest_records', 'placeholder="harvest_records"')) +
      '</div>' +
      '<div class="field"><label>DSN / 文件路径</label>' +
      '<input class="input input--mono" id="sk_dsn" value="' + esc(p.dsn || '') + '" placeholder="mysql: user:pass@tcp(127.0.0.1:3306)/db?charset=utf8mb4">' +
      '<span class="field-hint" id="sk_dsn_hint">' + esc(driverHint(p.driver || 'sqlite')) + '</span></div>' +
      '<div class="form-grid">' +
      fld('写入模式', selectHtml('sk_mode', [['append', '追加（append）'], ['upsert', '按唯一键覆盖（upsert）']], p.mode || 'append')) +
      fld('唯一键列（upsert 用）', inputHtml('sk_key', p.key_column || '', 'placeholder="例如 id"')) +
      fld('批量提交行数', numHtml('sk_batch', p.batch_size || 200, 'min="1"')) +
      '</div>' +
      '<div class="field"><label>备注</label>' +
      '<input class="input" id="sk_note" value="' + esc(p.note || '') + '" placeholder="给团队看的一句话说明"></div>';
  }

  function renderSinks(app) {
    var root = pageRoot(app);
    var state = { items: [] };

    root.innerHTML = headHtml('数据写入',
      '这里维护可复用的落库配置：不同数据库 / 不同表 / 不同 Excel·JSON 文件各建一套。任务里引用一套即可；' +
      '只想让某个接口单独写另一张表或另一个文件时，在任务详情里给那个接口填表名。',
      '<button class="btn btn--sm btn--primary" data-act="new">新建配置</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>') +
      '<div id="sink-list">' + U().loadingCard('加载落库配置…') + '</div>';

    var host = root.querySelector('#sink-list');

    function paint() {
      var items = state.items;
      if (!items.length) {
        host.innerHTML = '<div class="card"><div class="card-body">' +
          U().emptyCard('还没有落库配置',
            '点右上角「新建配置」建一套：选类型（SQLite / Excel / JSON / MySQL / PostgreSQL）、填表名或文件名，保存后就能在任务里引用。',
            '<button class="btn btn--primary" data-act="new">新建配置</button>') + '</div></div>';
        return;
      }
      host.innerHTML = '<div class="card"><div class="card-head"><h2>落库配置 <span class="count-pill">' + items.length + '</span></h2>' +
        '<div class="card-tools"><span class="muted">数据目录：' + esc((Opt.data && Opt.data.data_dir) || '') + '</span></div></div>' +
        '<div class="table-wrap"><table class="table"><thead><tr>' +
        '<th class="cell-name">名称</th><th style="width:150px">类型</th><th>写入目标</th>' +
        '<th style="width:150px">模式</th><th>备注</th><th class="actions-cell" style="width:200px">操作</th>' +
        '</tr></thead><tbody>' +
        items.map(function (p) {
          return '<tr><td><b>' + esc(p.name) + '</b>' +
            (p.dsn ? '<div class="cell-sub mono">' + esc(maskDSN(p.dsn)) + '</div>' : '') + '</td>' +
            '<td><span class="chip">' + esc(driverLabel(p.driver)) + '</span></td>' +
            '<td class="mono">' + esc(p.target || p.table || '') + '</td>' +
            '<td>' + esc(p.mode === 'upsert' ? ('覆盖（按 ' + (p.key_column || '—') + '）') : '追加') + '</td>' +
            '<td class="muted">' + esc(p.note || '—') + '</td>' +
            '<td class="actions-cell">' +
            '<button class="btn btn--sm" data-act="test" data-id="' + esc(p.id) + '">测试连接</button>' +
            '<button class="btn btn--sm" data-act="edit" data-id="' + esc(p.id) + '">编辑</button>' +
            '<button class="btn btn--sm btn--danger" data-act="del" data-id="' + esc(p.id) + '">删除</button>' +
            '</td></tr>';
        }).join('') + '</tbody></table></div></div>';
    }

    function load() {
      return api('GET', '/api/collect/sinks', undefined, { silent: true }).then(function (res) {
        state.items = U().apiItems(res);
        paint();
      }).catch(function (err) {
        host.innerHTML = U().errorCard(err.message || '读取落库配置失败');
      });
    }

    function readForm(overlay) {
      var q = function (id) { var el = overlay.querySelector('#' + id); return el ? el.value : ''; };
      return {
        name: q('sk_name').trim(),
        driver: q('sk_driver'),
        table: q('sk_table').trim(),
        dsn: q('sk_dsn').trim(),
        mode: q('sk_mode'),
        key_column: q('sk_key').trim(),
        batch_size: parseInt(q('sk_batch'), 10) || 200,
        note: q('sk_note').trim()
      };
    }

    function openEditor(item) {
      var p = item || {};
      var m = U().openModal({
        title: item ? ('编辑落库配置 · ' + (p.name || '')) : '新建落库配置',
        width: 720,
        body: sinkEditorHtml(p),
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn" id="sk_test">测试连接</button>' +
          '<button class="btn btn--primary" id="sk_save">保存</button>'
      });

      // 类型换了就换 DSN 提示
      var drv = m.overlay.querySelector('#sk_driver');
      if (drv) {
        drv.addEventListener('change', function () {
          var hint = m.overlay.querySelector('#sk_dsn_hint');
          if (hint) hint.textContent = driverHint(drv.value);
        });
      }

      m.overlay.querySelector('#sk_test').addEventListener('click', function () {
        var body = readForm(m.overlay);
        if (body.driver === '') { toast('先选择落库类型', 'error'); return; }
        api('POST', '/api/collect/sinks/test', body, { silent: true })
          .then(function (res) { toast((res && res.message) || '连接成功', 'success'); })
          .catch(function (err) { toast((err && err.message) || '连接失败', 'error', 6000); });
      });

      m.overlay.querySelector('#sk_save').addEventListener('click', function () {
        var body = readForm(m.overlay);
        if (!body.name) { toast('配置名称必填', 'error'); return; }
        var req = item
          ? api('PUT', '/api/collect/sinks/' + encodeURIComponent(item.id), body)
          : api('POST', '/api/collect/sinks', body);
        req.then(function () {
          toast('已保存', 'success');
          m.close();
          load();
        }).catch(function () { });
      });
    }

    function removeItem(p) {
      U().confirmDialog({
        title: '删除落库配置',
        message: '删除「' + (p.name || '') + '」后，已经引用它的任务不受影响（配置值已复制进任务），之后不能再选它。'
      }).then(function (ok) {
        if (!ok) return;
        api('DELETE', '/api/collect/sinks/' + encodeURIComponent(p.id)).then(function () {
          toast('已删除', 'success');
          load();
        }).catch(function () { });
      });
    }

    function testItem(p) {
      api('POST', '/api/collect/sinks/test', { id: p.id }, { silent: true })
        .then(function (res) { toast((res && res.message) || '连接成功', 'success'); })
        .catch(function (err) { toast((err && err.message) || '连接失败', 'error', 6000); });
    }

    function find(id) {
      for (var i = 0; i < state.items.length; i++) if (state.items[i].id === id) return state.items[i];
      return null;
    }

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      var el = t.closest('[data-act]');
      if (el) {
        var act = el.getAttribute('data-act');
        var id = el.getAttribute('data-id');
        if (act === 'new') { openEditor(null); return; }
        if (act === 'refresh') { load(); return; }
        if (act === 'edit') { var p = find(id); if (p) openEditor(p); return; }
        if (act === 'del') { var p2 = find(id); if (p2) removeItem(p2); return; }
        if (act === 'test') { var p3 = find(id); if (p3) testItem(p3); return; }
      }
      var link = t.closest('[data-goto]');
      if (link) { go(link.getAttribute('data-goto')); }
    });

    loadOptions();
    load();
  }

  /* ======================================================================
   * 页面 1e：#/collect/runs —— 执行日志（请求 / 响应 / 写入情况）
   * ====================================================================== */

  function renderRuns(app) {
    var root = pageRoot(app);
    var state = { items: [], tasks: [], taskID: '' };

    root.innerHTML = headHtml('执行日志',
      '每次采集的请求数、解析条数、写入行数；点「查看」进详情，运行日志里按顺序记着每一次请求与响应（明文，含 Cookie / 请求头 / 请求体 / 响应体）。',
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--danger" data-act="clear">清空日志</button>') +
      '<div class="card">' +
      '<div class="card-toolbar">' +
      '<div class="field" style="margin:0;min-width:220px"><label>按任务筛选</label>' +
      '<select class="input" id="run-filter"><option value="">全部任务</option></select></div>' +
      '<div class="toolbar-right"><span class="muted" id="run-count"></span></div>' +
      '</div>' +
      '<div id="run-table">' + U().loadingCard('加载执行日志…') + '</div>' +
      '</div>';

    var host = root.querySelector('#run-table');
    var filterEl = root.querySelector('#run-filter');
    var countEl = root.querySelector('#run-count');

    function paint() {
      var items = state.items;
      if (countEl) countEl.textContent = items.length ? ('共 ' + items.length + ' 条') : '';
      if (!items.length) {
        host.innerHTML = U().emptyCard('还没有执行记录',
          '去「采集任务」里点「立即运行」，或等定时触发；跑一次这里就会有一条记录。',
          '<button class="btn btn--primary" data-goto="#/collect/tasks">去采集任务</button>');
        return;
      }
      host.innerHTML = '<div class="table-wrap"><table class="table"><thead><tr>' +
        '<th style="width:150px">开始时间</th><th class="cell-name">任务</th><th style="width:74px">触发</th>' +
        '<th style="width:96px">结果</th><th class="num" style="width:70px">请求</th>' +
        '<th class="num" style="width:70px">解析</th><th class="num" style="width:70px">写入</th>' +
        '<th style="width:90px">耗时</th><th>写入目标</th><th class="actions-cell" style="width:90px">操作</th>' +
        '</tr></thead><tbody>' +
        items.map(function (r) {
          var cost = r.finished_at ? spanText(r.started_at, r.finished_at) : '进行中';
          return '<tr><td class="nowrap">' + esc(fmtDateTime(r.started_at)) + '</td>' +
            '<td class="cell-name">' + esc(r.task_name || r.task_id) +
            '<div class="cell-sub mono">' + esc(r.id) + '</div></td>' +
            '<td>' + esc(r.trigger === 'schedule' ? '定时' : '手动') + '</td>' +
            '<td>' + runStatusBadge(r.status) + '</td>' +
            '<td class="num">' + esc(r.requests || 0) + '</td>' +
            '<td class="num">' + esc(r.fetched || 0) + '</td>' +
            '<td class="num">' + esc(r.written || 0) + '</td>' +
            '<td class="nowrap">' + esc(cost) + '</td>' +
            '<td class="muted">' + esc(r.sink || '—') + '</td>' +
            '<td class="actions-cell"><button class="btn btn--sm" data-goto="#/collect/run/' + esc(r.id) + '">查看</button></td>' +
            '</tr>';
        }).join('') + '</tbody></table></div>';
    }

    function load() {
      var q = '/api/collect/runs?limit=100' + (state.taskID ? '&task_id=' + encodeURIComponent(state.taskID) : '');
      return api('GET', q, undefined, { silent: true }).then(function (res) {
        state.items = U().apiItems(res);
        paint();
      }).catch(function (err) {
        host.innerHTML = U().errorCard(err.message || '读取执行日志失败');
      });
    }

    function loadTasks() {
      return api('GET', '/api/collect/tasks', undefined, { silent: true }).then(function (res) {
        state.tasks = U().apiItems(res);
        if (!filterEl) return;
        filterEl.innerHTML = '<option value="">全部任务</option>' + state.tasks.map(function (t) {
          return '<option value="' + esc(t.id) + '">' + esc(t.name) + '</option>';
        }).join('');
        filterEl.value = state.taskID;
      }).catch(function () { });
    }

    if (filterEl) {
      filterEl.addEventListener('change', function () {
        state.taskID = filterEl.value;
        load();
      });
    }

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('[data-act="refresh"]')) { load(); return; }
      if (t.closest('[data-act="clear"]')) {
        U().confirmDialog({ title: '清空执行日志', message: '只删除历史运行记录，任务配置与已经写入的数据不受影响。确定清空？' })
          .then(function (ok) {
            if (!ok) return;
            api('DELETE', '/api/collect/runs?all=1').then(function (res) {
              toast((res && res.message) || '已清空', 'success');
              load();
            }).catch(function () { });
          });
        return;
      }
      var link = t.closest('[data-goto]');
      if (link) { go(link.getAttribute('data-goto')); }
    });

    loadTasks();
    load();
  }

  var START_URL_KEY = 'uitest.collect.startUrl';
  var CAP_SCOPE_KEY = 'uitest.collect.scope';
  var CAP_BODIES_KEY = 'uitest.collect.bodies';

  /** 录制范围偏好（记住上次的选择，避免每次都被重置回"只要接口"） */
  function capScopePref() {
    try { return lsRead(CAP_SCOPE_KEY) === 'all' ? 'all' : 'xhr'; } catch (e) { return 'xhr'; }
  }

  /** 响应体偏好 */
  function capBodiesPref() {
    try { return lsRead(CAP_BODIES_KEY) === '0' ? '0' : '1'; } catch (e) { return '1'; }
  }

  function captureFormHtml() {
    return '<div class="form-grid">' +
      fld('起始页地址', inputHtml('cap_url', startUrlPref(), 'placeholder="https://你的系统/列表页"')) +
      fld('录制范围', selectHtml('cap_types', [['xhr', 'XHR + Fetch（只要接口）'], ['all', '含页面文档（XHR + Fetch + Document）']], capScopePref())) +
      fld('响应体', selectHtml('cap_bodies', [['1', '抓（推荐，用于字段识别）'], ['0', '不抓（只看请求）']], capBodiesPref())) +
      '</div>' +
      '<div class="ds-actions"><button class="btn btn--primary" id="cap-start">开始录制</button>' +
      '<span class="field-hint" style="margin-left:10px">会打开一个内置浏览器标签页，你在里面正常登录 / 点列表 / 翻页即可；' +
      '「含页面文档」会额外录下页面本身（HTML）的请求，适合强缓存看不到接口的情况。</span></div>';
  }

  /** 只刷新"录制中横幅 + 会话列表 + 汇总"，不重建表单（否则会把正在输入的起始页刷掉） */
  function refreshCaptures(root, st, force) {
    api('GET', '/api/collect/captures', undefined, { silent: true }).then(function (res) {
      var items = U().apiItems(res);
      var host = root.querySelector('#cap-sessions');
      if (!host) return;
      var live = items.filter(function (c) { return !c.stopped; });
      var liveKey = live.map(function (c) { return c.id + ':' + (c.count || 0); }).join(',');

      var summary = root.querySelector('#cap-summary');
      if (summary) {
        summary.textContent = live.length
          ? ('录制中 ' + live.length + ' 个会话，共 ' + live.reduce(function (n, c) { return n + (c.count || 0); }, 0) + ' 条请求')
          : (items.length ? items.length + ' 个录制会话（已停止）' : '暂无录制');
      }

      // 录制中横幅：只在内容变化时重绘
      var liveHost = root.querySelector('#cap-live');
      if (liveHost && (force || st.lastLive !== liveKey)) {
        st.lastLive = liveKey;
        liveHost.innerHTML = live.map(function (c) {
          return '<div class="cap-banner"><i class="dot dot--blink"></i>' +
            '<span>正在录制 <b>' + esc(c.page_url || '(空白页)') + '</b>，已录 ' + esc(c.count || 0) + ' 条' +
            (c.source === 'extension' ? '（浏览器插件）' : '（内置浏览器）') + '</span>' +
            '<span class="muted">开始于 ' + esc(relTime(c.started_at)) + '</span>' +
            '<span style="margin-left:auto"></span>' +
            '<button class="btn btn--sm" id="cap-open" data-id="' + esc(c.id) + '">筛选结果</button>' +
            '<button class="btn btn--sm btn--primary" id="cap-stop" data-id="' + esc(c.id) + '">停止录制</button></div>';
        }).join('') +
          (live.length ? '<div class="field-hint" style="margin:-6px 0 10px">插件已经关掉、这里还显示「录制中」？点那一行的「丢弃」清掉它即可。</div>' : '');
        // 有录制中的会话时把表单收起来，停掉再放回来（表单本身不重建，输入不丢）
        var form = root.querySelector('#cap-form');
        if (form) form.classList.toggle('hidden', live.length > 0);
      }

      if (!items.length) { host.innerHTML = ''; return; }

      // 明确告诉用户"下一步在哪"：录完 → 点这一行进去勾选接口 → 配任务
      var pending = items.filter(function (c) { return c.stopped && (c.count || 0) > 0; });
      var callout = '';
      if (pending.length) {
        var totalReq = pending.reduce(function (n, c) { return n + (c.count || 0); }, 0);
        callout = '<div class="alert alert--info" style="margin:0 0 10px">' +
          '已经录到 <b>' + totalReq + '</b> 条接口请求。点下面表格里的 <b>「勾选接口 → 建任务」</b>，' +
          '在弹出的页面里勾选要抓的接口（可先筛选 / 删除噪声），再点底部的「下一步：配置采集任务」。' +
          (firstCaptureLink(pending[0].id)) + '</div>';
      }
      host.innerHTML = callout +
        '<div class="table-wrap" style="margin-top:14px"><table class="table"><thead><tr>' +
        '<th>页面</th><th>来源</th><th>开始时间</th><th class="num">条数</th><th>状态</th><th class="actions-cell">操作</th>' +
        '</tr></thead><tbody>' +
        items.map(function (c) {
          return '<tr><td><div class="url-cell">' + esc(c.page_url || '(空白页)') + '</div></td>' +
            '<td>' + (c.source === 'extension' ? '<span class="chip">浏览器插件</span>' : '<span class="chip">内置浏览器</span>') +
            (c.types && c.types.length ? '<div class="cell-sub">' + esc(c.types.join('+')) + '</div>' : '') + '</td>' +
            '<td>' + esc(fmtDateTime(c.started_at)) + '</td>' +
            '<td class="num">' + esc(c.count || 0) + '</td>' +
            '<td>' + (c.stopped ? '<span class="badge badge--draft"><i class="dot"></i>已停止</span>'
              : '<span class="badge badge--running"><i class="dot dot--blink"></i>录制中</span>') + '</td>' +
            '<td class="actions-cell"><button class="btn btn--sm btn--primary" id="cap-open" data-id="' + esc(c.id) + '">勾选接口 → 建任务</button>' +
            '<button class="btn btn--sm btn--danger" id="cap-del" data-id="' + esc(c.id) + '">丢弃</button></td></tr>';
        }).join('') +
        '</tbody></table></div>';
    }).catch(function (err) {
      var host = root.querySelector('#cap-sessions');
      if (host) host.innerHTML = U().errorCard(err.message || '读取录制状态失败');
    });
  }

  function startUrlPref() {
    var saved = '';
    try { saved = lsRead(START_URL_KEY) || ''; } catch (e) { saved = ''; }
    return saved || defaultStartURL();
  }

  /* ---------------- 清空数据（任务 / 运行 / 产出文件 / 录制结果） ---------------- */

  /**
   * 清空采集数据：分四类，勾选后一起删。
   * 服务端每个口子都要求 ?all=1，这里前端先弹一次确认框，避免误点。
   */
  function openClearModal(root, st) {
    Promise.all([
      api('GET', '/api/collect/tasks', undefined, { silent: true }).catch(function () { return { items: [] }; }),
      api('GET', '/api/collect/files', undefined, { silent: true }).catch(function () { return { items: [] }; }),
      api('GET', '/api/collect/captures', undefined, { silent: true }).catch(function () { return { items: [] }; })
    ]).then(function (res) {
      var taskCount = U().apiItems(res[0]).length;
      var files = U().apiItems(res[1]);
      var capCount = U().apiItems(res[2]).length;
      var fileSize = files.reduce(function (n, f) { return n + (f.size || 0); }, 0);
      var runHint = taskCount ? '（含这些任务的运行记录）' : '';

      var body = '<div class="alert alert--warn" style="margin-bottom:12px">' +
        '清空后不可恢复（数据快照只覆盖录制/设置，不覆盖采集任务）。想留证据的，先把产出文件下载下来。</div>' +
        '<label class="check-line"><input type="checkbox" id="clr_files"> <b>产出文件</b>' +
        '<span class="muted">　' + files.length + ' 个文件，' + fmtBytes(fileSize) + '（Excel / JSON / JSONL / SQLite 表格）</span></label>' +
        '<label class="check-line"><input type="checkbox" id="clr_tasks"> <b>采集任务与运行记录</b>' +
        '<span class="muted">　' + taskCount + ' 个任务' + runHint + '</span></label>' +
        '<label class="check-line"><input type="checkbox" id="clr_runs"> <b>仅运行记录</b>' +
        '<span class="muted">　保留任务配置，只删历史运行日志</span></label>' +
        '<label class="check-line"><input type="checkbox" id="clr_caps"> <b>录制结果</b>' +
        '<span class="muted">　' + capCount + ' 个会话（内存里的录制结果，已建任务不受影响）</span></label>';

      var m = U().openModal({
        title: '清空采集数据', width: 620, body: body,
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn btn--danger" id="clr_go">清空选中项</button>'
      });

      m.overlay.querySelector('#clr_go').addEventListener('click', function () {
        var pick = function (id) {
          var el = m.overlay.querySelector('#' + id);
          return !!(el && el.checked);
        };
        var want = [];
        if (pick('clr_files')) want.push('产出文件（' + files.length + ' 个）');
        if (pick('clr_tasks')) want.push('采集任务与运行记录（' + taskCount + ' 个任务）');
        if (pick('clr_runs')) want.push('全部采集运行记录');
        if (pick('clr_caps')) want.push('录制结果（' + capCount + ' 个会话）');
        if (!want.length) { toast('先勾选要清空的内容', 'error'); return; }

        U().confirmDialog({
          title: '确认清空',
          message: '将清空：' + want.join('、') + '。这个操作不可撤销，确定继续？',
          confirmText: '确认清空'
        }).then(function (ok) {
          if (!ok) return;
          var jobs = [];
          if (pick('clr_files')) jobs.push(api('DELETE', '/api/collect/files?all=1', undefined, { silent: true }));
          if (pick('clr_tasks')) jobs.push(api('DELETE', '/api/collect/tasks?all=1', undefined, { silent: true }));
          if (pick('clr_runs')) jobs.push(api('DELETE', '/api/collect/runs?all=1', undefined, { silent: true }));
          if (pick('clr_caps')) jobs.push(api('DELETE', '/api/collect/captures?all=1', undefined, { silent: true }));
          Promise.all(jobs).then(function (outs) {
            var msgs = outs.map(function (o) { return (o && o.message) || ''; }).filter(Boolean);
            toast(msgs.length ? msgs.join('；') : '已清空', 'success');
            m.close();
            refreshCaptures(root, st, true);
            loadTasks(root);
            renderFilesCard(root.querySelector('#files-card'), '');
          }).catch(function (err) {
            toast((err && err.message) || '清空失败', 'error');
          });
        });
      });
    }).catch(function (err) {
      toast((err && err.message) || '读取清空范围失败', 'error');
    });
  }

  function defaultStartURL() {
    var base = location.origin || 'http://127.0.0.1:8490';
    return base + '/demo/api-center.html';
  }

  /** 提示里的"直接打开"链接（避免用户还要去找那一行） */
  function firstCaptureLink(id) {
    if (!id) return '';
    return ' <a href="#/collect/capture/' + encodeURIComponent(id) + '">直接打开这次录制结果 ↗</a>';
  }

  function startCapture(root) {
    var url = val(root, 'cap_url').trim();
    if (!url) { toast('请填写起始页地址', 'error'); return; }
    lsWrite(START_URL_KEY, url);
    var scope = val(root, 'cap_types');
    var bodies = val(root, 'cap_bodies') === '1';
    var types = scope === 'all' ? ['XHR', 'Fetch', 'Document'] : ['XHR', 'Fetch'];
    lsWrite(CAP_SCOPE_KEY, scope === 'all' ? 'all' : 'xhr');
    lsWrite(CAP_BODIES_KEY, bodies ? '1' : '0');
    api('POST', '/api/collect/captures', {
      start_url: url, types: types, capture_bodies: bodies
    }).then(function (res) {
      toast('已开始录制（范围：' + types.join(' + ') + '），请在内置浏览器里正常操作', 'success');
      if (res && res.capture && res.capture.id) {
        go('#/collect/capture/' + encodeURIComponent(res.capture.id));
      }
    }).catch(function () { /* api 已提示 */ });
  }

  function stopCapture(root, id) {
    if (!id) return;
    api('POST', '/api/collect/captures/' + encodeURIComponent(id) + '/stop', {}).then(function (res) {
      toast('已停止录制，共录到 ' + (res.count || 0) + ' 条请求', 'success');
      go('#/collect/capture/' + encodeURIComponent(id));
    }).catch(function () { });
  }

  function deleteCapture(root, id) {
    U().confirmDialog({ title: '丢弃这次录制结果', message: '丢弃后这次录到的请求列表不可恢复（已建好的采集任务与已入库的数据不受影响）。' })
      .then(function (ok) {
        if (!ok) return;
        api('DELETE', '/api/collect/captures/' + encodeURIComponent(id)).then(function () {
          toast('已丢弃这次录制', 'success');
          if (root && root.querySelector) refreshCaptures(root, { lastLive: null }, true);
          else go('#/collect/captures');
        }).catch(function () { });
      });
  }

  function loadTasks(root) {
    var host = root.querySelector('#task-card');
    if (!host) return;
    api('GET', '/api/collect/tasks', undefined, { silent: true }).then(function (res) {
      var items = U().apiItems(res);
      if (!items.length) {
        host.innerHTML = '<div class="card"><div class="card-head"><h2>采集任务</h2></div><div class="card-body">' +
          U().emptyCard('还没有采集任务', '先在上面录一段接口，然后在结果里勾选要抓的接口，就能生成一个定时采集任务。') +
          '</div></div>';
        return;
      }
      host.innerHTML = '<div class="card"><div class="card-head"><h2>采集任务 <span class="count-pill">' + items.length + '</span></h2>' +
        '<div class="card-tools"><button class="btn btn--sm" data-tasks-refresh="1">刷新</button></div></div><div class="card-body">' +
        '<div class="table-wrap"><table class="table"><thead><tr>' +
        '<th class="cell-name">任务</th><th>接口</th><th>计划</th><th>下次触发</th><th>最近结果</th>' +
        '<th class="num">写入行数</th><th class="actions-cell">操作</th></tr></thead><tbody>' +
        items.map(function (t) {
          var next = fmtWhen(t.next_run_at);
          var last = t.last_status
            ? runStatusBadge(t.last_status) + '<div class="cell-sub">' + esc(relTime(t.last_run_at)) + '</div>'
            : '<span class="muted">还没跑过</span>';
          return '<tr>' +
            '<td><div><b>' + esc(t.name) + '</b> ' + taskStatusBadge(t.status, t.running) + '</div>' +
            '<div class="cell-sub mono">' + esc(t.sink.driver) + ' · ' + esc(t.sink.table) + '</div></td>' +
            '<td>' + esc(t.endpoint_count) + '</td>' +
            '<td>' + esc(t.schedule_text || '—') + '</td>' +
            '<td>' + esc(next) + '</td>' +
            '<td>' + last + '</td>' +
            '<td class="num">' + esc(t.last_written || 0) + '</td>' +
            '<td class="actions-cell">' +
            '<button class="btn btn--sm" data-task-act="open" data-id="' + esc(t.id) + '">详情</button>' +
            '<button class="btn btn--sm btn--primary" data-task-act="run" data-id="' + esc(t.id) + '">立即运行</button>' +
            '<button class="btn btn--sm" data-task-act="toggle" data-status="' + esc(t.status) + '" data-id="' + esc(t.id) + '">' +
            (t.status === 'active' ? '暂停' : '启用') + '</button>' +
            '<button class="btn btn--sm btn--danger" data-task-act="del" data-id="' + esc(t.id) + '">删除</button>' +
            '</td></tr>';
        }).join('') + '</tbody></table></div></div></div>';
    }).catch(function (err) {
      host.innerHTML = U().errorCard(err.message || '读取任务失败');
    });
  }

  function runTask(id) {
    api('POST', '/api/collect/tasks/' + encodeURIComponent(id) + '/run', {}).then(function (run) {
      toast('已开始采集', 'success');
      go('#/collect/run/' + encodeURIComponent(run.id));
    }).catch(function () { });
  }

  function toggleTask(id, status) {
    var next = status === 'active' ? 'paused' : 'active';
    api('PATCH', '/api/collect/tasks/' + encodeURIComponent(id), { status: next }).then(function () {
      toast(next === 'active' ? '任务已启用' : '任务已暂停', 'success');
      if (location.hash.indexOf('/collect/task/') >= 0) {
        location.reload();
      } else {
        var app = document.getElementById('app');
        if (app) { app.innerHTML = ''; renderTasks(app); }
      }
    }).catch(function () { });
  }

  function deleteTask(id, root) {
    U().confirmDialog({ title: '删除采集任务', message: '删除后这个任务的定时计划与运行记录都会一起清掉（数据库里已写入的数据不受影响）。' })
      .then(function (ok) {
        if (!ok) return;
        api('DELETE', '/api/collect/tasks/' + encodeURIComponent(id)).then(function () {
          toast('已删除', 'success');
          if (root) loadTasks(root);
        }).catch(function () { });
      });
  }

  /* ======================================================================
   * 页面 2：#/collect/capture/{id} —— 录制结果 + 接口筛选
   * ====================================================================== */

  function renderCapture(app, id) {
    var root = pageRoot(app);
    var state = {
      id: id,
      data: null,
      filter: {
        q: '', method: '', type: '',
        include: '', exclude: '',
        with_body: false, json: false,
        min_status: 0, max_status: 0
      },
      sel: {},
      timer: null
    };

    root.innerHTML = headHtml('录制结果', '勾选要定时抓取的接口；可以先按方法 / 状态 / 关键字 / 正则筛一遍。',
      '<button class="btn btn--sm" data-act="back">返回列表</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--ghost" data-act="stop">停止录制</button>' +
      '<button class="btn btn--sm btn--danger" data-act="del">删除这次录制</button>') +
      '<div class="card"><div class="card-body">' +
      '<div class="filter-bar">' +
      fld('关键字', inputHtml('f_q', '', 'placeholder="路径 / 主机"')) +
      fld('方法', selectHtml('f_method', [['', '全部'], ['GET', 'GET'], ['POST', 'POST'], ['PUT', 'PUT'], ['PATCH', 'PATCH'], ['DELETE', 'DELETE']], '')) +
      fld('类型', selectHtml('f_type', [['', '全部'], ['XHR', 'XHR'], ['Fetch', 'Fetch'], ['Document', '文档']], '')) +
      fld('状态', selectHtml('f_status', [['', '全部'], ['2xx', '2xx 成功'], ['4xx', '4xx'], ['5xx', '5xx']], '')) +
      fld('只看', selectHtml('f_only', [['', '不限'], ['json', 'JSON 响应'], ['body', '有响应体']], '')) +
      fld('include 正则', inputHtml('f_include', '', 'placeholder="/api/items"')) +
      fld('exclude 正则', inputHtml('f_exclude', '', 'placeholder="heartbeat|log"')) +
      '</div>' +
      '<div id="cap-meta" class="muted" style="margin-bottom:10px">加载中…</div>' +
      '<div id="cap-table"></div>' +
      '</div></div>' +
      // 勾选操作条放在卡片外：.card 是 overflow:hidden，放里面 sticky 会失效
      '<div class="sel-bar"><div class="grow" id="sel-info">已选 0 个接口</div>' +
      '<button class="btn btn--sm" data-act="sel-all">全选当前筛选结果</button>' +
      '<button class="btn btn--sm" data-act="sel-none">清空选择</button>' +
      '<button class="btn btn--sm btn--danger" data-act="del-sel" disabled>删除选中</button>' +
      '<button class="btn btn--primary" data-act="next" disabled>下一步：配置采集任务（定时 + 落库）</button></div>';

    function filterQuery() {
      var f = state.filter;
      var q = [];
      if (f.q) q.push('q=' + encodeURIComponent(f.q));
      if (f.method) q.push('method=' + encodeURIComponent(f.method));
      if (f.type) q.push('type=' + encodeURIComponent(f.type));
      if (f.include) q.push('include=' + encodeURIComponent(f.include));
      if (f.exclude) q.push('exclude=' + encodeURIComponent(f.exclude));
      if (f.with_body) q.push('with_body=1');
      if (f.json) q.push('json=1');
      return q.length ? '?' + q.join('&') : '';
    }

    function readFilter() {
      state.filter.q = val(root, 'f_q').trim();
      state.filter.method = val(root, 'f_method');
      state.filter.type = val(root, 'f_type');
      var st = val(root, 'f_status');
      state.filter.include = val(root, 'f_include').trim();
      state.filter.exclude = val(root, 'f_exclude').trim();
      state.filter.json = false;
      state.filter.with_body = false;
      state.filter.min_status = 0;
      state.filter.max_status = 0;
      if (st === '2xx') { state.filter.min_status = 200; state.filter.max_status = 299; }
      if (st === '4xx') { state.filter.min_status = 400; state.filter.max_status = 499; }
      if (st === '5xx') { state.filter.min_status = 500; state.filter.max_status = 599; }
      var only = val(root, 'f_only');
      state.filter.json = only === 'json';
      state.filter.with_body = only === 'body';
    }

    function queryWithStatus() {
      var base = filterQuery();
      var extra = [];
      if (state.filter.min_status) extra.push('min_status=' + state.filter.min_status);
      if (state.filter.max_status) extra.push('max_status=' + state.filter.max_status);
      if (!extra.length) return base;
      return (base ? base + '&' : '?') + extra.join('&');
    }

    function paintMeta(meta, facets) {
      var host = root.querySelector('#cap-meta');
      if (!host) return;
      var parts = [];
      if (meta && meta.source) {
        parts.push(meta.source === 'extension'
          ? '<span class="chip">浏览器插件录制</span>'
          : '<span class="chip">内置浏览器录制</span>');
      }
      if (meta && meta.page_url) parts.push('页面 ' + esc(meta.page_url));
      if (meta && meta.types && meta.types.length) parts.push('范围 ' + esc(meta.types.join(' + ')));
      if (meta && meta.started_at) parts.push('开始于 ' + esc(fmtDateTime(meta.started_at)));
      parts.push(meta && meta.stopped ? '已停止录制' : '<span style="color:#4f46e5">录制中…</span>');
      if (facets) {
        parts.push('共 ' + facets.total + ' 条，筛选后 ' + facets.kept + ' 条');
        var methods = Object.keys(facets.by_method || {}).map(function (k) { return k + ' ' + facets.by_method[k]; });
        if (methods.length) parts.push(methods.join(' / '));
        // 按类型统计：一眼看出「页面文档」到底有没有录到
        var tps = Object.keys(facets.by_type || {}).map(function (k) { return k + ' ' + facets.by_type[k]; });
        if (tps.length) parts.push('类型 ' + tps.join(' / '));
        if (meta && meta.skipped) {
          var sk = Object.keys(meta.skipped).map(function (k) { return k + ' ' + meta.skipped[k]; });
          if (sk.length) parts.push('范围外跳过 ' + sk.join(' / '));
        }
        if (facets.hosts && facets.hosts.length) parts.push('站点：' + facets.hosts.map(esc).join('、'));
      }
      host.innerHTML = parts.join(' · ');
    }

    function paintTable(items) {
      var host = root.querySelector('#cap-table');
      if (!host) return;
      if (!items.length) {
        host.innerHTML = U().emptyCard('没有匹配的请求',
          '换个关键字或放宽筛选条件；如果还没录到东西，先在内置浏览器（或插件录制时的那个标签页）里点点页面。');
        syncSelBar();
        return;
      }
      host.innerHTML = '<div class="table-wrap"><table class="table"><thead><tr>' +
        '<th class="check-cell"><input type="checkbox" class="row-check" data-act="check-all"></th>' +
        '<th style="width:44px">#</th><th style="width:70px">方法</th><th>URL</th>' +
        '<th style="width:64px">状态</th><th style="width:64px">类型</th>' +
        '<th class="num" style="width:78px">大小</th><th class="num" style="width:74px">耗时</th>' +
        '<th style="width:60px">响应</th><th style="width:60px">操作</th></tr></thead><tbody>' +
        items.map(function (it) {
          var picked = !!state.sel[it.id];
          return '<tr' + (picked ? ' class="is-selected"' : '') + '>' +
            '<td class="check-cell"><input type="checkbox" class="row-check" data-pick="' + esc(it.id) + '"' + (picked ? ' checked' : '') + '></td>' +
            '<td class="muted">' + esc(it.index) + '</td>' +
            '<td>' + methodChip(it.method) + '</td>' +
            '<td><div class="url-cell" title="' + esc(it.url) + '">' + esc(it.path || it.url) +
            (it.query ? '<span class="muted">?' + esc(it.query.slice(0, 60)) + '</span>' : '') + '</div></td>' +
            '<td>' + statusChip(it.status) + '</td>' +
            '<td class="muted">' + esc(it.type) + '</td>' +
            '<td class="num muted">' + esc(fmtBytes(it.bytes)) + '</td>' +
            '<td class="num muted">' + esc(fmtMS(it.duration_ms)) + '</td>' +
            '<td>' + (it.has_body ? '<button class="link-btn" data-act="preview" data-id="' + esc(it.id) + '">查看</button>' : '<span class="muted">—</span>') +
            '</td>' +
            '<td><button class="link-btn" style="color:var(--red)" data-act="del-one" data-id="' + esc(it.id) +
            '" title="从录制结果里删掉这条">删除</button></td></tr>';
        }).join('') + '</tbody></table></div>';
      syncSelBar();
    }

    function syncSelBar() {
      var n = Object.keys(state.sel).length;
      var info = root.querySelector('#sel-info');
      if (info) {
        info.innerHTML = n
          ? '已选中 <b>' + n + '</b> 个接口 —— 点右侧「下一步」配置定时与落库'
          : '还没勾选：先在左侧勾上要抓的接口（可用上面的筛选 / 删除先把噪声去掉）';
      }
      var btn = root.querySelector('[data-act="next"]');
      if (btn) btn.disabled = n === 0;
      var del = root.querySelector('[data-act="del-sel"]');
      if (del) {
        del.disabled = n === 0;
        del.textContent = n ? ('删除选中（' + n + '）') : '删除选中';
      }
    }

    // 删除录制结果里的若干条（单条 / 批量）
    function deleteRows(ids) {
      if (!ids || !ids.length) return;
      api('DELETE', '/api/collect/captures/' + encodeURIComponent(state.id) + '/requests', { ids: ids })
        .then(function (res) {
          ids.forEach(function (x) { delete state.sel[x]; });
          toast(res.message || ('已删除 ' + ids.length + ' 条'), 'success');
          load();
        }).catch(function () { });
    }

    function load() {
      readFilter();
      api('GET', '/api/collect/captures/' + encodeURIComponent(state.id) + queryWithStatus(), undefined, { silent: true })
        .then(function (res) {
          state.data = res;
          paintMeta(res.capture, res.facets);
          paintTable(U().apiItems(res));
        })
        .catch(function (err) {
          root.querySelector('#cap-table').innerHTML = U().errorCard(err.message || '读取录制结果失败');
        });
    }

    // ---- 事件 ----
    root.addEventListener('input', function (e) {
      var t = e.target;
      if (!t || !t.id) return;
      if (['f_q', 'f_include', 'f_exclude'].indexOf(t.id) >= 0) {
        clearTimeout(state.timer);
        state.timer = setTimeout(load, 320);
      }
    });
    root.addEventListener('change', function (e) {
      var t = e.target;
      if (!t) return;
      if (t.getAttribute && t.getAttribute('data-act') === 'check-all') {
        var boxes = Array.prototype.slice.call(root.querySelectorAll('[data-pick]'));
        boxes.forEach(function (b) {
          var id2 = b.getAttribute('data-pick');
          if (t.checked) state.sel[id2] = true; else delete state.sel[id2];
          b.checked = t.checked;
          var tr = b.closest('tr');
          if (tr) tr.classList.toggle('is-selected', t.checked);
        });
        syncSelBar();
        return;
      }
      var pick = t.getAttribute && t.getAttribute('data-pick');
      if (pick) {
        if (t.checked) state.sel[pick] = true; else delete state.sel[pick];
        var row = t.closest('tr');
        if (row) row.classList.toggle('is-selected', t.checked);
        syncSelBar();
        return;
      }
      if (['f_method', 'f_type', 'f_status', 'f_only'].indexOf(t.id) >= 0) load();
    });
    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      var actEl = t.closest('[data-act]');
      var act = actEl ? actEl.getAttribute('data-act') : '';
      if (act === 'back') { go('#/collect/captures'); return; }
      if (act === 'refresh') { load(); return; }
      if (act === 'stop') {
        api('POST', '/api/collect/captures/' + encodeURIComponent(state.id) + '/stop', {}).then(function (res) {
          toast('已停止录制（' + (res.count || 0) + ' 条）', 'success');
          load();
        }).catch(function () { });
        return;
      }
      if (act === 'del') {
        deleteCapture(null, state.id);
        return;
      }
      if (act === 'sel-all') {
        var items = U().apiItems(state.data);
        items.forEach(function (it) { state.sel[it.id] = true; });
        paintTable(items);
        return;
      }
      if (act === 'sel-none') { state.sel = {}; paintTable(U().apiItems(state.data)); return; }
      if (act === 'del-one') {
        var oneId = actEl.getAttribute('data-id');
        U().confirmDialog({
          title: '删除这条录制结果',
          message: '只从这次的录制结果里删掉它（不影响其它条目，也不影响已入库数据）。',
          confirmText: '删除这条'
        }).then(function (ok) { if (ok) deleteRows([oneId]); });
        return;
      }
      if (act === 'del-sel') {
        var ids = Object.keys(state.sel);
        if (!ids.length) return;
        U().confirmDialog({
          title: '删除选中的录制结果',
          message: '将从这次的录制结果里删掉 ' + ids.length + ' 条请求。剩下的条目仍然可以正常用来建采集任务。',
          confirmText: '删除 ' + ids.length + ' 条'
        }).then(function (ok) { if (ok) deleteRows(ids); });
        return;
      }
      if (act === 'next') { openCreateTaskModal(state.id, Object.keys(state.sel), state.data && state.data.capture); return; }
      if (act === 'preview') { showBodyPreview(state.id, actEl.getAttribute('data-id')); return; }
    });

    load();
    // 录制中自动刷新列表（3 秒），停止后不再刷
    var timer = setInterval(function () {
      if (!document.body.contains(root)) return;
      if (state.data && state.data.capture && state.data.capture.stopped) return;
      var q = val(root, 'f_q');
      if (q) return; // 用户正在搜索时不要打断
      load();
    }, 3000);
    onCleanup(function () { clearInterval(timer); clearTimeout(state.timer); });
  }

  function showBodyPreview(captureId, requestId) {
    api('POST', '/api/collect/captures/' + encodeURIComponent(captureId) + '/analyze', { request_id: requestId })
      .then(function (res) {
        var body = '<div class="muted" style="margin-bottom:8px">记录数组路径：' +
          (res.records_path ? '<code>' + esc(res.records_path) + '</code>' : '（响应本身是数组）') +
          '　记录数：' + esc(res.records) + '</div>';
        if (res.error) {
          body += '<div class="alert alert--warn">' + esc(res.error) + '</div>';
        } else {
          body += '<div class="chips" style="margin-bottom:10px">' +
            (res.fields || []).map(function (f) {
              return '<span class="chip">' + esc(f.column) + ' ← ' + esc(f.path) + '</span>';
            }).join('') + '</div>';
          body += tablePreviewHtml(res.rows || []);
        }
        U().openModal({ title: '响应预览与字段识别', width: 860, body: body });
      }).catch(function () { });
  }

  function tablePreviewHtml(rows) {
    if (!rows || !rows.length) return '<div class="muted">没有可预览的行</div>';
    var cols = {};
    rows.forEach(function (r) { Object.keys(r).forEach(function (k) { cols[k] = true; }); });
    var list = Object.keys(cols);
    return '<div class="table-wrap"><table class="table table--fields"><thead><tr>' +
      list.map(function (c) { return '<th>' + esc(c) + '</th>'; }).join('') + '</tr></thead><tbody>' +
      rows.map(function (r) {
        return '<tr>' + list.map(function (c) {
          var v = r[c];
          var text = (v === null || v === undefined) ? '' : (typeof v === 'object' ? JSON.stringify(v) : String(v));
          return '<td>' + esc(text.slice(0, 120)) + '</td>';
        }).join('') + '</tr>';
      }).join('') + '</tbody></table></div>';
  }

  /* ------------------------------------------------- 建任务弹窗 */

  function openCreateTaskModal(captureId, requestIds, captureMeta) {
    loadOptions().then(function () {
      var name = '采集任务 ' + new Date().toLocaleString('zh-CN', { hour12: false });
      var body = '<div class="form-grid">' +
        fld('任务名称', inputHtml('t_name', name)) +
        fld('说明（可空）', inputHtml('t_desc', '')) +
        fld('起始页 / Referer', inputHtml('t_start', (captureMeta && captureMeta.page_url) || '')) +
        '</div>' +
        '<div class="alert alert--info" style="margin:6px 0 4px">已选 ' + requestIds.length +
        ' 个接口。下面这些参数决定"什么时候抓、抓多快、写到哪"——默认值已经很保守，可以按需调整。</div>' +
        configFormHtml('t', {
          schedule: { mode: 'interval', every_seconds: 300, jitter_percent: 20 },
          throttle: { min_interval_ms: 1000, jitter_percent: 30, timeout_ms: 20000, retries: 2, backoff_ms: 1500, max_fail_streak: 3, max_pages_cap: 50 },
          sink: { driver: 'sqlite', table: 'harvest_records', mode: 'append', batch_size: 200 },
          session: { mode: 'browser', host: '', user_agent: (captureMeta && captureMeta.user_agent) || '', referer: (captureMeta && captureMeta.page_url) || '' }
        });

      var m = U().openModal({
        title: '新建定时采集任务', width: 900, body: body,
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn btn--primary" id="t_save">创建任务</button>'
      });
      bindCommonActions(m.overlay, 't', { captureId: captureId, requestIds: requestIds });
      loadRecordings().then(function () { refreshRecSelects(m.overlay); }).catch(function () { });

      m.overlay.querySelector('#t_save').addEventListener('click', function () {
        var cfg = readConfigForm(m.overlay, 't');
        var payload = {
          name: val(m.overlay, 't_name').trim(),
          description: val(m.overlay, 't_desc'),
          start_url: val(m.overlay, 't_start').trim(),
          capture_id: captureId,
          request_ids: requestIds,
          schedule: cfg.schedule,
          throttle: cfg.throttle,
          sink: cfg.sink,
          session: cfg.session,
          prelogin: cfg.prelogin,
          common: cfg.common
        };
        if (!payload.name) { toast('请填写任务名称', 'error'); return; }
        api('POST', '/api/collect/tasks', payload).then(function (task) {
          toast('任务已创建', 'success');
          m.close();
          go('#/collect/task/' + encodeURIComponent(task.id));
        }).catch(function () { });
      });
    });
  }

  /* ======================================================================
   * 页面 3：#/collect/task/{id} —— 任务配置 + 接口 + 最近运行
   * ====================================================================== */

  function renderTask(app, id) {
    var root = pageRoot(app);
    var state = { id: id, task: null, profiles: [], tkTab: 'overview' };

    root.innerHTML = headHtml('采集任务',
      '按页签分区：概览 / 公共参数 / 登录 / 接口设计 / 调度与落库 / 文件产出 / 最近运行。' +
      '接口块里可以改请求头、请求体、分页，也能看到录制时的原始请求数据（明文）；公共参数里的变量还能在 URL 里用 {{变量名}}。改完点「保存配置」。',
      '<button class="btn btn--sm" data-act="back">返回列表</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--primary" data-act="run">立即运行</button>' +
      '<button class="btn btn--sm" data-act="preview">试抓预览</button>' +
      '<button class="btn btn--sm" data-act="selftest">自检</button>' +
      '<button class="btn btn--sm btn--ghost" data-act="test-sink">测试落库</button>' +
      '<button class="btn btn--sm btn--danger" data-act="del">删除任务</button>') +
      '<div id="task-body">' + U().loadingCard('加载任务…') + '</div>';

    function load() {
      Promise.all([
        api('GET', '/api/collect/tasks/' + encodeURIComponent(state.id)),
        api('GET', '/api/collect/sinks', undefined, { silent: true }).catch(function () { return { items: [] }; })
      ]).then(function (out) {
        state.profiles = U().apiItems(out[1]);
        state.task = out[0];
        paint(state.task);
      }).catch(function (err) {
        root.querySelector('#task-body').innerHTML = U().errorCard(err.message || '任务不存在');
      });
    }

    function paint(task) {
      var host = root.querySelector('#task-body');
      var eps = task.endpoints || [];
      function tabBtn(key, label, tip) {
        return '<button class="tk-tab" data-tk-tab="' + key + '" title="' + esc(tip || label) + '">' + esc(label) + '</button>';
      }
      host.innerHTML = '<div class="card"><div class="card-head"><h2>' + esc(task.name) + ' ' +
        taskStatusBadge(task.status, task.running) + '</h2>' +
        '<div class="card-tools"><span class="muted">' + esc(task.schedule_text || '') + '</span></div></div>' +
        '<div class="card-body">' +
        '<div class="stat-line">' +
        '<div><span>接口</span><b>' + esc(task.endpoint_count) + '</b></div>' +
        '<div><span>下次触发</span><b>' + esc(fmtWhen(task.next_run_at)) + '</b></div>' +
        '<div><span>最近结果</span><b>' + esc(runStatusText(task.last_status)) + '</b></div>' +
        '<div><span>最近写入</span><b>' + esc(task.last_written || 0) + ' 行</b></div>' +
        '<div><span>累计运行</span><b>' + esc(task.run_count || 0) + ' 次</b></div>' +
        '</div>' +
        (task.last_error ? '<div class="alert alert--warn">' + esc(task.last_error) + '</div>' : '') +
        // 分区页签：不再一页长表单
        '<nav class="tk-tabs" id="tk-tabs">' +
        tabBtn('overview', '概览', '任务名称 / 说明 / 起始页') +
        tabBtn('common', '公共参数', '所有接口共用的 query/body 参数、请求头、Cookie、参数绑定、登录后刷新') +
        tabBtn('login', '登录', '前置登录（UI 自动化）+ 浏览器会话共享') +
        tabBtn('eps', '接口设计', '每个接口的方法 / URL / 分页 / 请求头 / 请求体 / 字段映射') +
        tabBtn('runtime', '调度与落库', '定时、限速、写入目标') +
        tabBtn('files', '文件产出', '这个任务产出的 Excel / JSON / JSONL，直接下载') +
        tabBtn('runs', '最近运行', '这个任务的运行记录，点进去看明文报文与 SQL') +
        '</nav>' +
        '<div class="tk-pane" data-pane="overview">' +
        '<div class="form-grid">' +
        fld('任务名称', inputHtml('tk_name', task.name)) +
        fld('说明', inputHtml('tk_desc', task.description || '')) +
        fld('起始页 / Referer', inputHtml('tk_start', task.start_url || '')) +
        '</div>' +
        '<div class="ds-actions"><button class="btn btn--primary" data-act="save">保存配置</button>' +
        '<span class="field-hint" style="margin-left:10px">保存后定时计划立即生效（改计划会重算下次触发时间）。</span></div>' +
        '</div>' +
        '<div class="tk-pane" data-pane="common" hidden></div>' +
        '<div class="tk-pane" data-pane="login" hidden></div>' +
        '<div class="tk-pane" data-pane="eps" hidden>' +
        '<div class="ds-block"><div class="ds-block-title">接口（' + eps.length + ' 个，勾选框控制是否参与采集）</div>' +
        '<div id="ep-list">' + eps.map(epBlockHtml).join('') + '</div>' +
        '</div>' +
        '</div>' +
        '<div class="tk-pane" data-pane="runtime" hidden>' +
        '<div class="ds-block"><div class="ds-block-title"><span>写入目标</span>' +
        '<span class="muted">' + esc(task.sink_target || '') + '</span></div>' +
        '<div class="form-grid">' +
        fld('引用「数据写入」配置', selectHtml('tk_profile', profileOptions(), task.sink_profile_id || ''),
          '选一套会把它的连接 / 表名复制到本任务；之后改配置不会再影响这个任务') +
        '</div>' +
        '<label class="check-line" style="margin-top:6px"><input type="checkbox" id="tk_split"' +
        (task.split_by_endpoint ? ' checked' : '') + '> 每个接口单独分表 / 分文件' +
        '<span class="field-hint">　表名 = 任务表名 + "_" + 接口名；某个接口单独填了表名就以接口为准</span></label>' +
        '</div>' +
        // 定时 / 限速 / 落库三块由下面从 #tk-cfg 里搬进来
        '<div class="ds-actions"><button class="btn btn--primary" data-act="save">保存配置</button></div>' +
        '</div>' +
        // 文件产出 / 最近运行：以前堆在页面底部，现在各自一个页签
        '<div class="tk-pane" data-pane="files" hidden><div id="task-files"></div></div>' +
        '<div class="tk-pane" data-pane="runs" hidden><div id="task-runs"></div></div>' +
        // 参数表单先整份渲染在隐藏容器里，再按标题搬到对应页签（保持 readConfigForm 的 id 约定不变）
        '<div id="tk-cfg" hidden>' + configFormHtml('tk', task) + '</div>' +
        '</div></div>' +
        '</div>';

      // 把配置块分配到页签：公共参数 / 登录 / 调度与落库
      (function distribute() {
        var cfgHost = host.querySelector('#tk-cfg');
        if (!cfgHost) return;
        var paneCommon = host.querySelector('[data-pane="common"]');
        var paneLogin = host.querySelector('[data-pane="login"]');
        var paneRuntime = host.querySelector('[data-pane="runtime"]');
        Array.prototype.slice.call(cfgHost.children).forEach(function (el) {
          if (!el.classList || !el.classList.contains('ds-block')) return;
          var titleEl = el.querySelector('.ds-block-title');
          var title = titleEl ? titleEl.textContent : '';
          var target = paneRuntime;
          if (title.indexOf('公共参数') >= 0) target = paneCommon;
          else if (title.indexOf('登录态') >= 0 || title.indexOf('前置登录') >= 0) target = paneLogin;
          if (target) target.appendChild(el);
        });
        if (cfgHost.parentNode) cfgHost.parentNode.removeChild(cfgHost);
      })();

      // 页签切换
      (function wireTabs() {
        var nav = host.querySelector('#tk-tabs');
        if (!nav) return;
        var panes = host.querySelectorAll('.tk-pane');
        function show(key) {
          state.tkTab = key;
          Array.prototype.forEach.call(nav.querySelectorAll('.tk-tab'), function (b) {
            b.classList.toggle('active', b.getAttribute('data-tk-tab') === key);
          });
          Array.prototype.forEach.call(panes, function (p) {
            p.hidden = p.getAttribute('data-pane') !== key;
          });
        }
        nav.addEventListener('click', function (e) {
          var b = e.target.closest ? e.target.closest('[data-tk-tab]') : null;
          if (!b) return;
          show(b.getAttribute('data-tk-tab'));
        });
        show(state.tkTab || 'overview');
      })();

      // 公共参数工具条：粘贴导入 / 从录制汇总 / 登录后刷新
      bindCommonActions(host, 'tk', { taskId: state.id });

      // 「登录流程录制」下拉需要录制列表：异步补一次（只拉一次）
      loadRecordings().then(function () { refreshRecSelects(host); }).catch(function () { });

      // 文件产出 / 最近运行两个页签的内容（重新渲染后容器是新的，必须重拉）
      loadRuns();
      loadFiles(task);
    }

    function profileOptions() {
      var out = [['', '（不引用，直接用任务里的配置）']];
      (state.profiles || []).forEach(function (p) {
        out.push([p.id, p.name + ' · ' + (p.target || p.table || '')]);
      });
      return out;
    }

    /** 把选中的落库配置填进表单（保存后就是任务自己的值） */
    function applyProfile(pid) {
      var p = null;
      (state.profiles || []).forEach(function (x) { if (x.id === pid) p = x; });
      if (!p) return;
      var set = function (id, v) { var el = root.querySelector('#' + id); if (el) el.value = v; };
      set('tk_driver', p.driver || 'sqlite');
      set('tk_dsn', p.dsn || '');
      set('tk_table', p.table || 'harvest_records');
      set('tk_sinkmode', p.mode || 'append');
      set('tk_key', p.key_column || '');
      set('tk_batch', p.batch_size || 200);
      toast('已填入「' + p.name + '」的连接信息，点保存生效', 'info');
    }

    /** 产出文件（只显示这个任务表名对应的文件） */
    function loadFiles(task) {
      var host = root.querySelector('#task-files');
      if (!host) return;
      var table = (task && task.sink && task.sink.table) || '';
      var driver = ((task && task.sink && task.sink.driver) || '').toLowerCase();
      if (driver === 'mysql' || driver === 'postgres' || driver === 'postgresql') {
        host.innerHTML = '<div class="card"><div class="card-head"><h2>文件产出</h2></div>' +
          '<div class="card-body muted">这个任务直接写数据库（' + esc(driver) + '），没有文件产出。' +
          '想看数据请连库查表，或把「写入数据库」改成 Excel / JSON 再跑一次。</div></div>';
        return;
      }
      renderFilesCard(host, table);
    }

    function findEp(eid) {
      var list = state.task.endpoints || [];
      for (var i = 0; i < list.length; i++) if (list[i].id === eid) return list[i];
      return null;
    }

    /** 把录制快照填进"可编辑的"请求头 / 请求体（只改表单，点保存才生效） */
    function fillSample(eid, what) {
      var ep = findEp(eid);
      if (!ep) return;
      var hEl = root.querySelector('#ep_headers_' + eid);
      var bEl = root.querySelector('#ep_body_' + eid);
      var filled = [];
      if (what === 'headers' || what === 'all') {
        var txt = headersToText(sessionlessHeaders(ep.sample_headers));
        if (hEl && txt) { hEl.value = txt; filled.push('请求头'); }
        else if (what === 'headers') toast('这条接口没有录制到的请求头', 'error');
      }
      if (what === 'body' || what === 'all') {
        var body = stripTruncMark(ep.sample_req_body);
        if (bEl && body) { bEl.value = body; filled.push('请求体'); }
        else if (what === 'body') toast('录制时没有请求体（GET 一般是空的）', 'info');
      }
      if (filled.length) {
        // 展开「请求头 / 请求体」折叠区，方便直接改
        var box = root.querySelector('#ep_reqbox_' + eid);
        if (box) box.open = true;
        toast('已填入 ' + filled.join(' / ') + '，改完点「保存配置」生效', 'success');
        if (hEl && (what === 'headers' || what === 'all')) hEl.focus();
        else if (bEl) bEl.focus();
      }
    }

    // 接口块默认折叠（只留标题行），需要改的时候点开——多接口任务不再一屏塞满
    function epBlockHtml(ep, i) {
      var fields = (ep.fields || []).map(function (f) {
        return '<span class="chip">' + esc(f.column) + ' ← ' + esc(f.path) + '</span>';
      }).join('') || '<span class="muted">未配置字段（运行时会按响应自动识别）</span>';
      var pg = normalizePag(ep.pagination);
      var opened = isEpOpen(ep.id, i);
      var eff = (state.task && state.task.endpoint_sinks && state.task.endpoint_sinks[ep.id]) || null;
      var targetText = eff && eff.table ? ' · 写入 ' + eff.table : '';
      var summary = '<span class="ep-summary">' + esc(String(ep.method || 'GET').toUpperCase()) +
        ' · ' + esc(pagShort(pg)) +
        ' · ' + (ep.fields || []).length + ' 字段' + esc(targetText) +
        ((ep.headers && Object.keys(ep.headers).length) ? ' · ' + Object.keys(ep.headers).length + ' 请求头' : '') +
        (ep.enabled === false ? ' · <span style="color:var(--red)">未启用</span>' : '') + '</span>';
      return '<div class="ep-block' + (opened ? ' is-open' : '') + '">' +
        '<div class="ep-head">' +
        '<button class="ep-chev" type="button" data-ep-toggle="' + esc(ep.id) + '" title="展开 / 收起">' +
        (opened ? '▾' : '▸') + '</button>' +
        '<span class="ep-idx">#' + (i + 1) + '</span>' +
        '<label class="check-line"><input type="checkbox" data-ep-enabled="' + esc(ep.id) + '"' +
        (ep.enabled !== false ? ' checked' : '') + '> 启用</label>' +
        '<input class="input" style="flex:1 1 160px" id="ep_name_' + esc(ep.id) + '" data-ep-name="' + esc(ep.id) + '" value="' + esc(ep.name) + '">' +
        summary +
        '<button class="btn btn--sm" data-ep-fields="' + esc(ep.id) + '">字段映射</button>' +
        '<button class="btn btn--sm" data-ep-preview="' + esc(ep.id) + '" title="只抓这一个接口的第一页（不写库）">试抓</button>' +
        '<button class="btn btn--sm" data-ep-pgall="' + esc(ep.id) + '" title="把这个接口的分页配置套到所有接口">分页套用全部</button>' +
        '<button class="btn btn--sm" data-ep-hdrall="' + esc(ep.id) + '" title="把这个接口的请求头套到所有接口">请求头套用全部</button>' +
        '<button class="btn btn--sm btn--danger" data-ep-del="' + esc(ep.id) + '">删除</button>' +
        '</div>' +
        '<div class="ep-body">' +
        '<div class="form-grid">' +
        fld('方法', selectHtml('ep_method_' + ep.id, [['GET', 'GET'], ['POST', 'POST'], ['PUT', 'PUT'], ['PATCH', 'PATCH'], ['DELETE', 'DELETE']], ep.method)) +
        fld('URL（支持 {{page}} {{date}} {{timestamp}} {{env:NAME}}）', inputHtml('ep_url_' + ep.id, ep.url)) +
        fld('记录路径（数组位置）', inputHtml('ep_path_' + ep.id, ep.records_path || '', 'placeholder="data.list"')) +
        fld('写入表 / 文件名（留空 = 用任务的）', inputHtml('ep_table_' + ep.id, (ep.sink && ep.sink.table) || '', 'placeholder="例如 products；文件类是文件名"')) +
        '</div>' +
        '<details class="ep-sample" id="ep_reqbox_' + esc(ep.id) + '"><summary>请求头 / 请求体（可改）' +
        ((ep.headers && Object.keys(ep.headers).length) ? ' · 请求头 ' + Object.keys(ep.headers).length + ' 项' : '') +
        (ep.body ? ' · 请求体 ' + esc(fmtBytes(String(ep.body).length)) : '') +
        '</summary>' +
        '<div class="form-grid" style="margin-top:8px">' +
        fld('请求头（一行一个 Name: Value，可改）', textareaHtml('ep_headers_' + ep.id, headersToText(ep.headers), 3),
          'Cookie / User-Agent / Referer 由「会话」提供，不用写；值里可用 {{env:NAME}}') +
        fld('请求体（原样发送，可改）', textareaHtml('ep_body_' + ep.id, ep.body || '', 3),
          '支持 {{page}} {{date}} {{env:NAME}}；分页用「请求体 JSON」时会自动改写翻页字段') +
        '</div>' +
        '</details>' +
        '<div class="ep-pager">' +
        '<div class="ep-pager-head"><b>分页</b><span class="muted" id="ep_phint_' + esc(ep.id) + '">' + esc(describePag(pg)) + '</span>' +
        '<button class="btn btn--sm" data-ep-detect="' + esc(ep.id) + '">自动识别分页</button></div>' +
        '<div class="form-grid">' +
        fld('翻页方式', selectHtml('ep_pmode_' + ep.id, [['auto', '自动（推荐）'], ['page', '页码 page=1,2,3'], ['offset', '偏移 offset=0,20,40'], ['cursor', '游标 cursor=xxx'], ['none', '不分页（只抓一次）']], pg.mode)) +
        fld('参数名', inputHtml('ep_pparam_' + ep.id, pg.param, 'placeholder="page / offset / cursor"')) +
        fld('参数位置', selectHtml('ep_pin_' + ep.id, [['auto', '自动（POST 进请求体，GET 进 URL）'], ['query', 'URL 参数'], ['body', '请求体 JSON']], pg.in)) +
        fld('每页条数', numHtml('ep_psize_' + ep.id, pg.page_size, 'min="0" placeholder="自动"')) +
        fld('条数参数名（可空）', inputHtml('ep_psizeparam_' + ep.id, pg.size_param, 'placeholder="size / limit / pageSize"'),
          '填了就把「每页条数」一起发给接口，和页码参数同进 query 或 body') +
        fld('最多页数', numHtml('ep_pmax_' + ep.id, pg.max_pages, 'min="1" max="2000"'),
          '填了就以你填的为准（硬顶 2000）；留空则用任务的「分页默认上限」') +
        fld('起始值', numHtml('ep_pstart_' + ep.id, pg.start, 'min="0"')) +
        '</div>' +
        '<div class="form-grid">' +
        fld('总数字段（可空）', inputHtml('ep_ptotal_' + ep.id, pg.total_path, 'placeholder="data.total"')) +
        fld('结束标志（可空）', inputHtml('ep_pmore_' + ep.id, pg.has_more_path, 'placeholder="data.has_more"')) +
        fld('游标字段（游标模式用）', inputHtml('ep_pnext_' + ep.id, pg.next_path, 'placeholder="data.next_cursor"')) +
        '</div>' +
        '<label class="check-line" style="margin-top:6px"><input type="checkbox" data-ep-short="' + esc(ep.id) + '"' +
        (pg.stop_on_short ? ' checked' : '') + '> 短页即止（返回条数 &lt; 每页条数时视为最后一页）</label>' +
        '</div>' +
        '<details class="ep-sample"><summary>字段映射（' + (ep.fields || []).length + ' 个字段 ← 响应路径）</summary>' +
        '<div class="chips" style="margin:6px 0 2px">' + fields + '</div>' +
        '<div class="muted">要增删改字段点上面的「字段映射」按钮（从样本重新识别也在那儿）</div>' +
        '</details>' +
        '<details class="ep-sample"><summary>录制时的请求数据' +
        (ep.sample_at ? '（' + esc(ep.sample_at) + '）' : '') + ' · 点开看' +
        '</summary>' +
        sampleHtml(ep) +
        '<div style="margin-top:8px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">' +
        '<button class="btn btn--sm" data-ep-fill="' + esc(ep.id) + '" data-fill="headers">填入请求头</button>' +
        '<button class="btn btn--sm" data-ep-fill="' + esc(ep.id) + '" data-fill="body">填入请求体</button>' +
        '<button class="btn btn--sm" data-ep-fill="' + esc(ep.id) + '" data-fill="all">全部填入</button>' +
        '<span class="muted">填进上面可编辑的输入框，改完点「保存配置」才生效</span>' +
        '</div>' +
        '</details>' +
        '</div>' +
        '</div>';
    }

    /** 录制快照（请求侧）：请求头 / 请求体 / 来源页面（明文，含 Cookie / Authorization） */
    function sampleHtml(ep) {
      var parts = [];
      var meta = [];
      if (ep.sample_page_url) meta.push('页面 ' + ep.sample_page_url);
      if (ep.created_from) meta.push('录制请求 id ' + ep.created_from);
      if (meta.length) {
        parts.push('<div class="muted" style="margin:2px 0 8px">' + esc(meta.join('　·　')) + '</div>');
      }

      var hk = ep.sample_headers ? Object.keys(ep.sample_headers) : [];
      if (hk.length) {
        parts.push('<div class="field"><label>请求头（录制时原样，明文）</label>' +
          '<div class="table-wrap"><table class="table table--fields"><tbody>' +
          hk.map(function (k) {
            return '<tr><td class="mono" style="width:30%">' + esc(k) + '</td><td class="mono">' + esc(ep.sample_headers[k]) + '</td></tr>';
          }).join('') +
          '</tbody></table></div></div>');
      }
      if (ep.sample_req_body) {
        parts.push('<div class="field"><label>请求体（录制时）</label>' +
          '<pre class="diagnose-out" style="max-height:260px">' + esc(ep.sample_req_body) + '</pre></div>');
      }
      if (!parts.length) {
        return '<div class="muted">这条接口没有录制快照（手工添加的接口，或老版本建的任务）。重录一遍接口就能带上。</div>';
      }
      return parts.join('');
    }

    function openFieldsModal(eid) {
      var ep = (state.task.endpoints || []).filter(function (x) { return x.id === eid; })[0];
      if (!ep) return;
      var rows = (ep.fields || []).slice();
      if (!rows.length) rows = [{ column: '', path: '' }];

      function rowsHtml() {
        return rows.map(function (r, i) {
          return '<tr data-frow="' + i + '">' +
            '<td><input class="input input--cell" data-fcol value="' + esc(r.column || '') + '" placeholder="列名"></td>' +
            '<td><input class="input input--cell" data-fpath value="' + esc(r.path || '') + '" placeholder="data.list[0].name"></td>' +
            '<td class="actions-cell"><button class="icon-btn icon-btn--danger" data-fdel="' + i + '">✕</button></td>' +
            '</tr>';
        }).join('');
      }

      var m = U().openModal({
        title: '字段映射 · ' + (ep.name || ''),
        width: 720,
        body: '<div class="muted" style="margin-bottom:8px">列名会变成数据库里的字段名（中文/字母/数字/下划线，其他字符自动转下划线）；' +
          '路径支持 <code>a.b[0].c</code>，也支持内置值 <code>@now</code> <code>@date</code> <code>@url</code> <code>@endpoint</code> <code>@page</code>。</div>' +
          '<div class="table-wrap"><table class="table table--fields"><thead><tr><th style="width:38%">列名</th><th>响应路径</th><th></th></tr></thead>' +
          '<tbody id="frows">' + rowsHtml() + '</tbody></table></div>' +
          '<div class="ds-actions"><button class="btn btn--sm" id="fadd">+ 增加一行</button>' +
          '<button class="btn btn--sm" id="fauto">从样本重新识别</button>' +
          '<span class="field-hint" id="fauto-msg"></span></div>',
        footer: '<button class="btn" data-modal-close>取消</button><button class="btn btn--primary" id="fok">确定</button>'
      });

      var tbody = m.overlay.querySelector('#frows');
      function sync() {
        var trs = Array.prototype.slice.call(tbody.querySelectorAll('[data-frow]'));
        rows = trs.map(function (tr) {
          return {
            column: tr.querySelector('[data-fcol]').value.trim(),
            path: tr.querySelector('[data-fpath]').value.trim()
          };
        });
      }
      m.overlay.querySelector('#fadd').addEventListener('click', function () {
        sync();
        rows.push({ column: '', path: '' });
        tbody.innerHTML = rowsHtml();
      });
      tbody.addEventListener('click', function (e) {
        var d = e.target.closest && e.target.closest('[data-fdel]');
        if (!d) return;
        sync();
        rows.splice(parseInt(d.getAttribute('data-fdel'), 10), 1);
        if (!rows.length) rows = [{ column: '', path: '' }];
        tbody.innerHTML = rowsHtml();
      });
      m.overlay.querySelector('#fauto').addEventListener('click', function () {
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/analyze', { endpoint_id: eid })
          .then(function (res) {
            if (res.error) { m.overlay.querySelector('#fauto-msg').textContent = res.error; return; }
            rows = (res.fields || []).map(function (f) { return { column: f.column, path: f.path }; });
            if (!rows.length) rows = [{ column: '', path: '' }];
            tbody.innerHTML = rowsHtml();
            m.overlay.querySelector('#fauto-msg').textContent = '已识别 ' + rows.length + ' 个字段';
          }).catch(function () { });
      });
      m.overlay.querySelector('#fok').addEventListener('click', function () {
        sync();
        ep.fields = rows.filter(function (r) { return r.column && r.path; });
        m.close();
        paint(state.task);
      });
    }

    function collectEndpoints() {
      var eps = state.task.endpoints || [];
      return eps.map(function (ep) {
        return {
          id: ep.id,
          name: (val(root, 'ep_name_' + ep.id) || ep.name || '').trim() || ep.name,
          sample_body: ep.sample_body || '',
          // 录制快照：保存时必须原样带上，否则一改配置就把"录制时看到的报文"抹掉了
          sample_headers: ep.sample_headers || null,
          sample_req_body: ep.sample_req_body || '',
          sample_page_url: ep.sample_page_url || '',
          sample_at: ep.sample_at || '',
          created_from: ep.created_from || '',
          method: val(root, 'ep_method_' + ep.id) || ep.method || 'GET',
          url: (val(root, 'ep_url_' + ep.id) || ep.url || '').trim(),
          records_path: val(root, 'ep_path_' + ep.id).trim(),
          pagination: {
            mode: val(root, 'ep_pmode_' + ep.id) || 'none',
            param: val(root, 'ep_pparam_' + ep.id).trim(),
            in: val(root, 'ep_pin_' + ep.id) || 'auto',
            start: numVal(root, 'ep_pstart_' + ep.id, 0),
            page_size: numVal(root, 'ep_psize_' + ep.id, 0),
            size_param: val(root, 'ep_psizeparam_' + ep.id).trim(),
            max_pages: numVal(root, 'ep_pmax_' + ep.id, 0),
            total_path: val(root, 'ep_ptotal_' + ep.id).trim(),
            has_more_path: val(root, 'ep_pmore_' + ep.id).trim(),
            next_path: val(root, 'ep_pnext_' + ep.id).trim(),
            stop_on_short: !!(ep.pagination && ep.pagination.stop_on_short !== false)
          },
          enabled: ep.enabled !== false,
          headers: textToHeaders(val(root, 'ep_headers_' + ep.id)),
          body: (function () {
            var el = root.querySelector('#ep_body_' + ep.id);
            return el ? el.value : (ep.body || '');
          })(),
          fields: ep.fields || [],
          sink: epSinkValue(ep.id)
        };
      });
    }

    /** 接口级落库覆盖：只填表名（其余沿用任务配置）；留空 = 用任务的 */
    function epSinkValue(eid) {
      var table = (val(root, 'ep_table_' + eid) || '').trim();
      return table ? { table: table } : null;
    }

    // 用存下来的样本响应体重新识别分页（不写库、不发请求）
    function detectPagination(eid) {
      api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/detect-pagination',
        { endpoint_id: eid })
        .then(function (res) {
          var ep = findEp(eid);
          if (!ep) return;
          ep.pagination = res.pagination || { mode: 'none' };
          toast(res.pagination_text || '已重新识别分页', res.hint ? 'info' : 'success');
          paint(state.task);
          if (res.hint) toast(res.hint, 'info', 6000);
        }).catch(function () { });
    }

    /** 自检：登录 → 导出会话 → 真发一次请求，逐步看断在哪 */
    function selftest() {
      var withLogin = !!(state.task && state.task.prelogin && state.task.prelogin.enabled);
      U().openModal({
        title: '自检中…',
        width: 780,
        body: '<div class="muted">' + (withLogin ? '先跑一遍前置登录（会开浏览器，可能要十几秒）' : '用当前浏览器登录态') +
          '，然后导出会话、真发一次请求…</div>',
        footer: '<button class="btn" data-modal-close>关闭</button>'
      });
      api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/selftest', { run_prelogin: withLogin })
        .then(function (rep) {
          var steps = (rep && rep.steps) || [];
          var body = '<div class="muted" style="margin-bottom:10px">' +
            (rep && rep.ok ? '✔ 全链路通过' : '✘ 有步骤没通过（看下面标红的那条）') + '</div>' +
            steps.map(function (st) {
              return '<div class="ds-block" style="border-left:3px solid ' + (st.ok ? 'var(--green)' : 'var(--red)') + '">' +
                '<div class="ds-block-title"><span>' + (st.ok ? '✔ ' : '✘ ') + esc(st.title) + '</span></div>' +
                '<div class="mono" style="font-size:12px;line-height:1.7;word-break:break-all;white-space:pre-wrap">' +
                esc(st.detail) + '</div></div>';
            }).join('');
          U().openModal({
            title: '自检结果',
            width: 780,
            body: body || '<div class="muted">没有结果</div>',
            footer: '<button class="btn" data-modal-close>关闭</button>'
          });
        }).catch(function () { });
    }

    /** 只试抓一个接口（先保存，服务端读的是保存后的配置），不写库 */
    function previewEndpoint(epId) {
      toast('正在保存配置并试抓这个接口…', 'info');
      save().then(function (task) {
        if (!task) return;
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/preview',
          { limit: 5, endpoint_id: epId })
          .then(function (res) { showPreviewModal(U().apiItems(res)); }).catch(function () { });
      });
    }

    /** 把这个接口的分页配置套到所有接口（只改表单，点「保存配置」才生效） */
    function applyPagToAll(epId) {
      var eps = (state.task && state.task.endpoints) || [];
      if (eps.length < 2) { toast('只有一个接口，不用套用', 'info'); return; }
      var keys = ['pmode', 'pparam', 'pin', 'psize', 'psizeparam', 'pmax', 'pstart', 'ptotal', 'pmore', 'pnext'];
      var src = {};
      keys.forEach(function (k) {
        var el = root.querySelector('#ep_' + k + '_' + epId);
        src[k] = el ? el.value : '';
      });
      var srcShortEl = root.querySelector('[data-ep-short="' + epId + '"]');
      var shortOn = !!(srcShortEl && srcShortEl.checked);
      var modeText = (root.querySelector('#ep_pmode_' + epId) || {}).value || 'none';
      U().confirmDialog({
        title: '分页配置套用全部',
        message: '把这个接口的分页配置（翻页方式 ' + modeText + '）复制到其余 ' + (eps.length - 1) + ' 个接口？\n只改表单，点「保存配置」才生效。',
        confirmText: '套用'
      }).then(function (ok) {
        if (!ok) return;
        var n = 0;
        eps.forEach(function (ep) {
          if (ep.id === epId) return;
          keys.forEach(function (k) {
            var el = root.querySelector('#ep_' + k + '_' + ep.id);
            if (el) el.value = src[k];
          });
          var sh = root.querySelector('[data-ep-short="' + ep.id + '"]');
          if (sh) sh.checked = shortOn;
          n++;
        });
        toast('分页配置已套用到 ' + n + ' 个接口，记得点「保存配置」', 'success');
      });
    }

    /** 把这个接口的请求头套到所有接口 */
    function applyHeadersToAll(epId) {
      var eps = (state.task && state.task.endpoints) || [];
      if (eps.length < 2) { toast('只有一个接口，不用套用', 'info'); return; }
      var srcEl = root.querySelector('#ep_headers_' + epId);
      var text = srcEl ? srcEl.value : '';
      if (!String(text).trim()) { toast('这个接口的请求头是空的', 'error'); return; }
      U().confirmDialog({
        title: '请求头套用全部',
        message: '把这个接口的请求头复制到其余 ' + (eps.length - 1) + ' 个接口？\n只改表单，点「保存配置」才生效。',
        confirmText: '套用'
      }).then(function (ok) {
        if (!ok) return;
        var n = 0;
        eps.forEach(function (ep) {
          if (ep.id === epId) return;
          var el = root.querySelector('#ep_headers_' + ep.id);
          if (el) { el.value = text; n++; }
        });
        toast('请求头已复制到 ' + n + ' 个接口，记得点「保存配置」', 'success');
      });
    }

    /** 现在跑一遍前置登录：先保存配置（服务端读的是保存后的任务），再执行登录流程并导出会话 */
    function runPrelogin() {
      var hint = root.querySelector('#pl_result');
      if (hint) hint.textContent = '正在保存配置…';
      save().then(function (task) {
        // 保存会重绘这块表单，元素要重新取
        var out = root.querySelector('#pl_result');
        if (!task) { if (out) out.textContent = '保存失败，先修配置'; return; }
        if (!task.prelogin || !task.prelogin.enabled) {
          if (out) out.textContent = '前置登录没打开（勾上开关并选好录制）';
          return;
        }
        if (out) out.textContent = '正在执行登录流程…';
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/prelogin', {})
          .then(function (res) {
            var ok = !!res.ok;
            toast(ok ? '登录流程执行完成' : ('登录失败：' + (res.error || '未知错误')), ok ? 'success' : 'error');
            out = root.querySelector('#pl_result');
            if (!out) return;
            var bits = [];
            bits.push((ok ? '✔ 登录 ' : '✘ 登录 ') + (res.elapsed_ms || 0) + ' ms');
            if (typeof res.cookie_count === 'number') bits.push('Cookie ' + res.cookie_count + ' 个');
            if (res.session_error) bits.push('会话导出失败：' + res.session_error);
            if (res.warning) bits.push('⚠ ' + res.warning);
            if (res.error) bits.push(res.error);
            if (res.run_id) bits.push('运行记录 ' + res.run_id);
            out.textContent = bits.join(' · ');
          }).catch(function () { });
      });
    }

    function save() {
      var cfg = readConfigForm(root, 'tk');
      var splitEl = root.querySelector('#tk_split');
      var payload = {
        name: val(root, 'tk_name').trim(),
        description: val(root, 'tk_desc'),
        start_url: val(root, 'tk_start').trim(),
        endpoints: collectEndpoints(),
        schedule: cfg.schedule,
        throttle: cfg.throttle,
        sink: cfg.sink,
        session: cfg.session,
        prelogin: cfg.prelogin,
        common: cfg.common,
        split_by_endpoint: !!(splitEl && splitEl.checked),
        sink_profile_id: val(root, 'tk_profile')
      };
      return api('PATCH', '/api/collect/tasks/' + encodeURIComponent(state.id), payload).then(function (task) {
        toast('已保存', 'success');
        state.task = task;
        paint(task);
        return task;
      }).catch(function () { return null; });
    }

    function loadRuns() {
      var host = root.querySelector('#task-runs');
      api('GET', '/api/collect/runs?task_id=' + encodeURIComponent(state.id) + '&limit=20', undefined, { silent: true })
        .then(function (res) {
          var items = U().apiItems(res);
          if (!items.length) {
            host.innerHTML = '<div class="card"><div class="card-head"><h2>最近运行</h2></div>' +
              '<div class="card-body muted">还没有运行记录，点右上「立即运行」跑一次。</div></div>';
            return;
          }
          host.innerHTML = '<div class="card"><div class="card-head"><h2>最近运行</h2></div><div class="card-body">' +
            '<div class="table-wrap"><table class="table"><thead><tr><th>开始时间</th><th>触发</th><th>结果</th>' +
            '<th class="num">请求</th><th class="num">解析</th><th class="num">写入</th><th class="actions-cell">操作</th></tr></thead><tbody>' +
            items.map(function (r) {
              return '<tr><td>' + esc(fmtDateTime(r.started_at)) + '</td>' +
                '<td>' + esc(r.trigger === 'schedule' ? '定时' : '手动') + '</td>' +
                '<td>' + runStatusBadge(r.status) + '</td>' +
                '<td class="num">' + esc(r.requests || 0) + '</td>' +
                '<td class="num">' + esc(r.fetched || 0) + '</td>' +
                '<td class="num">' + esc(r.written || 0) + '</td>' +
                '<td class="actions-cell"><button class="btn btn--sm" data-goto="#/collect/run/' + esc(r.id) + '">详情</button></td></tr>';
            }).join('') + '</tbody></table></div></div></div>';
        }).catch(function () { });
    }

    // 接口块：勾选 / 改名只改内存状态，保存时统一提交
    root.addEventListener('change', function (e) {
      var t = e.target;
      if (t && t.id === 'tk_profile') { applyProfile(t.value); return; }
      var eid = t && t.getAttribute && t.getAttribute('data-ep-enabled');
      if (eid) { var ep = findEp(eid); if (ep) ep.enabled = t.checked; return; }
      var sid = t && t.getAttribute && t.getAttribute('data-ep-short');
      if (sid) {
        var ep2 = findEp(sid);
        if (ep2) { ep2.pagination = normalizePag(ep2.pagination); ep2.pagination.stop_on_short = t.checked; }
      }
    });
    root.addEventListener('input', function (e) {
      var t = e.target;
      var eid = t && t.getAttribute && t.getAttribute('data-ep-name');
      if (eid) { var ep = findEp(eid); if (ep) ep.name = t.value; }
    });

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      var delEl = t.closest('[data-ep-del]');
      if (delEl) {
        var delId = delEl.getAttribute('data-ep-del');
        state.task.endpoints = (state.task.endpoints || []).filter(function (x) { return x.id !== delId; });
        paint(state.task);
        return;
      }
      var tgl = t.closest('[data-ep-toggle]');
      if (tgl) {
        // 只切 DOM，不重绘：避免把其它接口块里正在编辑的内容刷掉
        var tid = tgl.getAttribute('data-ep-toggle');
        var block = tgl.closest('.ep-block');
        if (block) {
          var open = !block.classList.contains('is-open');
          block.classList.toggle('is-open', open);
          tgl.textContent = open ? '▾' : '▸';
          EP_OPEN[tid] = open;
          if (open) {
            var first = block.querySelector('.ep-body input, .ep-body select');
            if (first && first.focus) first.focus();
          }
        }
        return;
      }
      var fillEl = t.closest('[data-ep-fill]');
      if (fillEl) { fillSample(fillEl.getAttribute('data-ep-fill'), fillEl.getAttribute('data-fill')); return; }
      var fEl = t.closest('[data-ep-fields]');
      if (fEl) { openFieldsModal(fEl.getAttribute('data-ep-fields')); return; }
      var dEl = t.closest('[data-ep-detect]');
      if (dEl) { detectPagination(dEl.getAttribute('data-ep-detect')); return; }
      var pgAll = t.closest('[data-ep-pgall]');
      if (pgAll) { applyPagToAll(pgAll.getAttribute('data-ep-pgall')); return; }
      var pvEl = t.closest('[data-ep-preview]');
      if (pvEl) { previewEndpoint(pvEl.getAttribute('data-ep-preview')); return; }
      var hdrAll = t.closest('[data-ep-hdrall]');
      if (hdrAll) { applyHeadersToAll(hdrAll.getAttribute('data-ep-hdrall')); return; }
      var actEl = t.closest('[data-act]');
      var act = actEl ? actEl.getAttribute('data-act') : '';
      if (act === 'save') { save(); return; }
      if (t.closest('[data-files-refresh]')) { loadFiles(state.task); return; }
      if (act === 'back') { go('#/collect/tasks'); return; }
      if (act === 'refresh') { load(); return; }
      if (act === 'run') { runTask(state.id); return; }
      if (act === 'del') { deleteTask(state.id, null); return; }
      if (act === 'test-sink') {
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/test-sink', {})
          .then(function (res) { toast(res.message || '落库目标可用', 'success'); }).catch(function () { });
        return;
      }
      if (act === 'selftest') { selftest(); return; }
      if (act === 'run-prelogin') { runPrelogin(); return; }
      if (act === 'preview') {
        toast('正在试抓（不写库）…', 'info');
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/preview', { limit: 5 })
          .then(function (res) { showPreviewModal(U().apiItems(res)); }).catch(function () { });
        return;
      }
      var goto = t.closest('[data-goto]');
      if (goto) { go(goto.getAttribute('data-goto')); }
    });

    load();
  }

  // 试抓详情里的「key: value」表（请求头 / query / 变量通用）
  function pvKVHtml(obj, empty) {
    var keys = Object.keys(obj || {}).sort();
    if (!keys.length) return '<div class="muted">' + esc(empty || '（无）') + '</div>';
    return '<div class="table-wrap"><table class="pv-kv"><tbody>' + keys.map(function (k) {
      return '<tr><th>' + esc(k) + '</th><td>' + esc(obj[k]) + '</td></tr>';
    }).join('') + '</tbody></table></div>';
  }

  function pvDetailsHtml(title, inner) {
    return '<details class="pv-details"><summary>' + esc(title) + '</summary>' +
      '<div class="pv-details-body">' + inner + '</div></details>';
  }

  function pvPre(text) {
    return '<pre class="pv-pre">' + esc(text || '') + '</pre>';
  }

  function renderPreviewItem(p) {
    var req = p.request || {};
    var resp = p.response || {};
    var html = '<div class="ds-block">' +
      '<div class="ds-block-title"><span>' + esc(p.name || p.endpoint_id || '接口') + '</span>' +
      '<span class="op-group">' + statusChip(p.status) +
      (req.duration_ms || req.duration_ms === 0 ? '<span class="muted">' + esc(fmtMS(req.duration_ms)) + '</span>' : '') +
      (resp.size ? '<span class="muted">' + esc(fmtBytes(resp.size)) + '</span>' : '') +
      '</span></div>' +
      '<div class="pv-url">' + methodChip(req.method || 'GET') + ' ' + esc(p.url || req.url || '') + '</div>';

    if (p.session) html += '<div class="muted" style="margin-top:4px">登录态：' + esc(p.session) + '</div>';
    if (p.warning) html += '<div class="alert alert--warn" style="margin:6px 0">' + esc(p.warning) + '</div>';
    if (p.error) {
      html += '<div class="alert alert--warn" style="margin:6px 0">' + esc(p.error) +
        (p.hint ? '<br>' + esc(p.hint) : '') + '</div>';
    }

    if ((p.process || []).length) {
      html += '<div class="pv-section"><div class="pv-section-title">抓取过程</div><ol class="pv-steps">' +
        p.process.map(function (s) { return '<li>' + esc(s) + '</li>'; }).join('') + '</ol></div>';
    }

    var reqInner = '';
    if (req.query_params && Object.keys(req.query_params).length) {
      reqInner += '<div class="pv-sub-title">Query / 表单参数' +
        (req.retries ? ' · 重试 ' + esc(req.retries) + ' 次' : '') + '</div>' + pvKVHtml(req.query_params);
    }
    if (req.vars && Object.keys(req.vars).length) {
      reqInner += '<div class="pv-sub-title">展开后的变量</div>' + pvKVHtml(req.vars);
    }
    reqInner += '<div class="pv-sub-title">请求头（Cookie / Authorization 已打码）</div>' +
      pvKVHtml(req.headers, '（没有请求头）');
    if (req.body) reqInner += '<div class="pv-sub-title">请求体</div>' + pvPre(req.body);
    if (!reqInner) reqInner = '<div class="muted">没有记录到请求报文</div>';
    html += pvDetailsHtml('请求参数（' + (req.method || 'GET') + (req.body ? ' · 含请求体' : '') + '）', reqInner);

    var respInner = '<div class="pv-kv-line">' +
      '<span>状态 <b>' + esc(resp.status || 0) + (resp.status_text ? ' ' + esc(resp.status_text) : '') + '</b></span>' +
      '<span>大小 <b>' + esc(fmtBytes(resp.size)) + '</b></span>' +
      '<span>耗时 <b>' + esc(fmtMS(req.duration_ms)) + '</b></span>' +
      (resp.content_type ? '<span>类型 <b class="mono">' + esc(resp.content_type) + '</b></span>' : '') +
      '</div>' +
      '<div class="pv-sub-title">响应头（Set-Cookie 已打码）</div>' + pvKVHtml(resp.headers, '（没有响应头）');
    if (resp.body_preview) {
      respInner += '<div class="pv-sub-title">响应体预览（前 2000 字节，敏感字段已打码）</div>' + pvPre(resp.body_preview);
    }
    html += pvDetailsHtml('响应报文', respInner);

    if (!p.error) {
      html += '<div class="pv-section"><div class="pv-section-title">解析结果</div>' +
        '<div class="muted">分页：' + esc(p.pagination || '不分页') +
        (p.total ? ' · 响应 total=' + esc(p.total) + '，预计 ' + esc(p.estimated_pages || '?') + ' 页' : '') + '</div>' +
        '<div class="muted">记录路径 <code>' + esc(p.records_path || '(响应本身)') + '</code> · 本页 ' +
        esc(p.records || 0) + ' 条 · 列 ' + esc((p.columns || []).join(', ')) + '</div>' +
        (p.hint ? '<div class="alert alert--warn" style="margin-top:6px">' + esc(p.hint) + '</div>' : '') +
        tablePreviewHtml(p.rows || []) + '</div>';
    }
    return html + '</div>';
  }

  // 复制用的纯文本明细（含完整请求/响应，敏感值已由后端打码）
  function previewPlainText(items) {
    var lines = [];
    items.forEach(function (p) {
      var req = p.request || {};
      var resp = p.response || {};
      lines.push('===== ' + (p.name || p.endpoint_id || '接口') + ' =====');
      if (p.session) lines.push('登录态: ' + p.session);
      if (p.warning) lines.push('[提示] ' + p.warning);
      if (p.error) lines.push('[错误] ' + p.error + (p.hint ? ' / ' + p.hint : ''));
      (p.process || []).forEach(function (s, i) { lines.push((i + 1) + '. ' + s); });
      lines.push('请求: ' + (req.method || 'GET') + ' ' + (p.url || req.url || ''));
      if (req.headers) {
        lines.push('请求头:');
        Object.keys(req.headers).sort().forEach(function (k) { lines.push('  ' + k + ': ' + req.headers[k]); });
      }
      if (req.body) lines.push('请求体: ' + req.body);
      lines.push('响应: ' + (resp.status || 0) + (resp.status_text ? ' ' + resp.status_text : '') +
        ' · ' + (resp.size || 0) + ' 字节');
      if (resp.headers) {
        lines.push('响应头:');
        Object.keys(resp.headers).sort().forEach(function (k) { lines.push('  ' + k + ': ' + resp.headers[k]); });
      }
      if (resp.body_preview) lines.push('响应体预览: ' + resp.body_preview);
      lines.push('');
    });
    return lines.join('\n');
  }

  function copyPreview(items) {
    var text = previewPlainText(items || []);
    var done = function () { toast('试抓明细已复制', 'success'); };
    var fallback = function () {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); done(); } catch (e) { toast('复制失败，请手动选择文本', 'error'); }
      if (ta.parentNode) ta.parentNode.removeChild(ta);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, fallback);
      return;
    }
    fallback();
  }

  function showPreviewModal(items) {
    items = items || [];
    var intro = items.length
      ? '<div class="muted" style="margin-bottom:10px">下面是本次试抓实际发出去的请求与收到的响应（Cookie / Authorization / password 已打码，可直接复制给同事）。</div>'
      : '';
    var body = intro + (items.map(renderPreviewItem).join('') || '<div class="muted">没有结果</div>');
    var m = U().openModal({
      title: '试抓预览（不会写入数据库）',
      width: 1100,
      body: body,
      footer: '<button class="btn" data-modal-close>关闭</button>' +
        '<button class="btn btn--primary" data-copy-preview>复制完整明细</button>'
    });
    if (m && m.overlay) {
      m.overlay.addEventListener('click', function (e) {
        var t = e.target && e.target.nodeType === 1 ? e.target : null;
        if (t && t.closest('[data-copy-preview]')) copyPreview(items);
      });
    }
  }

  /* ======================================================================
   * 页面 4：#/collect/run/{id} —— 一次采集的详情
   * ====================================================================== */

  function renderRun(app, id) {
    var root = pageRoot(app);
    var state = { id: id, run: null, es: null, timer: null, logs: [], httpOnly: false, sqlOnly: false };

    root.innerHTML = headHtml('执行日志 · 详情',
      '每次请求与响应都记在下面的日志里（明文，Cookie / Authorization 原样显示）；分页抓多页就有多组，方便确认参数与会话是否真的生效。',
      '<button class="btn btn--sm" data-act="back">返回执行日志</button>' +
      '<button class="btn btn--sm" data-act="back-task">回到任务</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--danger" data-act="cancel">取消运行</button>') +
      '<div id="run-body">' + U().loadingCard('加载运行…') + '</div>' +
      '<div class="card" style="margin-top:16px">' +
      '<div class="card-head"><h2>运行日志</h2>' +
      '<div class="card-tools">' +
      '<button class="btn btn--sm" data-act="log-toggle-parts">展开报文</button>' +
      '<label class="check-line"><input type="checkbox" id="log-http-only"> 只看请求/响应</label>' +
      '<label class="check-line"><input type="checkbox" id="log-sql-only"> 只看 SQL</label>' +
      '<span class="muted" id="log-count"></span>' +
      '</div></div>' +
      '<div class="card-body" style="padding:0"><div class="logs logs--panel" id="run-logs" style="max-height:520px;overflow:auto"></div></div></div>';

    function paint(run) {
      state.run = run;
      var host = root.querySelector('#run-body');
      var eps = run.endpoints || [];
      host.innerHTML = '<div class="card"><div class="card-head"><h2>' + esc(run.task_name || '采集任务') + ' ' +
        runStatusBadge(run.status) + '</h2><div class="card-tools"><span class="muted">' +
        esc(run.trigger === 'schedule' ? '定时触发' : '手动触发') + ' · ' + esc(fmtDateTime(run.started_at)) + '</span></div></div>' +
        '<div class="card-body">' +
        '<div class="stat-line">' +
        '<div><span>请求次数</span><b>' + esc(run.requests || 0) + '</b></div>' +
        '<div><span>解析记录</span><b>' + esc(run.fetched || 0) + '</b></div>' +
        '<div><span>写入行数</span><b>' + esc(run.written || 0) + '</b></div>' +
        '<div><span>耗时</span><b>' + esc(run.finished_at ? spanText(run.started_at, run.finished_at) : '进行中') + '</b></div>' +
        '</div>' +
        '<div class="muted" style="margin-bottom:10px">写入目标：' + esc(run.sink || '—') + '</div>' +
        (run.error ? '<div class="alert alert--error">' + esc(run.error) + '</div>' : '') +
        (eps.length ? '<div class="table-wrap"><table class="table"><thead><tr>' +
          '<th>接口</th><th style="width:70px">方法</th><th>URL</th><th style="width:64px">状态</th>' +
          '<th class="num">页数</th><th class="num">解析</th><th class="num">写入</th>' +
          '<th class="num">耗时</th><th>写入目标</th><th>问题</th></tr></thead><tbody>' +
          eps.map(function (e) {
            return '<tr><td>' + esc(e.name || e.endpoint_id) + '</td>' +
              '<td>' + methodChip(e.method) + '</td>' +
              '<td><div class="url-cell">' + esc(e.url) + '</div></td>' +
              '<td>' + statusChip(e.status) + '</td>' +
              '<td class="num nowrap">' + esc(e.pages || 0) +
              (e.stop_reason ? '<div class="cell-sub" title="' + esc(e.stop_reason) + '">停止原因</div>' : '') + '</td>' +
              '<td class="num">' + esc(e.fetched || 0) + '</td>' +
              '<td class="num">' + esc(e.written || 0) +
              (e.duplicates ? '<div class="cell-sub">去重 -' + esc(e.duplicates) + '</div>' : '') + '</td>' +
              '<td class="num">' + esc(fmtMS(e.duration_ms)) + '</td>' +
              '<td class="muted">' + esc(e.target || '—') + '</td>' +
              '<td class="cell-state' + (e.error ? ' cell-state--error' : '') + '">' +
              (e.error ? esc(e.error) + (e.hint ? '<div class="cell-sub">' + esc(e.hint) + '</div>' : '')
                : (e.stop_reason ? '<span class="muted">' + esc(e.stop_reason) + '</span>' : '<span class="muted">—</span>')) +
              '</td>' +
              '</tr>';
          }).join('') + '</tbody></table></div>' : '<div class="muted">还没有接口结果</div>') +
        '</div></div>';
      state.logs = (run.logs || []).slice();
      paintLogs();
    }

    function logVisible(l) {
      var cls = levelClass(l.level);
      if (state.httpOnly && !state.sqlOnly) return cls === 'http';
      if (state.sqlOnly && !state.httpOnly) return cls === 'sql';
      if (state.sqlOnly && state.httpOnly) return cls === 'sql' || cls === 'http';
      return true;
    }

    function logLineHtml(l) {
      var cls = levelClass(l.level);
      var ts = '<span class="log-ts">' + esc(clock(l.ts)) + '</span>';
      var lv = '<span class="log-lv">' + esc((l.level || 'info').toUpperCase()) + '</span>';
      if (cls === 'http') {
        // 报文单独排版：一行一个请求头、报文体折叠，长 JWT / 大 JSON 不再糊成一团
        return '<div class="logline logline--http">' + ts + lv +
          '<div class="log-msg">' + httpLineHtml(l.message) + '</div></div>';
      }
      return '<div class="logline logline--' + cls + '">' + ts + lv +
        '<span class="log-msg">' + esc(l.message) + '</span></div>';
    }

    /** HTTP 报文行：首行方法/URL（或状态码），请求头逐项列出，请求体 / 响应体折叠 */
    function httpLineHtml(message) {
      var msg = String(message || '');
      var nl = msg.indexOf('\n');
      var head = nl >= 0 ? msg.slice(0, nl) : msg;
      var rest = nl >= 0 ? msg.slice(nl + 1) : '';

      if (head.charAt(0) === '→') {
        var m = head.slice(1).match(/^\s*(\S+)\s*([\s\S]*)$/) || [];
        var method = m[1] || '';
        var tail = (m[2] || '').trim();
        var seq = '';
        var si = tail.lastIndexOf('　#');
        if (si >= 0) { seq = tail.slice(si + 1); tail = tail.slice(0, si); }
        return '<div class="http-head">' +
          '<span class="http-arrow">→</span>' +
          '<span class="http-method">' + esc(method) + '</span>' +
          '<span class="http-url">' + esc(tail) + '</span>' +
          (seq ? '<span class="http-seq">' + esc(seq) + '</span>' : '') +
          '</div>' + httpPartsHtml(rest);
      }

      if (head.charAt(0) === '←') {
        var r = head.slice(1).match(/^\s*(\d+)[\s　]*([\s\S]*)$/) || [];
        var status = r[1] || '';
        var code = parseInt(status, 10) || 0;
        var tone = code >= 200 && code < 300 ? 'is-ok'
          : (code >= 500 ? 'is-err' : (code >= 400 ? 'is-warn' : ''));
        return '<div class="http-head">' +
          '<span class="http-arrow">←</span>' +
          '<span class="http-status ' + tone + '">' + esc(status) + '</span>' +
          '<span class="http-size">' + esc((r[2] || '').trim()) + '</span>' +
          '</div>' + (rest.trim() ? bodyDetails('响应体', rest.trim()) : '');
      }

      return '<div class="http-url">' + esc(head) + '</div>' +
        (rest.trim() ? '<pre class="http-body">' + esc(rest.trim()) + '</pre>' : '');
    }

    /** 把「请求头 … / 请求体 …」拆成两块可折叠内容 */
    function httpPartsHtml(rest) {
      rest = rest || '';
      var hIdx = rest.indexOf('请求头  ');
      var bIdx = rest.indexOf('请求体  ');
      if (hIdx < 0 && bIdx < 0) {
        return rest.trim() ? '<pre class="http-body">' + esc(rest.trim()) + '</pre>' : '';
      }
      var out = '';
      if (hIdx >= 0) {
        var hEnd = bIdx > hIdx ? bIdx : rest.length;
        out += headersDetails(rest.slice(hIdx + 5, hEnd).trim());
      }
      if (bIdx >= 0) {
        var body = rest.slice(bIdx + 5).trim();
        if (body) out += bodyDetails('请求体', body);
      }
      return out;
    }

    function headersDetails(raw) {
      if (!raw) return '';
      var items = raw.split(' | ');
      var rows = [];
      var hint = '';
      items.forEach(function (it) {
        var i = it.indexOf(': ');
        var k = i > 0 ? it.slice(0, i) : it;
        var v = i > 0 ? it.slice(i + 2) : '';
        rows.push('<tr><td class="k">' + esc(k) + '</td><td class="v">' + esc(v) + '</td></tr>');
        // 摘要里带一眼关键凭证：不用展开也能看出 Authorization/Cookie 带没带
        if (!hint && /^(authorization|cookie|x-token|x-auth-token)$/i.test(k.trim())) {
          var short = v.length > 42 ? v.slice(0, 42) + '…' : v;
          hint = ' · ' + k.trim() + ': ' + short;
        }
      });
      return '<details class="http-part"><summary>请求头（' + items.length + '）' + esc(hint) + '</summary>' +
        '<table class="http-kv"><tbody>' + rows.join('') + '</tbody></table></details>';
    }

    function bodyDetails(label, text) {
      return '<details class="http-part"><summary>' + esc(label) + '（' + esc(fmtBytes(utf8Len(text))) + '）</summary>' +
        '<pre class="http-body">' + esc(prettyBody(text)) + '</pre></details>';
    }

    /** 看着像 JSON 就格式化（超过 20KB 跳过，避免拖慢渲染） */
    function prettyBody(text) {
      var s = String(text || '');
      if (s.length > 20000) return s;
      var t = s.trim();
      if (t.charAt(0) !== '{' && t.charAt(0) !== '[') return s;
      try { return JSON.stringify(JSON.parse(t), null, 2); } catch (e) { return s; }
    }

    function utf8Len(s) {
      var n = 0;
      for (var i = 0; i < s.length; i++) {
        var c = s.charCodeAt(i);
        if (c < 0x80) n += 1;
        else if (c < 0x800) n += 2;
        else if (c >= 0xd800 && c <= 0xdbff) { n += 4; i++; }
        else n += 3;
      }
      return n;
    }

    function updateLogCount() {
      var el = root.querySelector('#log-count');
      if (!el) return;
      var http = 0;
      for (var i = 0; i < state.logs.length; i++) {
        if (levelClass(state.logs[i].level) === 'http') http++;
      }
      el.textContent = '共 ' + state.logs.length + ' 行 · 请求/响应 ' + http + ' 行';
    }

    function paintLogs() {
      var host = root.querySelector('#run-logs');
      if (!host) return;
      var list = state.logs.filter(logVisible);
      host.innerHTML = list.length ? list.map(logLineHtml).join('')
        : '<div style="padding:12px;color:#9fb0c7">' +
        (state.httpOnly ? '还没有请求/响应日志'
          : (state.sqlOnly ? '还没有 SQL 日志（落库选 Excel / JSON / JSONL 时不会产生 SQL）' : '暂无日志')) +
        '</div>';
      host.scrollTop = host.scrollHeight;
      updateLogCount();
    }

    function appendLog(l) {
      state.logs.push(l);
      var host = root.querySelector('#run-logs');
      if (host && logVisible(l)) {
        host.insertAdjacentHTML('beforeend', logLineHtml(l));
        host.scrollTop = host.scrollHeight;
      }
      updateLogCount();
    }

    /** 一键展开 / 折叠所有报文块（请求头、请求体、响应体） */
    function toggleParts() {
      var host = root.querySelector('#run-logs');
      if (!host) return;
      var parts = Array.prototype.slice.call(host.querySelectorAll('.http-part'));
      if (!parts.length) return;
      var anyClosed = parts.some(function (d) { return !d.open; });
      parts.forEach(function (d) { d.open = anyClosed; });
      var btn = root.querySelector('[data-act="log-toggle-parts"]');
      if (btn) btn.textContent = anyClosed ? '折叠报文' : '展开报文';
    }

    function clock(ts) {
      return U().fmtClock ? U().fmtClock(ts) : String(ts || '');
    }

    function levelClass(l) {
      return U().levelClass ? U().levelClass(l) : 'info';
    }

    function load() {
      api('GET', '/api/collect/runs/' + encodeURIComponent(state.id)).then(function (run) {
        paint(run);
        if (run.status === 'running' || run.status === 'queued') startStream();
      }).catch(function (err) {
        root.querySelector('#run-body').innerHTML = U().errorCard(err.message || '运行记录不存在');
      });
    }

    function startStream() {
      if (state.es || !window.EventSource) return;
      var es = new EventSource('/api/collect/runs/' + encodeURIComponent(state.id) + '/events');
      state.es = es;
      es.onmessage = function (ev) {
        var data;
        try { data = JSON.parse(ev.data); } catch (e) { return; }
        if (data.type === 'log' && data.log) {
          appendLog(data.log);
        } else if (data.type === 'status' || data.type === 'end') {
          stopStream();
          api('GET', '/api/collect/runs/' + encodeURIComponent(state.id)).then(paint).catch(function () { });
        }
      };
      es.onerror = function () { stopStream(); };
    }

    function stopStream() {
      if (state.es) { try { state.es.close(); } catch (e) { } state.es = null; }
    }

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      var actEl = t.closest('[data-act]');
      var act = actEl ? actEl.getAttribute('data-act') : '';
      if (act === 'back') { go('#/collect/runs'); return; }
      if (act === 'back-task') {
        var tid = state.run && state.run.task_id;
        if (tid) go('#/collect/task/' + encodeURIComponent(tid));
        else toast('这次运行没有关联任务', 'error');
        return;
      }
      if (act === 'log-toggle-parts') { toggleParts(); return; }
      if (act === 'refresh') { load(); return; }
      if (act === 'cancel') {
        api('POST', '/api/collect/runs/' + encodeURIComponent(state.id) + '/cancel', {})
          .then(function () { toast('已发送取消', 'success'); load(); }).catch(function () { });
      }
    });

    // 日志过滤：只看请求/响应、只看 SQL（分页跑很多页时先把业务日志滤掉）
    var httpBox = root.querySelector('#log-http-only');
    if (httpBox) {
      httpBox.addEventListener('change', function () {
        state.httpOnly = !!httpBox.checked;
        paintLogs();
      });
    }
    var sqlBox = root.querySelector('#log-sql-only');
    if (sqlBox) {
      sqlBox.addEventListener('change', function () {
        state.sqlOnly = !!sqlBox.checked;
        paintLogs();
      });
    }

    load();
    // 轮询兜底：SSE 断掉时也能看到进度
    state.timer = setInterval(function () {
      if (!document.body.contains(root)) return;
      if (!state.run || (state.run.status !== 'running' && state.run.status !== 'queued')) return;
      api('GET', '/api/collect/runs/' + encodeURIComponent(state.id), undefined, { silent: true })
        .then(paint).catch(function () { });
    }, 3000);
    onCleanup(function () {
      stopStream();
      clearInterval(state.timer);
    });
  }

  /* ------------------------------------------------------------ 对外 */

  window.UITestCollect = {
    renderCaptures: renderCaptures,
    renderTasks: renderTasks,
    renderFiles: renderFiles,
    renderSinks: renderSinks,
    renderRuns: renderRuns,
    renderCapture: renderCapture,
    renderTask: renderTask,
    renderRun: renderRun
  };
})();
