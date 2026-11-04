/* ==========================================================================
 * 接口采集控制台
 * --------------------------------------------------------------------------
 * 流程：录制（内置浏览器旁路记录真实请求）→ 筛选 → 建采集任务（定时/限速/落库）
 *       → 定时抓取 → 写入数据库或文件（SQLite / MySQL / PostgreSQL / Excel / JSON / JSONL）
 *
 * 说明：
 *   - 依赖 app.js 暴露的 window.UITest（api / toast / esc / 弹窗 / 分页等），
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
    return '<div class="page-head"><h1>' + esc(title) + '</h1>' +
      (sub ? '<div class="sub">' + sub + '</div>' : '') +
      (actions ? '<div class="head-actions" style="margin-top:10px">' + actions + '</div>' : '') +
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

  function selectHtml(id, options, value) {
    var html = '<select class="input" id="' + id + '">';
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

  function sessionOptions() {
    if (Opt.data && Opt.data.session_modes) {
      return Opt.data.session_modes.map(function (d) { return [d.value, d.label]; });
    }
    return [['browser', '复用内置浏览器登录态'], ['cookie', '手工 Cookie'], ['none', '无会话']];
  }

  // 定时 / 限速 / 落库 / 会话 四段配置（建任务弹窗与任务详情共用）
  function configFormHtml(pfx, cfg) {
    cfg = cfg || {};
    var sch = cfg.schedule || { mode: 'interval', every_seconds: 300, jitter_percent: 20 };
    var th = cfg.throttle || {};
    var sink = cfg.sink || { driver: 'sqlite', table: 'harvest_records', mode: 'append', batch_size: 200 };
    var sess = cfg.session || { mode: 'browser' };
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
      '<div class="ds-block"><div class="ds-block-title">写入数据库</div><div class="form-grid">' +
      fld('数据库类型', selectHtml(pfx + '_driver', driverOptions(), sink.driver)) +
      fld('连接串 DSN', inputHtml(pfx + '_dsn', sink.dsn || '', 'placeholder="SQLite 可留空"')) +
      fld('表名 / 文件名', inputHtml(pfx + '_table', sink.table || 'harvest_records')) +
      fld('写入模式', selectHtml(pfx + '_sinkmode', [['append', '追加'], ['upsert', '按主键覆盖']], sink.mode || 'append')) +
      fld('去重键列（upsert 用）', inputHtml(pfx + '_key', sink.key_column || '', 'placeholder="如 id"')) +
      fld('批量提交行数', numHtml(pfx + '_batch', sink.batch_size || 200)) +
      '</div></div>' +
      '<div class="ds-block"><div class="ds-block-title">会话（登录态）</div><div class="form-grid">' +
      fld('来源', selectHtml(pfx + '_smode', sessionOptions(), sess.mode || 'browser')) +
      fld('站点（browser 用）', inputHtml(pfx + '_host', sess.host || '', 'placeholder="https://example.com"')) +
      fld('User-Agent', inputHtml(pfx + '_ua', sess.user_agent || '', 'placeholder="留空 = 录制时的 UA"')) +
      fld('Referer', inputHtml(pfx + '_referer', sess.referer || '', 'placeholder="留空 = 录制页面地址"')) +
      '</div>' +
      fld('Cookie（cookie 模式，形如 a=1; b=2）', textareaHtml(pfx + '_cookie', sess.cookie || '', 2)) +
      '</div>';
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
      session: {
        mode: val(root, pfx + '_smode') || 'none',
        host: val(root, pfx + '_host'),
        user_agent: val(root, pfx + '_ua'),
        referer: val(root, pfx + '_referer'),
        cookie: val(root, pfx + '_cookie')
      }
    };
  }

  // 分页配置的空值兜底（后端会再 Normalize 一次，这里只负责显示不炸）
  function normalizePag(p) {
    p = p || {};
    return {
      mode: p.mode || 'none',
      param: p.param || '',
      in: p.in || 'query',
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
   * 页面 1：#/collect —— 录制入口 + 任务列表
   * ====================================================================== */

  function renderList(app) {
    var root = pageRoot(app);
    var st = { lastLive: null };

    root.innerHTML = headHtml('接口采集',
      '在内置浏览器或你自己的浏览器（插件）里正常操作页面 → 录下真实请求 → 勾选要抓的接口 → 设定时间 → 自动抓取写入数据库。' +
      '采集默认单任务串行、带随机间隔抖动、遇到 401/403/429 立即停手，不对目标站点造成压力。',
      '<button class="btn btn--sm" id="cap-refresh">刷新</button>' +
      '<button class="btn btn--sm btn--danger" id="cap-clear">清空数据</button>') +
      '<div class="card">' +
      '<div class="card-head"><h2>录制接口</h2>' +
      '<div class="card-tools"><span class="muted" id="cap-summary">加载中…</span></div></div>' +
      '<div class="card-body">' +
      // 下面三块各自独立重绘：表单区只在"录制状态切换"时重建，避免把正在输入的内容刷掉
      '<div id="cap-live"></div>' +
      '<div id="cap-form">' + captureFormHtml() + '</div>' +
      '<div id="cap-sessions"></div>' +
      '</div></div>' +
      '<div id="task-card" style="margin-top:16px">' + U().loadingCard('加载采集任务…') + '</div>' +
      '<div id="files-card" style="margin-top:16px"></div>';

    loadOptions();
    refreshCaptures(root, st, true);
    loadTasks(root);
    renderFilesCard(root.querySelector('#files-card'), '');

    root.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('#cap-refresh')) { refreshCaptures(root, st, true); loadTasks(root); renderFilesCard(root.querySelector('#files-card'), ''); return; }
      if (t.closest('#cap-clear')) { openClearModal(root, st); return; }
      if (t.closest('[data-files-refresh]')) { renderFilesCard(root.querySelector('#files-card'), ''); return; }
      if (t.closest('#cap-start')) { startCapture(root); return; }
      if (t.closest('#cap-stop')) { stopCapture(root, t.closest('#cap-stop').getAttribute('data-id')); return; }
      if (t.closest('#cap-open')) { go('#/collect/capture/' + encodeURIComponent(t.closest('#cap-open').getAttribute('data-id'))); return; }
      if (t.closest('#cap-del')) { deleteCapture(root, t.closest('#cap-del').getAttribute('data-id')); return; }
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

  var START_URL_KEY = 'uitest.collect.startUrl';

  function captureFormHtml() {
    return '<div class="form-grid">' +
      fld('起始页地址', inputHtml('cap_url', startUrlPref(), 'placeholder="https://你的系统/列表页"')) +
      fld('录制范围', selectHtml('cap_types', [['xhr', 'XHR + Fetch（只要接口）'], ['all', '含页面文档']], 'xhr')) +
      fld('响应体', selectHtml('cap_bodies', [['1', '抓（推荐，用于字段识别）'], ['0', '不抓（只看请求）']], '1')) +
      '</div>' +
      '<div class="ds-actions"><button class="btn btn--primary" id="cap-start">开始录制</button>' +
      '<span class="field-hint" style="margin-left:10px">会打开一个内置浏览器标签页，你在里面正常登录 / 点列表 / 翻页即可；' +
      '也可以改用浏览器插件在你自己的浏览器里录（插件弹窗里勾「同时录制接口请求」）。</span></div>';
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
            '<td>' + (c.source === 'extension' ? '<span class="chip">浏览器插件</span>' : '<span class="chip">内置浏览器</span>') + '</td>' +
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
    api('POST', '/api/collect/captures', {
      start_url: url, types: types, capture_bodies: bodies
    }).then(function (res) {
      toast('已开始录制，请在内置浏览器里正常操作', 'success');
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
          else go('#/collect');
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
        '<div class="card-tools"><button class="btn btn--sm" id="cap-refresh">刷新</button></div></div><div class="card-body">' +
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
        if (app) { app.innerHTML = ''; renderList(app); }
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
      filter: { q: '', method: '', type: '', kind: '', with_body: false, json: false, include: '', exclude: '', host: '' },
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
      '<div class="sel-bar"><div class="grow" id="sel-info">已选 0 个接口</div>' +
      '<button class="btn btn--sm" data-act="sel-all">全选当前筛选结果</button>' +
      '<button class="btn btn--sm" data-act="sel-none">清空选择</button>' +
      '<button class="btn btn--sm btn--danger" data-act="del-sel" disabled>删除选中</button>' +
      '<button class="btn btn--primary" data-act="next" disabled>下一步：配置采集任务（定时 + 落库）</button></div>' +
      '</div></div>';

    function filterQuery() {
      var f = state.filter;
      var q = [];
      if (f.q) q.push('q=' + encodeURIComponent(f.q));
      if (f.method) q.push('method=' + encodeURIComponent(f.method));
      if (f.type) q.push('type=' + encodeURIComponent(f.type));
      if (f.kind) q.push('kind=' + encodeURIComponent(f.kind));
      if (f.host) q.push('host=' + encodeURIComponent(f.host));
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
      if (meta && meta.started_at) parts.push('开始于 ' + esc(fmtDateTime(meta.started_at)));
      parts.push(meta && meta.stopped ? '已停止录制' : '<span style="color:#4f46e5">录制中…</span>');
      if (facets) {
        parts.push('共 ' + facets.total + ' 条，筛选后 ' + facets.kept + ' 条');
        var methods = Object.keys(facets.by_method || {}).map(function (k) { return k + ' ' + facets.by_method[k]; });
        if (methods.length) parts.push(methods.join(' / '));
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
          return '<tr data-row="' + esc(it.id) + '"' + (picked ? ' class="is-selected"' : '') + '>' +
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
      if (act === 'back') { go('#/collect'); return; }
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
          session: cfg.session
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
    var state = { id: id, task: null };

    root.innerHTML = headHtml('采集任务', '定时、限速、落库、登录态都在这里调；改完点「保存」。',
      '<button class="btn btn--sm" data-act="back">返回列表</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--primary" data-act="run">立即运行</button>' +
      '<button class="btn btn--sm" data-act="preview">试抓预览</button>' +
      '<button class="btn btn--sm btn--ghost" data-act="test-sink">测试落库</button>' +
      '<button class="btn btn--sm btn--danger" data-act="del">删除任务</button>') +
      '<div id="task-body">' + U().loadingCard('加载任务…') + '</div>' +
      '<div id="task-files" style="margin-top:16px"></div>' +
      '<div id="task-runs" style="margin-top:16px"></div>';

    function load() {
      api('GET', '/api/collect/tasks/' + encodeURIComponent(state.id)).then(function (task) {
        state.task = task;
        paint(task);
        loadRuns();
        loadFiles(task);
      }).catch(function (err) {
        root.querySelector('#task-body').innerHTML = U().errorCard(err.message || '任务不存在');
      });
    }

    function paint(task) {
      var host = root.querySelector('#task-body');
      var eps = task.endpoints || [];
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
        '<div class="form-grid">' +
        fld('任务名称', inputHtml('tk_name', task.name)) +
        fld('说明', inputHtml('tk_desc', task.description || '')) +
        fld('起始页 / Referer', inputHtml('tk_start', task.start_url || '')) +
        '</div>' +
        '<div class="ds-block"><div class="ds-block-title">接口（' + eps.length + ' 个，勾选框控制是否参与采集）</div>' +
        '<div id="ep-list">' + eps.map(epBlockHtml).join('') + '</div>' +
        '</div>' +
        configFormHtml('tk', task) +
        '<div class="ds-actions"><button class="btn btn--primary" data-act="save">保存配置</button>' +
        '<span class="field-hint" style="margin-left:10px">保存后定时计划立即生效（改计划会重算下次触发时间）。' +
        '接口块点左侧 ▸ 可展开 / 收起。</span></div>' +
        '</div></div>';
    }

    /** 产出文件（只显示这个任务表名对应的文件） */
    function loadFiles(task) {
      var host = root.querySelector('#task-files');
      if (!host) return;
      var table = (task && task.sink && task.sink.table) || '';
      var driver = ((task && task.sink && task.sink.driver) || '').toLowerCase();
      if (driver === 'mysql' || driver === 'postgres' || driver === 'postgresql') {
        host.innerHTML = '';
        return;
      }
      renderFilesCard(host, table);
    }

    function findEp(eid) {
      var list = state.task.endpoints || [];
      for (var i = 0; i < list.length; i++) if (list[i].id === eid) return list[i];
      return null;
    }

    // 接口块默认折叠（只留标题行），需要改的时候点开——多接口任务不再一屏塞满
    function epBlockHtml(ep, i) {
      var fields = (ep.fields || []).map(function (f) {
        return '<span class="chip">' + esc(f.column) + ' ← ' + esc(f.path) + '</span>';
      }).join('') || '<span class="muted">未配置字段（运行时会按响应自动识别）</span>';
      var pg = normalizePag(ep.pagination);
      var opened = isEpOpen(ep.id, i);
      var summary = '<span class="ep-summary">' + esc(String(ep.method || 'GET').toUpperCase()) +
        ' · ' + esc(pagShort(pg)) +
        ' · ' + (ep.fields || []).length + ' 字段' +
        (ep.enabled === false ? ' · <span style="color:var(--red)">未启用</span>' : '') + '</span>';
      return '<div class="ep-block' + (opened ? ' is-open' : '') + '" data-ep="' + esc(ep.id) + '">' +
        '<div class="ep-head">' +
        '<button class="ep-chev" type="button" data-ep-toggle="' + esc(ep.id) + '" title="展开 / 收起">' +
        (opened ? '▾' : '▸') + '</button>' +
        '<span class="ep-idx">#' + (i + 1) + '</span>' +
        '<label class="check-line"><input type="checkbox" data-ep-enabled="' + esc(ep.id) + '"' +
        (ep.enabled !== false ? ' checked' : '') + '> 启用</label>' +
        '<input class="input" style="flex:1 1 160px" id="ep_name_' + esc(ep.id) + '" data-ep-name="' + esc(ep.id) + '" value="' + esc(ep.name) + '">' +
        summary +
        '<button class="btn btn--sm" data-ep-fields="' + esc(ep.id) + '">字段映射</button>' +
        '<button class="btn btn--sm btn--danger" data-ep-del="' + esc(ep.id) + '">删除</button>' +
        '</div>' +
        '<div class="ep-body">' +
        '<div class="form-grid">' +
        fld('方法', selectHtml('ep_method_' + ep.id, [['GET', 'GET'], ['POST', 'POST'], ['PUT', 'PUT'], ['PATCH', 'PATCH'], ['DELETE', 'DELETE']], ep.method)) +
        fld('URL（支持 {{page}} {{date}} {{timestamp}} {{env:NAME}}）', inputHtml('ep_url_' + ep.id, ep.url)) +
        fld('记录路径（数组位置）', inputHtml('ep_path_' + ep.id, ep.records_path || '', 'placeholder="data.list"')) +
        '</div>' +
        '<div class="ep-pager">' +
        '<div class="ep-pager-head"><b>分页</b><span class="muted" id="ep_phint_' + esc(ep.id) + '">' + esc(describePag(pg)) + '</span>' +
        '<button class="btn btn--sm" data-ep-detect="' + esc(ep.id) + '">自动识别分页</button></div>' +
        '<div class="form-grid">' +
        fld('翻页方式', selectHtml('ep_pmode_' + ep.id, [['auto', '自动（推荐）'], ['page', '页码 page=1,2,3'], ['offset', '偏移 offset=0,20,40'], ['cursor', '游标 cursor=xxx'], ['none', '不分页（只抓一次）']], pg.mode)) +
        fld('参数名', inputHtml('ep_pparam_' + ep.id, pg.param, 'placeholder="page / offset / cursor"')) +
        fld('参数位置', selectHtml('ep_pin_' + ep.id, [['query', 'URL 参数'], ['body', '请求体 JSON']], pg.in)) +
        fld('每页条数', numHtml('ep_psize_' + ep.id, pg.page_size, 'min="0" placeholder="自动"')) +
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
        '<div class="field"><label>字段映射（列名 ← 响应路径）</label><div class="chips">' + fields + '</div></div>' +
        '</div>' +
        '</div>';
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
          sample_status: ep.sample_status || 0,
          sample_body: ep.sample_body || '',
          created_from: ep.created_from || '',
          method: val(root, 'ep_method_' + ep.id) || ep.method || 'GET',
          url: (val(root, 'ep_url_' + ep.id) || ep.url || '').trim(),
          records_path: val(root, 'ep_path_' + ep.id).trim(),
          pagination: {
            mode: val(root, 'ep_pmode_' + ep.id) || 'none',
            param: val(root, 'ep_pparam_' + ep.id).trim(),
            in: val(root, 'ep_pin_' + ep.id) || 'query',
            start: numVal(root, 'ep_pstart_' + ep.id, 0),
            page_size: numVal(root, 'ep_psize_' + ep.id, 0),
            max_pages: numVal(root, 'ep_pmax_' + ep.id, 0),
            total_path: val(root, 'ep_ptotal_' + ep.id).trim(),
            has_more_path: val(root, 'ep_pmore_' + ep.id).trim(),
            next_path: val(root, 'ep_pnext_' + ep.id).trim(),
            stop_on_short: !!(ep.pagination && ep.pagination.stop_on_short !== false)
          },
          enabled: ep.enabled !== false,
          headers: ep.headers || null,
          body: ep.body || '',
          fields: ep.fields || []
        };
      });
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

    function save() {
      var cfg = readConfigForm(root, 'tk');
      var payload = {
        name: val(root, 'tk_name').trim(),
        description: val(root, 'tk_desc'),
        start_url: val(root, 'tk_start').trim(),
        endpoints: collectEndpoints(),
        schedule: cfg.schedule,
        throttle: cfg.throttle,
        sink: cfg.sink,
        session: cfg.session
      };
      api('PATCH', '/api/collect/tasks/' + encodeURIComponent(state.id), payload).then(function (task) {
        toast('已保存', 'success');
        state.task = task;
        paint(task);
        loadRuns();
        loadFiles(task);
      }).catch(function () { });
    }

    function loadRuns() {
      var host = root.querySelector('#task-runs');
      api('GET', '/api/collect/runs?task_id=' + encodeURIComponent(state.id) + '&limit=20', undefined, { silent: true })
        .then(function (res) {
          var items = U().apiItems(res);
          if (!items.length) { host.innerHTML = ''; return; }
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
      var fEl = t.closest('[data-ep-fields]');
      if (fEl) { openFieldsModal(fEl.getAttribute('data-ep-fields')); return; }
      var dEl = t.closest('[data-ep-detect]');
      if (dEl) { detectPagination(dEl.getAttribute('data-ep-detect')); return; }
      var actEl = t.closest('[data-act]');
      var act = actEl ? actEl.getAttribute('data-act') : '';
      if (act === 'save') { save(); return; }
      if (t.closest('[data-files-refresh]')) { loadFiles(state.task); return; }
      if (act === 'back') { go('#/collect'); return; }
      if (act === 'refresh') { load(); return; }
      if (act === 'run') { runTask(state.id); return; }
      if (act === 'del') { deleteTask(state.id, null); return; }
      if (act === 'test-sink') {
        api('POST', '/api/collect/tasks/' + encodeURIComponent(state.id) + '/test-sink', {})
          .then(function (res) { toast(res.message || '落库目标可用', 'success'); }).catch(function () { });
        return;
      }
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

  function showPreviewModal(items) {
    var body = items.map(function (p) {
      var html = '<div class="ds-block"><div class="ds-block-title">' + esc(p.name || p.endpoint_id || '接口') + ' ' +
        statusChip(p.status) + '</div>' +
        '<div class="muted" style="margin-bottom:6px">' + esc(p.url || '') + '</div>';
      if (p.error) {
        html += '<div class="alert alert--warn">' + esc(p.error) + (p.hint ? '<br>' + esc(p.hint) : '') + '</div>';
      } else {
        html += '<div class="muted" style="margin-bottom:4px">分页：' + esc(p.pagination || '不分页') +
          (p.total ? ' · 响应 total=' + esc(p.total) + '，预计 ' + esc(p.estimated_pages || '?') + ' 页' : '') + '</div>' +
          '<div class="muted" style="margin-bottom:6px">记录路径 <code>' + esc(p.records_path || '(响应本身)') +
          '</code> · 本页 ' + esc(p.records || 0) + ' 条 · 列 ' + esc((p.columns || []).join(', ')) + '</div>' +
          tablePreviewHtml(p.rows || []);
      }
      return html + '</div>';
    }).join('');
    U().openModal({ title: '试抓预览（不会写入数据库）', width: 900, body: body || '<div class="muted">没有结果</div>' });
  }

  /* ======================================================================
   * 页面 4：#/collect/run/{id} —— 一次采集的详情
   * ====================================================================== */

  function renderRun(app, id) {
    var root = pageRoot(app);
    var state = { id: id, run: null, es: null, timer: null };

    root.innerHTML = headHtml('采集运行', '抓了多少、写了多少、哪一步出错，都在这里。',
      '<button class="btn btn--sm" data-act="back">返回任务</button>' +
      '<button class="btn btn--sm" data-act="refresh">刷新</button>' +
      '<button class="btn btn--sm btn--danger" data-act="cancel">取消运行</button>') +
      '<div id="run-body">' + U().loadingCard('加载运行…') + '</div>' +
      '<div class="card" style="margin-top:16px"><div class="card-head"><h2>实时日志</h2></div>' +
      '<div class="card-body"><div class="logs" id="run-logs" style="max-height:360px;overflow:auto"></div></div></div>';

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
          '<th class="num">耗时</th><th>问题</th></tr></thead><tbody>' +
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
              '<td class="cell-state' + (e.error ? ' cell-state--error' : '') + '">' +
              (e.error ? esc(e.error) + (e.hint ? '<div class="cell-sub">' + esc(e.hint) + '</div>' : '')
                : (e.stop_reason ? '<span class="muted">' + esc(e.stop_reason) + '</span>' : '<span class="muted">—</span>')) +
              '</td></tr>';
          }).join('') + '</tbody></table></div>' : '<div class="muted">还没有接口结果</div>') +
        '</div></div>';
      paintLogs(run.logs || []);
    }

    function paintLogs(logs) {
      var host = root.querySelector('#run-logs');
      if (!host) return;
      host.innerHTML = logs.map(function (l) {
        return '<div class="logline logline--' + levelClass(l.level) + '">' +
          '<span class="log-ts">' + esc(clock(l.ts)) + '</span>' +
          '<span class="log-lv">' + esc((l.level || 'info').toUpperCase()) + '</span>' +
          '<span class="log-msg">' + esc(l.message) + '</span></div>';
      }).join('');
      host.scrollTop = host.scrollHeight;
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
          var host = root.querySelector('#run-logs');
          if (!host) return;
          host.insertAdjacentHTML('beforeend', '<div class="logline logline--' + levelClass(data.log.level) + '">' +
            '<span class="log-ts">' + esc(clock(data.log.ts)) + '</span>' +
            '<span class="log-lv">' + esc((data.log.level || 'info').toUpperCase()) + '</span>' +
            '<span class="log-msg">' + esc(data.log.message) + '</span></div>');
          host.scrollTop = host.scrollHeight;
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
      if (act === 'back') {
        var tid = state.run && state.run.task_id;
        go(tid ? '#/collect/task/' + encodeURIComponent(tid) : '#/collect');
        return;
      }
      if (act === 'refresh') { load(); return; }
      if (act === 'cancel') {
        api('POST', '/api/collect/runs/' + encodeURIComponent(state.id) + '/cancel', {})
          .then(function () { toast('已发送取消', 'success'); load(); }).catch(function () { });
      }
    });

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
    renderList: renderList,
    renderCapture: renderCapture,
    renderTask: renderTask,
    renderRun: renderRun
  };
})();
