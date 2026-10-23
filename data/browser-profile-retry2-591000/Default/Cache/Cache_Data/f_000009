/* ==========================================================================
 * UI 自动化测试平台 · 网页控制台（纯静态 / 无构建 / 无外部依赖）
 * --------------------------------------------------------------------------
 * 目录：
 *   1. 常量与文案映射
 *   2. 通用工具（转义 / 时间 / 格式化）
 *   3. 接口封装 api() + toast + 模态框 + 二次确认
 *   4. 顶部导航：健康轮询 / 内置浏览器控制
 *   5. hash 路由与页面清理（定时器 / SSE）
 *   6. 页面：录制列表 / 录制详情 / 运行列表 / 运行详情 / 元素库 / 设置
 *
 * 说明：所有接口均为同源相对路径 fetch('/api/...')，字段名严格按 docs/API.md（snake_case）。
 * ========================================================================== */

(function () {
  'use strict';

  /* ======================================================================
   * 1. 常量与文案映射
   * ====================================================================== */

  // 动作中文化
  var ACTION_LABEL = {
    navigate: '打开页面',
    click: '点击',
    input: '输入',
    select: '选择',
    check: '勾选',
    uncheck: '取消勾选',
    key: '按键',
    hover: '悬停',
    wait: '等待',
    upload: '上传',
    assert: '断言',
    script: '脚本'
  };

  // 运行状态
  var RUN_STATUS = {
    queued: { label: '排队中', cls: 'badge--queued' },
    running: { label: '执行中', cls: 'badge--running' },
    passed: { label: '通过', cls: 'badge--passed' },
    failed: { label: '失败', cls: 'badge--failed' },
    canceled: { label: '已取消', cls: 'badge--canceled' }
  };

  // 录制状态
  var REC_STATUS = {
    draft: { label: '草稿', cls: 'badge--draft' },
    ready: { label: '就绪', cls: 'badge--ready' }
  };

  // 数据来源
  var SOURCE_LABEL = { ai: 'AI 生成', rules: '内置规则', manual: '手动填写' };

  // 运行结束态
  var TERMINAL = { passed: 1, failed: 1, canceled: 1 };

  // 步骤行内可编辑字段
  var STEP_FIELDS = ['annotation', 'value', 'data_key', 'expect'];

  // 需要数据字段（data_key）的动作
  var DATA_ACTIONS = ['input', 'select', 'upload'];

  /* ======================================================================
   * 2. 通用工具
   * ====================================================================== */

  function $(sel, root) { return (root || document).querySelector(sel); }
  function $$(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

  var ESC_MAP = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  function esc(v) {
    return String(v === null || v === undefined ? '' : v).replace(/[&<>"']/g, function (c) { return ESC_MAP[c]; });
  }

  function pad2(n) { return n < 10 ? '0' + n : '' + n; }

  function toDate(v) {
    if (!v) return null;
    var d = new Date(v);
    return isNaN(d.getTime()) ? null : d;
  }

  // 2026-02-01 10:00:00
  function fmtDateTime(v) {
    var d = toDate(v);
    if (!d) return '—';
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' +
      pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }

  // 12:00:01.123（日志时间）
  function fmtClock(v) {
    var d = toDate(v) || new Date();
    var ms = String(d.getMilliseconds());
    while (ms.length < 3) ms = '0' + ms;
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()) + '.' + ms;
  }

  // 相对时间：3 分钟前
  function relTime(v) {
    var d = toDate(v);
    if (!d) return '—';
    var diff = Date.now() - d.getTime();
    if (diff < 0) return '刚刚';
    if (diff < 10000) return '刚刚';
    if (diff < 60000) return Math.floor(diff / 1000) + ' 秒前';
    if (diff < 3600000) return Math.floor(diff / 60000) + ' 分钟前';
    if (diff < 86400000) return Math.floor(diff / 3600000) + ' 小时前';
    if (diff < 604800000) return Math.floor(diff / 86400000) + ' 天前';
    return fmtDateTime(v).slice(0, 16);
  }

  // 本地时间字符串（备份列表用）
  function fmtTime(iso) {
    if (!iso) return '-';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  function fmtDuration(ms) {
    if (ms === null || ms === undefined || isNaN(ms)) return '—';
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
    var m = Math.floor(ms / 60000);
    var s = Math.round((ms % 60000) / 1000);
    return m + ' 分 ' + s + ' 秒';
  }

  // 运行耗时：优先 duration_ms，其次 started_at/finished_at 差值
  function runDuration(run) {
    if (!run) return '—';
    if (typeof run.duration_ms === 'number') return fmtDuration(run.duration_ms);
    var s = toDate(run.started_at);
    if (!s) return '—';
    var e = toDate(run.finished_at);
    if (!e && !TERMINAL[run.status]) e = new Date();
    if (!e) return '—';
    return fmtDuration(e.getTime() - s.getTime());
  }

  // 通过步数 / 总步数
  function stepCounts(run) {
    if (!run) return '—';
    var total = run.total_steps;
    var passed = run.passed_steps;
    if (Array.isArray(run.step_results)) {
      total = run.step_results.length;
      passed = run.step_results.filter(function (r) { return r.status === 'passed'; }).length;
    }
    if (total === null || total === undefined) total = run.step_count;
    // 列表接口的 Run 摘要可能不含步数统计，此时给出占位符而不是误导性的 0
    if (total === null || total === undefined) return '—';
    if (passed === null || passed === undefined) passed = '–';
    return passed + '/' + total;
  }

  function actionLabel(action) { return ACTION_LABEL[action] || action || '—'; }

  function isActiveRun(status) { return status === 'queued' || status === 'running'; }

  function runStatusBadge(status) {
    var m = RUN_STATUS[status] || { label: status || '未知', cls: 'badge--queued' };
    var dot = status === 'running' ? '<i class="dot dot--blink"></i>' : '<i class="dot"></i>';
    return '<span class="badge ' + m.cls + '">' + dot + esc(m.label) + '</span>';
  }

  function recStatusBadge(status) {
    var m = REC_STATUS[status] || { label: status || '未知', cls: 'badge--draft' };
    return '<span class="badge ' + m.cls + '"><i class="dot"></i>' + esc(m.label) + '</span>';
  }

  function sourceLabel(s) { return SOURCE_LABEL[s] || s || '—'; }

  function tagChips(tags) {
    if (!Array.isArray(tags) || !tags.length) return '<span class="muted">—</span>';
    return tags.map(function (t) { return '<span class="chip">' + esc(t) + '</span>'; }).join('');
  }

  // 把 value 中的 {{变量}} 高亮成小徽章（先转义，再包裹）
  function highlightVars(value) {
    if (!value) return '<span class="muted">—</span>';
    return esc(value).replace(/\{\{\s*([^{}]+?)\s*\}\}/g, function (all, name) {
      return '<span class="var-badge">{{' + name + '}}</span>';
    });
  }

  function hasVars(value) { return typeof value === 'string' && /\{\{[^{}]*\}\}/.test(value); }

  // 标签字符串 → 数组（支持中英文逗号、空格分隔）
  function parseTags(str) {
    if (!str) return [];
    return String(str).split(/[,，;；\s]+/).map(function (s) { return s.trim(); })
      .filter(function (s) { return s !== ''; });
  }

  function apiItems(res) {
    if (Array.isArray(res)) return res;
    if (res && Array.isArray(res.items)) return res.items;
    return [];
  }

  function safeFileName(name) {
    return String(name || 'recording').replace(/[\\/:*?"<>|\s]+/g, '_').slice(0, 80) || 'recording';
  }

  function levelClass(level) {
    var l = String(level || 'info').toLowerCase();
    if (l === 'error' || l === 'err' || l === 'fatal') return 'err';
    if (l === 'warn' || l === 'warning') return 'warn';
    if (l === 'success' || l === 'ok' || l === 'passed') return 'ok';
    if (l === 'debug' || l === 'trace') return 'debug';
    return 'info';
  }

  /* ======================================================================
   * 3. 接口封装 + toast + 模态框
   * ====================================================================== */

  /**
   * 统一 fetch 封装
   * @param {string} method HTTP 方法
   * @param {string} path   同源相对路径，如 /api/recordings
   * @param {*}      body   有值则自动 JSON.stringify
   * @param {object} opts   {silent:true} 时不弹 toast（用于轮询）
   */
  async function api(method, path, body, opts) {
    opts = opts || {};
    var init = { method: method, headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json; charset=utf-8';
      init.body = JSON.stringify(body);
    }
    var res;
    try {
      res = await fetch(path, init);
    } catch (err) {
      var netMsg = '无法连接服务端（' + (err && err.message ? err.message : '网络错误') + '）';
      if (!opts.silent) toast(netMsg, 'error');
      var ne = new Error(netMsg);
      ne.network = true;
      throw ne;
    }
    var text = '';
    try { text = await res.text(); } catch (e) { text = ''; }
    var data = null;
    if (text) {
      try { data = JSON.parse(text); } catch (e2) { data = { raw: text }; }
    }
    if (!res.ok) {
      var msg = (data && (data.error || data.message)) || ('请求失败（HTTP ' + res.status + '）');
      if (!opts.silent) toast(msg, 'error');
      var err2 = new Error(msg);
      err2.status = res.status;
      err2.data = data;
      throw err2;
    }
    return data === null ? {} : data;
  }

  // 右下角 toast，3 秒自动消失
  function toast(message, kind, ms) {
    var host = $('#toast-host');
    if (!host) return;
    kind = kind || 'info';
    var node = document.createElement('div');
    node.className = 'toast toast--' + kind;
    node.innerHTML = '<span class="toast-dot"></span><span class="toast-text">' + esc(message) + '</span>';
    host.appendChild(node);
    var life = typeof ms === 'number' ? ms : 3000;
    setTimeout(function () {
      node.classList.add('toast--out');
      setTimeout(function () { if (node.parentNode) node.parentNode.removeChild(node); }, 240);
    }, life);
  }

  // 打开模态框（返回 {overlay, close}）
  var openModalClosers = [];

  function openModal(opts) {
    opts = opts || {};
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.innerHTML =
      '<div class="modal" role="dialog" aria-modal="true" style="max-width:' + (opts.width || 480) + 'px">' +
      '<div class="modal-head"><h3>' + esc(opts.title || '') + '</h3>' +
      '<button class="icon-btn" data-modal-close aria-label="关闭" title="关闭">✕</button></div>' +
      '<div class="modal-body">' + (opts.body || '') + '</div>' +
      (opts.footer ? '<div class="modal-foot">' + opts.footer + '</div>' : '') +
      '</div>';
    document.body.appendChild(overlay);

    var closed = false;
    function close() {
      if (closed) return;
      closed = true;
      document.removeEventListener('keydown', onKey);
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      var i = openModalClosers.indexOf(close);
      if (i >= 0) openModalClosers.splice(i, 1);
    }
    function onKey(e) { if (e.key === 'Escape') close(); }

    document.addEventListener('keydown', onKey);
    overlay.addEventListener('click', function (e) { if (e.target === overlay) close(); });
    $$('[data-modal-close]', overlay).forEach(function (b) { b.addEventListener('click', close); });
    openModalClosers.push(close);

    var first = overlay.querySelector('input, textarea, select');
    if (first) {
      try { first.focus(); } catch (e) { /* ignore */ }
    }
    return { overlay: overlay, close: close };
  }

  function closeAllModals() {
    openModalClosers.slice().forEach(function (fn) { try { fn(); } catch (e) { /* ignore */ } });
    openModalClosers = [];
  }

  // 二次确认（Promise<boolean>）
  function confirmDialog(opts) {
    opts = opts || {};
    return new Promise(function (resolve) {
      var m = openModal({
        title: opts.title || '确认操作',
        width: 440,
        body: '<p class="confirm-text">' + esc(opts.message || '') + '</p>',
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn ' + (opts.danger === false ? 'btn--primary' : 'btn--danger') + '" data-ok>' +
          esc(opts.confirmText || '确定') + '</button>'
      });
      var answered = false;
      var ok = m.overlay.querySelector('[data-ok]');
      if (ok) {
        ok.addEventListener('click', function () {
          answered = true;
          m.close();
          resolve(true);
        });
      }
      // 其他方式关闭（× / 遮罩 / ESC）视为取消
      var mo = new MutationObserver(function () {
        if (!document.body.contains(m.overlay)) {
          mo.disconnect();
          if (!answered) resolve(false);
        }
      });
      mo.observe(document.body, { childList: true });
    });
  }

  /* ---- 通用展示片段 ---- */

  function loadingCard(text) {
    return '<div class="card state-card"><span class="spinner"></span><span>' + esc(text || '加载中…') + '</span></div>';
  }

  function emptyCard(title, desc, actionsHtml) {
    return '<div class="empty"><div class="empty-title">' + esc(title) + '</div>' +
      '<p>' + (desc || '') + '</p>' +
      (actionsHtml ? '<div class="empty-actions">' + actionsHtml + '</div>' : '') +
      '</div>';
  }

  function errorCard(message) {
    return '<div class="card state-card" style="color:var(--red)"><span>加载失败：' + esc(message || '未知错误') + '</span>' +
      '<button class="btn btn--sm" onclick="location.reload()">刷新页面</button></div>';
  }

  function notFoundCard() {
    return emptyCard('页面不存在', '请检查地址，或回到 <a href="#/recordings">录制列表</a>。');
  }

  function field(labelText, controlHtml, hintHtml) {
    return '<div class="field"><label>' + esc(labelText) + '</label>' + controlHtml +
      (hintHtml ? '<span class="field-hint">' + hintHtml + '</span>' : '') + '</div>';
  }

  /* ======================================================================
   * 4. 顶部导航：健康轮询 + 内置浏览器控制
   * ====================================================================== */

  var Health = { data: null, timer: null };

  // 辅助能力开关（服务端 /api/health 带出来，控制台全局用来决定是否显示提醒等）
  var Assist = { auto_data_key: true, stable_id_hint: true, auto_rebind_uid: true };

  async function refreshHealth() {
    try {
      Health.data = await api('GET', '/api/health', undefined, { silent: true });
    } catch (e) {
      Health.data = null;
    }
    if (Health.data && Health.data.assist) {
      var a = Health.data.assist;
      Assist.auto_data_key = a.auto_data_key !== false;
      Assist.stable_id_hint = a.stable_id_hint !== false;
      Assist.auto_rebind_uid = a.auto_rebind_uid !== false;
    }
    paintHealth();
  }

  function paintHealth() {
    var host = $('#health');
    if (!host) return;
    var h = Health.data;

    var badge;
    if (h && h.ok !== false) {
      var count = (h.recordings === null || h.recordings === undefined) ? 0 : h.recordings;
      badge = '<span class="badge badge--ok" title="服务端版本 ' + esc(h.version || '') + '">' +
        '<i class="dot"></i>服务正常 · ' + esc(count) + ' 条录制</span>';
    } else {
      badge = '<span class="badge badge--fail" title="无法访问 /api/health"><i class="dot"></i>服务异常</span>';
    }

    var running = !!(h && (h.browser_running === true || h.browser_running === 'true'));
    host.innerHTML = badge +
      '<span class="browser-state ' + (running ? 'is-on' : '') + '">浏览器：' + (running ? '运行中' : '未启动') + '</span>' +
      '<button class="btn btn--sm" data-browser="launch"' + (running ? ' disabled' : '') + '>启动浏览器</button>' +
      '<button class="btn btn--sm btn--ghost" data-browser="close"' + (running ? '' : ' disabled') + '>关闭</button>';
  }

  function bindHealthControls() {
    var host = $('#health');
    if (!host) return;
    host.addEventListener('click', async function (e) {
      var target = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!target || !target.closest) return;
      var btn = target.closest('[data-browser]');
      if (!btn) return;
      var what = btn.getAttribute('data-browser');
      btn.disabled = true;
      try {
        if (what === 'launch') {
          await api('POST', '/api/browser/launch', {});
          toast('内置浏览器已启动', 'success');
        } else {
          await api('POST', '/api/browser/close', {});
          toast('内置浏览器已关闭', 'success');
        }
      } catch (err) {
        /* api() 已 toast */
      }
      await refreshHealth();
    });
  }

  /* ======================================================================
   * 5. hash 路由 + 页面级清理
   * ====================================================================== */

  var cleanups = [];

  // 页面注册清理函数（定时器 / EventSource 等）
  function addCleanup(fn) { cleanups.push(fn); }

  function runCleanups() {
    var list = cleanups;
    cleanups = [];
    for (var i = list.length - 1; i >= 0; i--) {
      try { list[i](); } catch (e) { console.warn('[cleanup]', e); }
    }
  }

  function parseHash() {
    var raw = String(location.hash || '').replace(/^#/, '');
    return raw.split('/').filter(function (p) { return p !== ''; });
  }

  function navigate(hash) {
    if (location.hash === hash) {
      router();
    } else {
      location.hash = hash;
    }
  }

  function setActiveNav(page) {
    $$('#nav .nav-item').forEach(function (a) {
      a.classList.toggle('active', a.getAttribute('data-nav') === page);
    });
  }

  // 新建一个页面容器（路由切换时随 innerHTML 一起销毁，事件监听自动释放）
  function pageRoot(app) {
    var root = document.createElement('div');
    root.className = 'page';
    app.appendChild(root);
    return root;
  }

  function router() {
    runCleanups();
    closeAllModals();

    var app = $('#app');
    if (!app) return;
    app.innerHTML = '';
    try { window.scrollTo(0, 0); } catch (e) { /* ignore */ }

    var parts = parseHash();
    var page = parts[0] || 'recordings';
    var id = parts[1] ? decodeURIComponent(parts[1]) : '';
    var title = '录制';

    if (page === 'recordings') {
      setActiveNav('recordings');
      if (id) { title = '录制详情'; renderRecordingDetail(app, id); }
      else { renderRecordings(app); }
    } else if (page === 'runs') {
      setActiveNav('runs');
      title = '运行';
      if (id) { title = '运行详情'; renderRunDetail(app, id); }
      else { renderRuns(app); }
    } else if (page === 'elements') {
      setActiveNav('elements');
      title = '元素库';
      renderElements(app);
    } else if (page === 'settings') {
      setActiveNav('settings');
      title = '设置';
      renderSettings(app);
    } else {
      setActiveNav('');
      title = '页面不存在';
      pageRoot(app).innerHTML = notFoundCard();
    }

    document.title = title + ' · UI 自动化测试平台';
  }

  /* ======================================================================
   * 浮动卡片工具：让任意「卡片」默认变成可拖动/可缩放的悬浮面板
   *   —— 实时日志、数据准备都用它，行为一致：
   *      · 面板始终悬浮（不做停靠模式），可拖动、可缩放、可收起
   *      · 位置/尺寸记忆、拖出视口自动夹回、双击标题栏收起/展开
   *      · 元素被 innerHTML 重建后重新调用 applyFloatCard() 即可恢复
   * ====================================================================== */

  var FLOAT_MIN_W = 320;
  var FLOAT_MIN_H = 200;

  function lsRead(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }
  function lsWrite(key, val) {
    try { localStorage.setItem(key, val); } catch (e) { /* 隐私模式忽略 */ }
  }
  function jsonRead(key) {
    try {
      var v = JSON.parse(lsRead(key) || 'null');
      return v && typeof v === 'object' ? v : null;
    } catch (e) { return null; }
  }
  function clampPx(v, min, max) {
    return Math.max(min, Math.min(max, v));
  }

  /**
   * 给卡片接上浮动/拖动/缩放/收起能力。
   * @param {HTMLElement} el 目标卡片（需有 .card-head 作为拖动手柄）
   * @param {object} opts { key, grid, gridClass, defaultFloating, defaultPos }
   */
  function applyFloatCard(el, opts) {
    if (!el || !el.classList) return;
    opts = opts || {};
    var key = opts.key || 'card';
    var FLAG_KEY = 'uitest.float.' + key;
    var POS_KEY = 'uitest.pos.' + key;
    var head = el.querySelector('.card-head') || el.firstElementChild;
    var grid = opts.grid || el.parentElement;

    function isFloating() { return el.classList.contains('is-floating'); }

    function persistPos() {
      if (!isFloating()) return;
      var r = el.getBoundingClientRect();
      lsWrite(POS_KEY, JSON.stringify({
        l: Math.round(r.left), t: Math.round(r.top),
        w: Math.round(r.width), h: Math.round(r.height)
      }));
    }

    function syncGrid() {
      if (grid && opts.gridClass) grid.classList.toggle(opts.gridClass, isFloating());
    }

    function setCollapsed(on) {
      el.classList.toggle('is-collapsed', !!on);
      var b = el.querySelector('[data-float-act="collapse"]');
      if (b) b.textContent = on ? '展开' : '收起';
    }

    function setFloating(on, silent) {
      if (on) {
        var pos = jsonRead(POS_KEY) || {};
        var vw = window.innerWidth, vh = window.innerHeight;
        var def = opts.defaultPos || {};
        var w = clampPx(pos.w || def.w || Math.round(vw * 0.28), FLOAT_MIN_W, Math.max(FLOAT_MIN_W, vw - 8));
        var h = clampPx(pos.h || def.h || Math.round(vh * 0.62), FLOAT_MIN_H, Math.max(FLOAT_MIN_H, vh - 8));
        var l = typeof pos.l === 'number' ? pos.l : (def.l === 'right' ? vw - w - 24 : (def.l || vw - w - 24));
        var t = typeof pos.t === 'number' ? pos.t : (def.t || 96);
        el.classList.add('is-floating');
        el.classList.remove('is-collapsed');
        el.style.width = w + 'px';
        el.style.height = h + 'px';
        el.style.left = clampPx(l, 4, Math.max(4, vw - w - 4)) + 'px';
        el.style.top = clampPx(t, 4, Math.max(4, vh - h - 4)) + 'px';
        setCollapsed(false);
      } else {
        el.classList.remove('is-floating');
        el.style.width = '';
        el.style.height = '';
        el.style.left = '';
        el.style.top = '';
      }
      syncGrid();
      if (!silent) lsWrite(FLAG_KEY, on ? '1' : '0');
    }

    // 头部工具按钮：只要「收起」（面板始终悬浮，不再提供停靠）
    if (head && !head.querySelector('[data-float-act]')) {
      var tools = document.createElement('div');
      tools.className = 'float-tools';
      tools.innerHTML = '<button class="btn btn--sm" type="button" data-float-act="collapse">收起</button>';
      head.appendChild(tools);
    }

    // 按钮行为（挂在卡片元素上，元素不被替换就一直有效；重复调用不会重复绑定）
    if (!el.__floatClickWired) {
      el.__floatClickWired = true;
      el.addEventListener('click', function (e) {
        var t = e.target && e.target.nodeType === 1 ? e.target.closest('[data-float-act]') : null;
        if (!t) return;
        if (t.getAttribute('data-float-act') === 'collapse') {
          setCollapsed(!el.classList.contains('is-collapsed'));
          e.stopPropagation();
        }
      });
    }

    // 拖动（只认标题栏，标题栏上的控件不触发）
    var drag = null;
    function onMove(ev) {
      if (!drag) return;
      var rect = el.getBoundingClientRect();
      el.style.left = clampPx(ev.clientX - drag.dx, 4, Math.max(4, window.innerWidth - rect.width - 4)) + 'px';
      el.style.top = clampPx(ev.clientY - drag.dy, 4, Math.max(4, window.innerHeight - rect.height - 4)) + 'px';
    }
    function onUp() {
      document.removeEventListener('pointermove', onMove);
      document.removeEventListener('pointerup', onUp);
      document.removeEventListener('pointercancel', onUp);
      el.classList.remove('is-dragging');
      if (drag) persistPos();
      drag = null;
    }
    // 拖动/双击：标题栏可能被 innerHTML 重建，所以按"这个 head 元素是否已接好"来判断，
    // 避免重复绑定导致一次拖动走两倍位移。
    if (head && head.__floatDragWired !== key) {
      head.__floatDragWired = key;
      head.setAttribute('title', '按住这一行可以拖动窗口');
      head.addEventListener('pointerdown', function (ev) {
        if (!isFloating() || ev.button !== 0) return;
        if (ev.target.closest('button, input, textarea, select, label, a')) return;
        var rect = el.getBoundingClientRect();
        drag = { dx: ev.clientX - rect.left, dy: ev.clientY - rect.top };
        el.classList.add('is-dragging');
        document.addEventListener('pointermove', onMove);
        document.addEventListener('pointerup', onUp);
        document.addEventListener('pointercancel', onUp);
        ev.preventDefault();
      });
      head.addEventListener('dblclick', function (ev) {
        if (ev.target.closest('button, input, textarea, select, label, a')) return;
        setCollapsed(!el.classList.contains('is-collapsed'));
      });
    }

    // 手动缩放后记住尺寸
    if (typeof window.ResizeObserver === 'function' && !el.__floatRO) {
      var t2 = null;
      var ro = new ResizeObserver(function () {
        if (!isFloating()) return;
        if (t2) clearTimeout(t2);
        t2 = setTimeout(persistPos, 300);
      });
      ro.observe(el);
      el.__floatRO = ro;
    }

    // 面板始终悬浮（最小化/尺寸/位置来自 localStorage，不存在"停靠"状态）
    setFloating(true, true);
    return { setFloating: setFloating, setCollapsed: setCollapsed, isFloating: isFloating };
  }

  /* ======================================================================
   * 6.1 录制列表  #/recordings
   * ====================================================================== */

  function renderRecordings(app) {
    var root = pageRoot(app);

    var state = { items: [], q: '', loading: true, debounce: null };

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>录制</h1><p class="sub">插件录制下来的业务场景；点名称查看步骤与数据准备</p></div>' +
      '<div class="head-actions">' +
      '<button class="btn" data-act="refresh">刷新</button>' +
      '<button class="btn" data-act="import">导入</button>' +
      '<button class="btn btn--primary" data-act="create">新建录制</button>' +
      '</div></div>' +
      '<section class="card">' +
      '<div class="card-toolbar">' +
      '<div class="search">' +
      '<span class="search-icon" aria-hidden="true">' +
      '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
      'stroke-linecap="round"><circle cx="11" cy="11" r="7"></circle><path d="M20 20l-3.6-3.6"></path></svg>' +
      '</span>' +
      '<input id="rec-search" class="input" type="search" placeholder="按名称 / 标签 / 说明过滤" autocomplete="off">' +
      '</div>' +
      '<div class="toolbar-right muted" id="rec-count"></div>' +
      '</div>' +
      '<div class="table-wrap">' +
      '<table class="table">' +
      '<thead><tr>' +
      '<th style="width:32%">名称</th>' +
      '<th class="num" style="width:70px">步骤数</th>' +
      '<th style="width:96px">状态</th>' +
      '<th>标签</th>' +
      '<th style="width:130px">更新时间</th>' +
      '<th class="actions-cell" style="width:210px">操作</th>' +
      '</tr></thead>' +
      '<tbody id="rec-body"><tr><td colspan="6" class="cell-state">加载中…</td></tr></tbody>' +
      '</table>' +
      '</div>' +
      '</section>';

    var tbody = $('#rec-body', root);
    var countEl = $('#rec-count', root);

    // 前端过滤：名称 / 标签 / 说明
    function filterItems(items, q) {
      var kw = String(q || '').trim().toLowerCase();
      if (!kw) return items;
      return items.filter(function (it) {
        var hay = [it.name, it.description, (it.tags || []).join(' ')].join(' ').toLowerCase();
        return hay.indexOf(kw) >= 0;
      });
    }

    function rowHtml(rec) {
      var rid = encodeURIComponent(rec.id);
      return '<tr>' +
        '<td class="cell-name">' +
        '<a class="strong" href="#/recordings/' + rid + '">' + esc(rec.name || '(未命名)') + '</a>' +
        (rec.description ? '<div class="cell-sub">' + esc(rec.description) + '</div>' : '') +
        '</td>' +
        '<td class="num mono">' + (rec.step_count === null || rec.step_count === undefined ? '—' : esc(rec.step_count)) + '</td>' +
        '<td>' + recStatusBadge(rec.status) + '</td>' +
        '<td>' + tagChips(rec.tags) + '</td>' +
        '<td class="nowrap muted" title="' + esc(fmtDateTime(rec.updated_at)) + '">' + esc(relTime(rec.updated_at)) + '</td>' +
        '<td class="actions-cell">' +
        '<a class="btn btn--sm" href="#/recordings/' + rid + '">查看</a>' +
        '<button class="btn btn--sm" data-act="run" data-id="' + esc(rec.id) + '">执行</button>' +
        '<button class="btn btn--sm" data-act="export" data-id="' + esc(rec.id) + '">导出</button>' +
        '<button class="btn btn--sm btn--danger" data-act="del" data-id="' + esc(rec.id) + '">删除</button>' +
        '</td>' +
        '</tr>';
    }

    function paint() {
      var items = filterItems(state.items, state.q);
      countEl.textContent = state.items.length ? ('共 ' + items.length + ' 条' + (state.q ? '（已过滤）' : '')) : '';

      if (!state.items.length) {
        tbody.innerHTML = '<tr><td colspan="6" class="cell-state" style="padding:0">' +
          emptyCard(
            '还没有任何录制',
            '先在浏览器里安装录制插件，打开业务页面点「开始录制」；也可以直接新建一条空录制，或导入其他人导出的 JSON。',
            '<button class="btn btn--primary" data-act="create">新建录制</button>' +
            '<button class="btn" data-act="import">导入 JSON</button>'
          ) + '</td></tr>';
        return;
      }
      if (!items.length) {
        tbody.innerHTML = '<tr><td colspan="6" class="cell-state" style="padding:0">' +
          emptyCard('没有匹配的录制', '换个关键词，或清空搜索框。',
            '<button class="btn" data-act="clear-search">清空搜索</button>') + '</td></tr>';
        return;
      }
      tbody.innerHTML = items.map(rowHtml).join('');
    }

    async function load(withLoading) {
      if (withLoading) tbody.innerHTML = '<tr><td colspan="6" class="cell-state">加载中…</td></tr>';
      try {
        var res = await api('GET', '/api/recordings?q=' + encodeURIComponent(state.q || ''), undefined, { silent: true });
        state.items = apiItems(res);
        state.loading = false;
        paint();
      } catch (e) {
        state.loading = false;
        countEl.textContent = '';
        tbody.innerHTML = '<tr><td colspan="6" class="cell-state cell-state--error">' +
          esc(e.message || '加载失败') +
          '　<button class="btn btn--sm" data-act="refresh">重试</button></td></tr>';
      }
    }

    /* ---- 新建 / 导入 ---- */
    function openCreateModal() {
      var m = openModal({
        title: '新建录制',
        width: 540,
        body:
          field('名称 *', '<input class="input" id="f-name" placeholder="例如：新建服务商品">') +
          field('起始 URL', '<input class="input input--mono" id="f-url" placeholder="http://127.0.0.1:8480/admin/#/products">',
            '执行时会先打开这个地址') +
          field('标签', '<input class="input" id="f-tags" placeholder="用逗号分隔，如：商品, 回归">') +
          field('说明', '<textarea class="input" id="f-desc" rows="3" placeholder="这条录制做什么"></textarea>'),
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn btn--primary" id="f-submit">创建</button>'
      });

      var submit = $('#f-submit', m.overlay);
      submit.addEventListener('click', async function () {
        var name = $('#f-name', m.overlay).value.trim();
        if (!name) { toast('请填写录制名称', 'error'); return; }
        var payload = { name: name };
        var url = $('#f-url', m.overlay).value.trim();
        if (url) payload.start_url = url;
        var tags = parseTags($('#f-tags', m.overlay).value);
        if (tags.length) payload.tags = tags;
        var desc = $('#f-desc', m.overlay).value.trim();
        if (desc) payload.description = desc;

        submit.disabled = true;
        try {
          var rec = await api('POST', '/api/recordings', payload);
          m.close();
          toast('录制已创建', 'success');
          await refreshHealth();
          if (rec && rec.id) navigate('#/recordings/' + encodeURIComponent(rec.id));
          else load();
        } catch (e) {
          submit.disabled = false;
        }
      });
    }

    function openImportModal() {
      var m = openModal({
        title: '导入录制',
        width: 620,
        body: field('粘贴导出的 JSON',
          '<textarea class="input input--mono" id="f-json" rows="12" placeholder="{ &quot;name&quot;: &quot;新建服务商品&quot;, &quot;steps&quot;: [ ... ] }"></textarea>',
          '内容与「导出」得到的一致；步骤会一并导入。'),
        footer: '<button class="btn" data-modal-close>取消</button>' +
          '<button class="btn btn--primary" id="f-import">导入</button>'
      });

      var btn = $('#f-import', m.overlay);
      btn.addEventListener('click', async function () {
        var raw = $('#f-json', m.overlay).value.trim();
        if (!raw) { toast('请先粘贴 JSON', 'error'); return; }
        var payload;
        try { payload = JSON.parse(raw); }
        catch (e) { toast('JSON 解析失败：' + e.message, 'error'); return; }
        btn.disabled = true;
        try {
          var rec = await api('POST', '/api/recordings/import', payload);
          m.close();
          toast('导入成功', 'success');
          await refreshHealth();
          load();
          if (rec && rec.id) navigate('#/recordings/' + encodeURIComponent(rec.id));
        } catch (e) {
          btn.disabled = false;
        }
      });
    }

    /* ---- 行操作 ---- */
    async function exportRecording(recId, recName) {
      try {
        var res = await fetch('/api/recordings/' + encodeURIComponent(recId) + '/export');
        if (!res.ok) throw new Error('导出失败（HTTP ' + res.status + '）');
        var blob = await res.blob();
        var url = URL.createObjectURL(blob);
        var a = document.createElement('a');
        a.href = url;
        a.download = safeFileName(recName) + '.json';
        document.body.appendChild(a);
        a.click();
        if (a.parentNode) a.parentNode.removeChild(a);
        setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
        toast('已开始下载导出文件', 'success');
      } catch (e) {
        toast(e.message || '导出失败', 'error');
      }
    }

    async function runRecording(recId) {
      try {
        var run = await api('POST', '/api/runs', { recording_id: recId, dataset_source: 'manual', mode: 'server' });
        toast('已提交执行，正在跳转运行详情…', 'success');
        if (run && run.id) navigate('#/runs/' + encodeURIComponent(run.id));
      } catch (e) { /* api 已提示 */ }
    }

    async function deleteRecording(rec) {
      var ok = await confirmDialog({
        title: '删除录制',
        message: '确定删除「' + (rec.name || rec.id) + '」吗？该录制的步骤会一起删除，且不可恢复。',
        confirmText: '删除'
      });
      if (!ok) return;
      try {
        await api('DELETE', '/api/recordings/' + encodeURIComponent(rec.id));
        toast('已删除', 'success');
        await load();
        await refreshHealth();
      } catch (e) { /* api 已提示 */ }
    }

    function findRec(id) {
      for (var i = 0; i < state.items.length; i++) {
        if (state.items[i].id === id) return state.items[i];
      }
      return { id: id, name: id };
    }

    /* ---- 事件 ---- */
    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');
      var id = btn.getAttribute('data-id');

      if (act === 'refresh') { load(true); return; }
      if (act === 'create') { openCreateModal(); return; }
      if (act === 'import') { openImportModal(); return; }
      if (act === 'clear-search') {
        state.q = '';
        var input = $('#rec-search', root);
        if (input) input.value = '';
        load();
        return;
      }
      if (act === 'run') { runRecording(id); return; }
      if (act === 'export') {
        var rec = findRec(id);
        exportRecording(id, rec.name || id);
        return;
      }
      if (act === 'del') { deleteRecording(findRec(id)); return; }
    });

    var searchInput = $('#rec-search', root);
    searchInput.addEventListener('input', function () {
      state.q = searchInput.value;
      if (state.debounce) clearTimeout(state.debounce);
      state.debounce = setTimeout(function () {
        state.debounce = null;
        load();
      }, 260);
    });

    addCleanup(function () {
      if (state.debounce) clearTimeout(state.debounce);
    });

    load();
  }

  /* ======================================================================
   * 6.2 录制详情  #/recordings/:id
   * ====================================================================== */

  function renderRecordingDetail(app, id) {
    var root = pageRoot(app);
    var base = '/api/recordings/' + encodeURIComponent(id);

    var ui = {
      rec: null,
      drafts: {},          // step_id -> {annotation, value, data_key, expect}
      expandAll: false,    // 展开全部定位
      overrides: {},       // step_id -> bool（单行覆盖展开状态）
      showValue: true,     // 是否显示「输入值」列
      dataset: {},         // data_key -> value
      fields: [],          // AI/规则生成的字段
      source: 'manual',    // ai | rules | manual
      instruction: '',
      warning: '',
      generating: false,
      running: false
    };

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>录制详情</h1><p class="sub"><a href="#/recordings">← 返回录制列表</a></p></div>' +
      '</div>' +
      '<div id="rd-body">' + loadingCard() + '</div>';

    var body = $('#rd-body', root);

    /* ---- 数据同步 / 判断 ---- */

    function findStep(stepId) {
      var steps = (ui.rec && ui.rec.steps) || [];
      for (var i = 0; i < steps.length; i++) {
        if (steps[i].id === stepId) return steps[i];
      }
      return null;
    }

    // 保留用户未保存的本地修改，其余以服务端为准
    function syncDrafts(rec) {
      var prevMap = {};
      var prevSteps = (ui.rec && ui.rec.steps) || [];
      prevSteps.forEach(function (s) { prevMap[s.id] = s; });

      var next = {};
      var steps = rec.steps || [];
      steps.forEach(function (step) {
        var d = ui.drafts[step.id];
        var p = prevMap[step.id];
        var out = {};
        STEP_FIELDS.forEach(function (f) {
          var serverVal = step[f] === null || step[f] === undefined ? '' : String(step[f]);
          var val = serverVal;
          if (d && p) {
            var prevServer = p[f] === null || p[f] === undefined ? '' : String(p[f]);
            if (d[f] !== undefined && d[f] !== prevServer) val = d[f];
          }
          out[f] = val;
        });
        next[step.id] = out;
      });
      ui.drafts = next;
    }

    function isDirty(step) {
      if (!step) return false;
      var d = ui.drafts[step.id];
      if (!d) return false;
      for (var i = 0; i < STEP_FIELDS.length; i++) {
        var f = STEP_FIELDS[i];
        var sv = step[f] === null || step[f] === undefined ? '' : String(step[f]);
        var dv = d[f] === null || d[f] === undefined ? '' : String(d[f]);
        if (sv !== dv) return true;
      }
      return false;
    }

    function isLocOpen(step) {
      var o = ui.overrides[step.id];
      return typeof o === 'boolean' ? o : ui.expandAll;
    }

    /* ---- 片段 ---- */

    function targetCellHtml(step) {
      var t = step.target || {};
      var primary = t.label || t.text || t.placeholder || t.aria || '';
      if (!primary) primary = t.tag ? '<' + t.tag + '>' : '—';
      var sub = [];
      if (t.page) sub.push(t.page);
      if (t.field_kind) sub.push(t.field_kind);
      return '<div class="target-primary">' + esc(primary) + '</div>' +
        (sub.length ? '<div class="target-sub">' + esc(sub.join(' · ')) + '</div>' : '');
    }

    function locatorCellHtml(step, open) {
      var loc = step.locator || {};
      var strategy = loc.strategy || '';
      var value = loc.value || '';
      var fb = Array.isArray(loc.fallbacks) ? loc.fallbacks : [];
      // 导航步骤不做元素定位，locator 里的 url 只是记录地址，界面上给个更明确的说法
      if (step.action === 'navigate') {
        return '<div class="locator"><span class="locator-na">—（导航步骤按「输入值」里的地址跳转）</span></div>';
      }
      // 定位稳定性提示：Vue/React 重新打包后各策略的存活率差异很大
      var stab = '';
      if (strategy === 'uid' || strategy === 'attr' || strategy === 'id' || strategy === 'name') {
        stab = '<span class="badge badge--stable" title="业务语义/团队自定义属性，重新打包也不会变">稳</span>';
      } else if (strategy === 'label' || strategy === 'near' || strategy === 'role' || strategy === 'cell' || strategy === 'placeholder') {
        stab = '<span class="badge badge--mid" title="靠表单项标题 / 按钮文字 / 表格行文字定位，改版只要文案不变就能用">中</span>';
      } else if (strategy === 'css' || strategy === 'xpath' || strategy === 'text') {
        stab = '<span class="badge badge--weak" title="结构路径或纯文本，改版/重新打包后容易失效（有兜底，但建议沉淀成 uid）">弱</span>';
      }
      // 缺稳定锚点：没有 data-testid 一类属性时给出红色提醒（设置里可关）
      var hasTestAttr = false;
      if (step.target && step.target.test_attr) hasTestAttr = true;
      fb.forEach(function (f) { if (f && f.strategy === 'attr') hasTestAttr = true; });
      var missTestId = '';
      if (Assist.stable_id_hint && !hasTestAttr) {
        missTestId = '<span class="badge badge--missing" title="这个元素没有 data-testid / data-test / data-cy 之类的稳定属性。' +
          '现在是靠标题/文字定位的，建议让前端加一个（例如 data-testid=&quot;product-name&quot;），定位就永远不怕改版">缺 testid</span>';
      }
      var h = '<div class="locator">' +
        '<code class="loc-code">' + esc(strategy ? strategy + '=' + value : '—') + '</code>' +
        (strategy === 'uid' ? '<span class="badge badge--uid">ID 绑定</span>' : '') + stab + missTestId;

      if (fb.length) {
        h += '<button class="link-btn" data-act="toggle-loc" data-step="' + esc(step.id) + '">' +
          (open ? '收起备用（' + fb.length + '）' : '备用定位 ' + fb.length + ' 条') + '</button>';
        if (open) {
          h += '<ul class="fb-list">' + fb.map(function (f) {
            return '<li><span class="mono">' + esc((f.strategy || '') + '=' + (f.value || '')) + '</span></li>';
          }).join('') + '</ul>';
        }
      }
      return h + '</div>';
    }

    function valueCellHtml(step, d) {
      var needsKey = DATA_ACTIONS.indexOf(step.action) >= 0 || !!d.data_key;
      var val = d.value === undefined ? '' : d.value;
      return '<div class="value-cell">' +
        '<input class="input input--cell input--mono" data-edit="value" value="' + esc(val) + '" placeholder="输入值，支持 {{变量}}">' +
        (needsKey ? '<input class="input input--cell input--tiny input--mono" data-edit="data_key" value="' + esc(d.data_key) + '" placeholder="data_key（数据字段名）">' : '') +
        (hasVars(val) ? '<div class="var-line">' + highlightVars(val) + '</div>' : '') +
        '</div>';
    }

    function rowOpsHtml(step, index, total, dirty) {
      return '<div class="op-group">' +
        '<button class="icon-btn" data-act="step-up" data-step="' + esc(step.id) + '" title="上移"' + (index === 0 ? ' disabled' : '') + '>↑</button>' +
        '<button class="icon-btn" data-act="step-down" data-step="' + esc(step.id) + '" title="下移"' + (index === total - 1 ? ' disabled' : '') + '>↓</button>' +
        '<button class="btn btn--sm ' + (dirty ? 'btn--primary' : '') + '" data-act="step-save" data-step="' + esc(step.id) + '">保存</button>' +
        '<button class="icon-btn icon-btn--danger" data-act="step-del" data-step="' + esc(step.id) + '" title="删除该步骤">✕</button>' +
        '</div>';
    }

    function stepRowHtml(step, index, total) {
      var d = ui.drafts[step.id] || { annotation: '', value: '', data_key: '', expect: '' };
      var dirty = isDirty(step);
      return '<tr data-step="' + esc(step.id) + '"' + (dirty ? ' class="is-dirty"' : '') + '>' +
        '<td class="w-idx mono">' + (index + 1) + '</td>' +
        '<td class="w-action"><span class="action-tag" title="' + esc(step.action || '') + '">' + esc(actionLabel(step.action)) + '</span>' +
        (step.optional ? ' <span class="chip chip--mini">可选</span>' : '') + '</td>' +
        '<td class="w-target">' + targetCellHtml(step) + '</td>' +
        '<td class="w-locator" data-cell="locator">' + locatorCellHtml(step, isLocOpen(step)) + '</td>' +
        '<td class="w-value" data-col="value">' + valueCellHtml(step, d) + '</td>' +
        '<td><input class="input input--cell" data-edit="annotation" value="' + esc(d.annotation) + '" placeholder="步骤备注"></td>' +
        '<td><input class="input input--cell" value="' + esc(d.expect) + '" data-edit="expect" placeholder="如 url:/products"></td>' +
        '<td class="actions-cell">' + rowOpsHtml(step, index, total, dirty) + '</td>' +
        '</tr>';
    }

    function stepsTableHtml(steps) {
      return '<div class="table-wrap">' +
        '<table class="table table--steps' + (ui.showValue ? '' : ' hide-value') + '">' +
        '<thead><tr>' +
        '<th class="w-idx">序号</th>' +
        '<th class="w-action">动作</th>' +
        '<th class="w-target">目标描述</th>' +
        '<th class="w-locator">定位</th>' +
        '<th class="w-value" data-col="value">输入值 / 数据字段</th>' +
        '<th>备注 annotation</th>' +
        '<th>期望 expect</th>' +
        '<th class="actions-cell">操作</th>' +
        '</tr></thead>' +
        '<tbody>' + steps.map(function (s, i) { return stepRowHtml(s, i, steps.length); }).join('') + '</tbody>' +
        '</table></div>';
    }

    function headerCardHtml(rec) {
      var meta = '<dl class="meta-grid">' +
        '<div><dt>起始 URL</dt><dd class="mono">' + (rec.start_url
          ? '<a href="' + esc(rec.start_url) + '" target="_blank" rel="noopener">' + esc(rec.start_url) + '</a>'
          : '<span class="muted">—</span>') + '</dd></div>' +
        '<div><dt>状态</dt><dd>' + recStatusBadge(rec.status) + '</dd></div>' +
        '<div><dt>步骤数</dt><dd class="mono">' + esc((rec.steps || []).length) + '</dd></div>' +
        '<div><dt>创建时间</dt><dd class="mono">' + esc(fmtDateTime(rec.created_at)) + '</dd></div>' +
        '<div><dt>更新时间</dt><dd class="mono">' + esc(fmtDateTime(rec.updated_at)) + '</dd></div>' +
        '</dl>';

      return '<section class="card rec-head">' +
        '<div class="rec-head-row">' +
        '<div class="field-inline field-inline--grow">' +
        '<label for="edit-name">名称（可点击编辑）</label>' +
        '<input class="input input--name" id="edit-name" value="' + esc(rec.name || '') + '" placeholder="录制名称">' +
        '</div>' +
        '<div class="field-inline">' +
        '<label for="edit-tags">标签（逗号分隔）</label>' +
        '<input class="input" id="edit-tags" value="' + esc((rec.tags || []).join(', ')) + '" placeholder="商品, 回归">' +
        '</div>' +
        '<div class="head-actions" style="padding-bottom:1px">' +
        '<button class="btn btn--primary btn--sm" data-act="head-save">保存名称/标签</button>' +
        '<button class="btn btn--sm" data-act="duplicate">复制为新录制</button>' +
        '<button class="btn btn--sm btn--danger" data-act="rec-delete">删除</button>' +
        '</div>' +
        '</div>' +
        meta +
        '</section>';
    }

    /* ---- 数据准备面板 ---- */

    function requiredKeys(steps) {
      var seen = {};
      var order = [];
      (steps || []).forEach(function (s) {
        if (DATA_ACTIONS.indexOf(s.action) >= 0 && s.data_key && !seen[s.data_key]) {
          seen[s.data_key] = true;
          order.push({ key: s.data_key, hint: s.annotation || '' });
        }
      });
      return order;
    }

    function coverageBlockHtml(steps) {
      var req = requiredKeys(steps);
      if (!req.length) {
        return '<div class="ds-block" id="ds-coverage">' +
          '<div class="ds-block-title"><span>数据覆盖率</span><span class="mono">0/0</span></div>' +
          '<p class="ds-hints">这条录制的输入类步骤还没有 data_key，执行时没有必须填充的字段。</p>' +
          '</div>';
      }
      var filled = req.filter(function (r) {
        return String(ui.dataset[r.key] === null || ui.dataset[r.key] === undefined ? '' : ui.dataset[r.key]).trim() !== '';
      });
      var pct = Math.round(filled.length / req.length * 100);
      var list = req.map(function (r) {
        var val = ui.dataset[r.key] === null || ui.dataset[r.key] === undefined ? '' : String(ui.dataset[r.key]);
        var has = val.trim() !== '';
        return '<li>' +
          '<span class="key-name">' + esc(r.key) + '</span>' +
          (has ? '<span class="ok">✓ 已填</span>' : '<span class="miss">✗ 缺失</span>') +
          (has ? '' : '<input class="input input--mono" data-dskey="' + esc(r.key) + '" value="" placeholder="手动补一个值">') +
          (r.hint ? '<span class="key-hint">' + esc(r.hint) + '</span>' : '') +
          '</li>';
      }).join('');

      return '<div class="ds-block" id="ds-coverage">' +
        '<div class="ds-block-title"><span>数据覆盖率</span><span class="mono">' +
        filled.length + '/' + req.length + ' · ' + pct + '%</span></div>' +
        '<div class="progress"><i style="width:' + pct + '%"></i></div>' +
        '<ul class="key-list">' + list + '</ul>' +
        '</div>';
    }

    function updateCoverageCounter() {
      var box = $('#ds-coverage', root);
      if (!box) return;
      var req = requiredKeys(ui.rec ? ui.rec.steps : []);
      var filled = req.filter(function (r) {
        return String(ui.dataset[r.key] === null || ui.dataset[r.key] === undefined ? '' : ui.dataset[r.key]).trim() !== '';
      }).length;
      var pct = req.length ? Math.round(filled / req.length * 100) : 0;
      var counter = box.querySelector('.ds-block-title .mono');
      if (counter) counter.textContent = filled + '/' + req.length + ' · ' + pct + '%';
      var bar = box.querySelector('.progress > i');
      if (bar) bar.style.width = pct + '%';
    }

    function fieldsTableHtml() {
      if (!ui.fields.length) return '';
      var rows = ui.fields.map(function (f) {
        var raw = ui.dataset[f.key];
        var val = raw === null || raw === undefined ? (f.value === null || f.value === undefined ? '' : f.value) : raw;
        return '<tr>' +
          '<td class="mono">' + esc(f.key) + '</td>' +
          '<td>' + esc(f.label || '') + '</td>' +
          '<td><span class="kind-tag">' + esc(f.kind || 'text') + '</span></td>' +
          '<td><input class="input input--cell input--mono" data-dskey="' + esc(f.key) + '" value="' + esc(val) + '"></td>' +
          '<td class="note-cell">' + esc(f.note || '') + '</td>' +
          '</tr>';
      }).join('');

      return '<div class="ds-block">' +
        '<div class="ds-block-title"><span>生成的数据字段</span><span class="mono">' + ui.fields.length + ' 项</span></div>' +
        '<div class="table-wrap"><table class="table table--fields">' +
        '<thead><tr><th>字段 key</th><th>中文名</th><th>类型</th><th>生成值（可改）</th><th>说明</th></tr></thead>' +
        '<tbody>' + rows + '</tbody></table></div>' +
        '</div>';
    }

    function dataCardHtml(rec) {
      var steps = rec.steps || [];
      var hints = '';
      // source=rules 的黄色提示与 warning 往往是同一句话，只显示一次
      if (ui.source === 'rules' && ui.fields.length && !ui.warning) {
        hints += '<div class="alert alert--warn">未配置大模型，已用内置规则生成</div>';
      }
      if (ui.warning) {
        hints += '<div class="alert alert--warn">' + esc(ui.warning) + '</div>';
      }
      if (ui.source === 'ai' && ui.fields.length) {
        hints += '<div class="alert alert--info">已使用大模型生成（' + esc(sourceLabel(ui.source)) + '）</div>';
      }

      return '<div class="card-head"><h2>数据准备</h2>' +
        '<span class="count-pill">' + esc(sourceLabel(ui.source)) + '</span></div>' +
        '<div class="card-body">' +
        '<div class="field">' +
        '<label for="ds-instruction">数据准备指令（可选）</label>' +
        '<textarea class="input" id="ds-instruction" rows="3" placeholder="商品名称带时间戳，价格 99-299，库存 50，分类选&quot;服务类&quot;">' + esc(ui.instruction) + '</textarea>' +
        '<span class="field-hint">用一句话描述需要的业务数据，会作为 AI 生成提示。</span>' +
        '</div>' +
        '<button class="btn btn--primary btn--block" data-act="gen-dataset"' + (ui.generating ? ' disabled' : '') + '>' +
        (ui.generating ? '生成中…' : 'AI 生成测试数据') + '</button>' +
        '<div class="ds-hints">未配置大模型时会自动改用内置规则生成，离线也能用。</div>' +
        (hints ? '<div style="margin-top:12px">' + hints + '</div>' : '') +
        fieldsTableHtml() +
        coverageBlockHtml(steps) +
        '<div class="ds-block">' +
        '<button class="btn btn--primary btn--block" data-act="run-start"' + (ui.running ? ' disabled' : '') + '>' +
        (ui.running ? '提交中…' : '执行测试') + '</button>' +
        '<div class="ds-hints">执行会使用上面的 dataset 与指令，随后跳转到运行详情查看实时日志。</div>' +
        '</div>' +
        '</div>';
    }

    /* ---- 渲染 ---- */

    function paint() {
      var rec = ui.rec;
      if (!rec) return;
      var steps = rec.steps || [];

      body.innerHTML = headerCardHtml(rec) +
        navigateOnlyHintHtml(steps) +
        missingTestIdHintHtml(steps) +
        '<div class="detail-grid">' +
        '<section class="card steps-card">' +
        '<div class="card-head">' +
        '<h2>步骤 <span class="count-pill">' + steps.length + '</span></h2>' +
        '<div class="card-tools">' +
        '<button class="btn btn--sm" data-act="toggle-loc">' + (ui.expandAll ? '收起全部定位' : '展开全部定位') + '</button>' +
        '<button class="btn btn--sm" data-act="toggle-value">' + (ui.showValue ? '隐藏「输入值」列' : '显示「输入值」列') + '</button>' +
        '<button class="btn btn--sm" data-act="rebind" title="打开起始页，把每步定位重算成稳定的 data-uitest-id（老录制建议跑一次）">一键刷新定位</button>' +
        '<button class="btn btn--sm" data-act="reload">刷新</button>' +
        '</div></div>' +
        (steps.length ? stepsTableHtml(steps) :
          emptyCard('这条录制还没有步骤',
            '打开浏览器插件，点「开始录制」，在业务页面上正常操作一遍（点击、输入、选择…），步骤会自动同步到这里。')) +
        '</section>' +
        '<aside class="card data-card">' + dataCardHtml(rec) + '</aside>' +
        '</div>';

      // 数据准备面板：默认悬浮、可拖动（与实时日志同一套交互）
      applyFloatCard($('.data-card', root), {
        key: 'data',
        grid: $('.detail-grid', root),
        gridClass: 'has-floating-data',
        defaultFloating: true,
        defaultPos: { l: 'right', t: 300, w: 380, h: 540 }
      });
    }

    /**
     * 只有导航步骤时给出诊断提示。
     * 插件的内容脚本只在页面加载时注入；如果被测页面在装插件之前就打开了（或页面不允许
     * 注入脚本），录下来的就只有 background 自动补的 navigate 步骤，看起来像"定位全是
     * css=html"。这里直接把原因和解决办法摊在页面上。
     */
    /** 缺 data-testid 汇总提示（设置里可关）：直接列出是哪几个元素，方便推给前端 */
    function missingTestIdHintHtml(steps) {
      if (!Assist.stable_id_hint) return '';
      var missing = [];
      (steps || []).forEach(function (s) {
        if (!s || s.action === 'navigate') return;
        var fb = (s.locator && s.locator.fallbacks) || [];
        var has = !!(s.target && s.target.test_attr);
        if (!has) {
          for (var i = 0; i < fb.length; i++) {
            if (fb[i] && fb[i].strategy === 'attr') { has = true; break; }
          }
        }
        if (!has) {
          var label = (s.target && (s.target.label || s.target.text || s.target.placeholder)) || s.annotation || ('第 ' + s.index + ' 步');
          if (missing.indexOf(label) < 0) missing.push(label);
        }
      });
      if (!missing.length) return '';
      var shown = missing.slice(0, 8).map(function (x) { return '「' + esc(x) + '」'; }).join('、');
      var more = missing.length > 8 ? ' 等 ' + missing.length + ' 个' : '';
      return '<div class="callout callout--soft">' +
        '<b>' + missing.length + ' 个元素没有稳定锚点（data-testid）。</b>' +
        '现在靠表单项标题 / 按钮文字定位，改版或重新打包后可能失效，建议让前端给它们加上 ' +
        '<code class="mono">data-testid</code>（例如 <code class="mono">data-testid="product-name"</code>）：' +
        shown + more + '。' +
        '<br>加完之后回到这里点「一键刷新定位」，定位就会全部升级成 ID 绑定。' +
        '</div>';
    }

    function navigateOnlyHintHtml(steps) {
      if (!steps.length) return '';
      var real = steps.filter(function (s) { return s && s.action && s.action !== 'navigate'; });
      if (real.length > 0) return '';
      return '<div class="callout callout--warn">' +
        '<strong>这条录制只有「打开页面」步骤，没有录到任何操作。</strong>' +
        '<div>说明插件的内容脚本没有在被测页面上运行：通常是页面在安装/更新插件之前就已打开，' +
        '或者该页面不允许注入脚本（chrome:// 等系统页）。</div>' +
        '<div>处理：保持「录制中」，在被测页面刷新一次（或点插件弹窗里的「注入录制脚本」），' +
        '再操作一遍；然后新建一条录制重录即可。</div>' +
        '</div>';
    }

    function paintDataCard() {
      var card = $('.data-card', root);
      if (!card || !ui.rec) return;
      card.innerHTML = dataCardHtml(ui.rec);
      // 卡片内容被重建，浮动状态/拖动手柄要重新接上
      applyFloatCard(card, {
        key: 'data',
        grid: $('.detail-grid', root),
        gridClass: 'has-floating-data',
        defaultFloating: true,
        defaultPos: { l: 'right', t: 300, w: 380, h: 540 }
      });
    }

    async function load() {
      try {
        var rec = await api('GET', base);
        syncDrafts(rec);
        ui.rec = rec;
        paint();
      } catch (e) {
        body.innerHTML = errorCard(e.message || '无法加载录制详情') +
          '<div class="card card-body"><a class="btn" href="#/recordings">← 返回录制列表</a></div>';
      }
    }

    /* ---- 业务动作 ---- */

    async function saveHeader() {
      var rec = ui.rec;
      if (!rec) return;
      var nameEl = $('#edit-name', root);
      var tagsEl = $('#edit-tags', root);
      if (!nameEl) return;
      var name = nameEl.value.trim();
      if (!name) { toast('名称不能为空', 'error'); return; }

      var tags = parseTags(tagsEl ? tagsEl.value : '');
      var patch = {};
      if (name !== (rec.name || '')) patch.name = name;
      var oldTags = (rec.tags || []).join(',');
      if (tags.join(',') !== oldTags) patch.tags = tags;
      if (!Object.keys(patch).length) { toast('名称和标签没有变化', 'info'); return; }

      try {
        await api('PATCH', base, patch);
        toast('已保存', 'success');
        await load();
        await refreshHealth();
      } catch (e) { /* api 已提示 */ }
    }

    async function duplicateRecording() {
      var rec = ui.rec;
      if (!rec) return;
      try {
        var created = await api('POST', base + '/duplicate', { name: (rec.name || '录制') + ' 副本' });
        toast('已复制为新录制', 'success');
        await refreshHealth();
        if (created && created.id) navigate('#/recordings/' + encodeURIComponent(created.id));
      } catch (e) { /* api 已提示 */ }
    }

    /** 一键刷新定位：打开起始页重算每步的 uid（老录制升级成 ID 绑定） */
    async function rebind() {
      var rec = ui.rec;
      if (!rec) return;
      if (!(rec.steps || []).length) { toast('这条录制还没有步骤', 'info'); return; }
      var ok = await confirmDialog({
        title: '一键刷新定位',
        message: '会打开起始页 ' + (rec.start_url || '（录制里的地址）') + '，逐步重新定位并把新的 data-uitest-id 写回录制。' +
          '\n\n需要内置浏览器可用；过程中请勿关闭浏览器窗口。大约 10~60 秒。',
        confirmText: '开始刷新'
      });
      if (!ok) return;
      var card = $('.steps-card', root);
      if (card) card.classList.add('is-busy');
      toast('正在刷新定位，请稍候…', 'info', 4000);
      try {
        var r = await api('POST', base + '/rebind', {});
        var msg = (r && r.message) || '已刷新';
        toast(msg, (r && r.failed) ? 'warn' : 'success', 6000);
        await load();
        await refreshHealth();
      } catch (e) {
        /* api 已提示 */
      } finally {
        if (card) card.classList.remove('is-busy');
      }
    }

    async function deleteRecording() {
      var rec = ui.rec;
      if (!rec) return;
      var ok = await confirmDialog({
        title: '删除录制',
        message: '确定删除「' + (rec.name || rec.id) + '」吗？步骤会一并删除，不可恢复。',
        confirmText: '删除'
      });
      if (!ok) return;
      try {
        await api('DELETE', base);
        toast('已删除', 'success');
        await refreshHealth();
        navigate('#/recordings');
      } catch (e) { /* api 已提示 */ }
    }

    async function saveStep(stepId) {
      var step = findStep(stepId);
      var d = ui.drafts[stepId];
      if (!step || !d) return;
      var patch = {};
      STEP_FIELDS.forEach(function (f) {
        var sv = step[f] === null || step[f] === undefined ? '' : String(step[f]);
        var dv = d[f] === null || d[f] === undefined ? '' : String(d[f]);
        if (sv !== dv) patch[f] = dv;
      });
      if (!Object.keys(patch).length) { toast('这一行没有修改', 'info'); return; }
      try {
        await api('PATCH', base + '/steps/' + encodeURIComponent(stepId), patch);
        toast('步骤已保存', 'success');
        await load();
      } catch (e) { /* api 已提示 */ }
    }

    async function moveStep(stepId, delta) {
      var rec = ui.rec;
      if (!rec) return;
      var ids = (rec.steps || []).map(function (s) { return s.id; });
      var from = ids.indexOf(stepId);
      var to = from + delta;
      if (from < 0 || to < 0 || to >= ids.length) return;
      var tmp = ids[from];
      ids[from] = ids[to];
      ids[to] = tmp;
      try {
        await api('POST', base + '/reorder', { step_ids: ids });
        toast('步骤顺序已更新', 'success');
        await load();
      } catch (e) { /* api 已提示 */ }
    }

    async function deleteStep(stepId) {
      var step = findStep(stepId);
      var ok = await confirmDialog({
        title: '删除步骤',
        message: '确定删除步骤「' + (step && step.annotation ? step.annotation : stepId) + '」吗？',
        confirmText: '删除'
      });
      if (!ok) return;
      try {
        await api('DELETE', base + '/steps/' + encodeURIComponent(stepId));
        toast('步骤已删除', 'success');
        await load();
      } catch (e) { /* api 已提示 */ }
    }

    async function generateDataset() {
      var rec = ui.rec;
      if (!rec) return;
      ui.generating = true;
      paintDataCard();
      try {
        var res = await api('POST', '/api/ai/dataset', {
          recording_id: rec.id,
          instruction: ui.instruction
        });
        var fields = (res && Array.isArray(res.fields)) ? res.fields : [];
        ui.fields = fields;
        ui.source = (res && res.source) ? res.source : 'rules';
        ui.warning = (res && res.warning) ? res.warning : '';

        var ds = Object.assign({}, (res && res.dataset) || {});
        fields.forEach(function (f) {
          if (ds[f.key] === undefined || ds[f.key] === '') ds[f.key] = f.value;
        });
        // 保留用户手动补过的值
        Object.keys(ui.dataset).forEach(function (k) {
          var v = ui.dataset[k];
          if (v !== undefined && String(v) !== '' && (ds[k] === undefined || ds[k] === '')) ds[k] = v;
        });
        ui.dataset = ds;

        if (ui.source === 'rules') toast('未配置大模型，已用内置规则生成', 'info');
        else toast('已生成测试数据', 'success');
      } catch (e) {
        ui.warning = e.message || '生成失败';
      }
      ui.generating = false;
      paintDataCard();
    }

    async function runTest() {
      var rec = ui.rec;
      if (!rec) return;

      // 缺失字段提醒（不强制拦截）
      var req = requiredKeys(rec.steps || []);
      var missing = req.filter(function (r) {
        return String(ui.dataset[r.key] === null || ui.dataset[r.key] === undefined ? '' : ui.dataset[r.key]).trim() === '';
      }).map(function (r) { return r.key; });
      if (missing.length) {
        var go = await confirmDialog({
          title: '字段缺少数据',
          message: '还有 ' + missing.length + ' 个字段没有数据（' + missing.join('、') + '），仍然执行吗？',
          confirmText: '仍然执行',
          danger: false
        });
        if (!go) return;
      }

      ui.running = true;
      paintDataCard();
      try {
        var run = await api('POST', '/api/runs', {
          recording_id: rec.id,
          dataset: ui.dataset,
          dataset_source: ui.source,
          ai: { enabled: true, instruction: ui.instruction || '' },
          mode: 'server'
        });
        toast('已提交执行，正在跳转运行详情…', 'success');
        ui.running = false;
        await refreshHealth();
        if (run && run.id) navigate('#/runs/' + encodeURIComponent(run.id));
      } catch (e) {
        ui.running = false;
        paintDataCard();
      }
    }

    /* ---- 事件（委托在 root 上，重绘不丢监听） ---- */

    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');

      if (act === 'reload') { load(); return; }
      if (act === 'rebind') { rebind(); return; }

      if (act === 'toggle-loc') {
        var stepId = btn.getAttribute('data-step');
        if (!stepId) {
          ui.expandAll = !ui.expandAll;
          ui.overrides = {};
          paint();
          return;
        }
        var step = findStep(stepId);
        if (!step) return;
        ui.overrides[stepId] = !isLocOpen(step);
        // 只重绘该单元格，避免其它行输入框失焦
        var rows = $$('tr[data-step]', root);
        for (var i = 0; i < rows.length; i++) {
          if (rows[i].getAttribute('data-step') === stepId) {
            var cell = rows[i].querySelector('td[data-cell="locator"]');
            if (cell) cell.innerHTML = locatorCellHtml(step, isLocOpen(step));
            break;
          }
        }
        return;
      }

      if (act === 'toggle-value') {
        ui.showValue = !ui.showValue;
        var table = $('.table--steps', root);
        if (table) table.classList.toggle('hide-value', !ui.showValue);
        btn.textContent = ui.showValue ? '隐藏「输入值」列' : '显示「输入值」列';
        return;
      }

      if (act === 'head-save') { saveHeader(); return; }
      if (act === 'duplicate') { duplicateRecording(); return; }
      if (act === 'rec-delete') { deleteRecording(); return; }
      if (act === 'step-save') { saveStep(btn.getAttribute('data-step')); return; }
      if (act === 'step-up') { moveStep(btn.getAttribute('data-step'), -1); return; }
      if (act === 'step-down') { moveStep(btn.getAttribute('data-step'), 1); return; }
      if (act === 'step-del') { deleteStep(btn.getAttribute('data-step')); return; }
      if (act === 'gen-dataset') { generateDataset(); return; }
      if (act === 'run-start') { runTest(); return; }
    });

    root.addEventListener('input', onFieldInput);
    root.addEventListener('change', onFieldInput);

    function onFieldInput(e) {
      var t = e.target;
      if (!t || t.nodeType !== 1) return;

      // 数据准备指令
      if (t.id === 'ds-instruction') {
        ui.instruction = t.value;
        return;
      }

      // 数据字段值（AI 结果表 / 覆盖率补值）
      var dsKey = t.getAttribute('data-dskey');
      if (dsKey) {
        ui.dataset[dsKey] = t.value;
        var inCoverage = !!(t.closest && t.closest('#ds-coverage'));
        if (inCoverage) {
          var li = t.closest('li');
          if (li) {
            var mark = li.querySelector('.ok, .miss');
            var okNow = String(t.value).trim() !== '';
            if (mark) {
              mark.className = okNow ? 'ok' : 'miss';
              mark.textContent = okNow ? '✓ 已填' : '✗ 缺失';
            }
          }
          updateCoverageCounter();
        } else {
          // 字段表里的改动：同步覆盖率列表
          var box = $('#ds-coverage', root);
          if (box && ui.rec) box.outerHTML = coverageBlockHtml(ui.rec.steps || []);
        }
        return;
      }

      // 步骤行内编辑
      var fieldName = t.getAttribute('data-edit');
      if (!fieldName) return;
      var tr = t.closest('tr[data-step]');
      if (!tr) return;
      var sid = tr.getAttribute('data-step');
      if (!ui.drafts[sid]) ui.drafts[sid] = { annotation: '', value: '', data_key: '', expect: '' };
      ui.drafts[sid][fieldName] = t.value;

      var step = findStep(sid);
      var dirty = isDirty(step);
      tr.classList.toggle('is-dirty', dirty);
      var saveBtn = tr.querySelector('[data-act="step-save"]');
      if (saveBtn) saveBtn.classList.toggle('btn--primary', dirty);

      // 输入值列：{{变量}} 高亮实时更新
      if (fieldName === 'value') {
        var cell = t.closest('.value-cell');
        if (cell) {
          var line = cell.querySelector('.var-line');
          if (hasVars(t.value)) {
            if (!line) {
              line = document.createElement('div');
              line.className = 'var-line';
              cell.appendChild(line);
            }
            line.innerHTML = highlightVars(t.value);
          } else if (line) {
            line.parentNode.removeChild(line);
          }
        }
      }
    }

    load();
  }

  /* ======================================================================
   * 6.3 运行列表  #/runs（每 3 秒轮询）
   * ====================================================================== */

  function renderRuns(app) {
    var root = pageRoot(app);
    var state = { items: [], timer: null };

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>运行</h1><p class="sub">最近 50 次执行，自动刷新（每 3 秒）；点录制名查看实时日志</p></div>' +
      '<div class="head-actions">' +
      '<span class="muted" id="poll-hint">自动刷新中</span>' +
      '<button class="btn" data-act="refresh">刷新</button>' +
      '</div></div>' +
      '<section class="card">' +
      '<div class="table-wrap"><table class="table">' +
      '<thead><tr>' +
      '<th style="width:104px">状态</th>' +
      '<th>录制名称</th>' +
      '<th style="width:104px">数据来源</th>' +
      '<th class="num" style="width:104px">通过/总步数</th>' +
      '<th style="width:96px">耗时</th>' +
      '<th style="width:130px">开始时间</th>' +
      '<th class="actions-cell" style="width:140px">操作</th>' +
      '</tr></thead>' +
      '<tbody id="runs-body"><tr><td colspan="7" class="cell-state">加载中…</td></tr></tbody>' +
      '</table></div>' +
      '</section>';

    var tbody = $('#runs-body', root);
    var hint = $('#poll-hint', root);

    function rowHtml(run) {
      var rid = encodeURIComponent(run.id);
      return '<tr>' +
        '<td>' + runStatusBadge(run.status) + '</td>' +
        '<td class="cell-name">' +
        '<a class="strong" href="#/runs/' + rid + '">' + esc(run.recording_name || run.recording_id || '(录制已删除)') + '</a>' +
        '<div class="cell-sub mono">' + esc(run.id) + '</div>' +
        '</td>' +
        '<td><span class="chip">' + esc(sourceLabel(run.dataset_source)) + '</span></td>' +
        '<td class="num mono">' + esc(stepCounts(run)) + '</td>' +
        '<td class="mono nowrap">' + esc(runDuration(run)) + '</td>' +
        '<td class="nowrap muted" title="' + esc(fmtDateTime(run.started_at)) + '">' + esc(relTime(run.started_at || run.created_at)) + '</td>' +
        '<td class="actions-cell">' +
        '<a class="btn btn--sm" href="#/runs/' + rid + '">查看</a>' +
        '<button class="btn btn--sm" data-act="rerun" data-id="' + esc(run.id) + '">重跑</button>' +
        '</td>' +
        '</tr>';
    }

    function paint() {
      if (!state.items.length) {
        tbody.innerHTML = '<tr><td colspan="7" class="cell-state" style="padding:0">' +
          emptyCard('还没有运行记录',
            '进入某条录制详情，在「数据准备」面板点「执行测试」即可提交一次运行。',
            '<a class="btn btn--primary" href="#/recordings">去录制列表</a>') + '</td></tr>';
        return;
      }
      tbody.innerHTML = state.items.map(rowHtml).join('');
    }

    async function load(silent, withLoading) {
      if (withLoading) tbody.innerHTML = '<tr><td colspan="7" class="cell-state">加载中…</td></tr>';
      try {
        var res = await api('GET', '/api/runs?limit=50', undefined, { silent: !!silent });
        state.items = apiItems(res);
        paint();
        if (hint) hint.textContent = '自动刷新中 · ' + fmtClock(new Date()).slice(0, 8);
      } catch (e) {
        if (!silent) {
          tbody.innerHTML = '<tr><td colspan="7" class="cell-state cell-state--error">' +
            esc(e.message || '加载失败') +
            '　<button class="btn btn--sm" data-act="refresh">重试</button></td></tr>';
          if (hint) hint.textContent = '刷新失败';
        }
      }
    }

    async function rerun(runId) {
      try {
        // 列表接口不返回 dataset，先取一次详情再重跑
        var detail = await api('GET', '/api/runs/' + encodeURIComponent(runId));
        if (!detail || !detail.recording_id) { toast('该运行没有关联录制，无法重跑', 'error'); return; }
        var run = await api('POST', '/api/runs', {
          recording_id: detail.recording_id,
          dataset: detail.dataset || {},
          dataset_source: detail.dataset_source || 'manual',
          mode: 'server'
        });
        toast('已重新提交执行', 'success');
        if (run && run.id) navigate('#/runs/' + encodeURIComponent(run.id));
        else load(false);
      } catch (e) { /* api 已提示 */ }
    }

    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');
      if (act === 'refresh') { load(false, true); return; }
      if (act === 'rerun') { rerun(btn.getAttribute('data-id')); return; }
    });

    load(false, true);
    state.timer = setInterval(function () { load(true); }, 3000);
    addCleanup(function () {
      if (state.timer) clearInterval(state.timer);
      state.timer = null;
    });
  }

  /* ======================================================================
   * 6.4 运行详情  #/runs/:id（SSE 实时日志 + 轮询降级）
   * ====================================================================== */

  function renderRunDetail(app, id) {
    var root = pageRoot(app);
    var apiPath = '/api/runs/' + encodeURIComponent(id);

    var ui = {
      run: null,
      autoScroll: true,
      seen: {},       // 日志去重 key
      logText: []     // 复制日志用
    };

    var es = null;
    var pollTimer = null;
    var refreshTimer = null;
    var transportStarted = false;
    var eventsEnded = false;
    var alive = true;

    addCleanup(function () {
      alive = false;
      if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
      if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
      if (refreshTimer) { clearTimeout(refreshTimer); refreshTimer = null; }
    });

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>运行详情</h1><p class="sub"><a href="#/runs">← 返回运行列表</a></p></div>' +
      '</div>' +
      '<div id="run-head">' + loadingCard() + '</div>' +
      '<div id="run-dataset"></div>' +
      '<div class="split">' +
      '<section class="card" id="run-steps">' +
      '<div class="card-head"><h2>逐步结果</h2><span class="muted" id="step-summary"></span></div>' +
      '<div id="run-steps-body"></div>' +
      '</section>' +
      '<aside class="card logs-card" id="logs-card">' +
      '<div class="card-head logs-head" id="logs-head" title="按住这一行可以拖动窗口">' +
      '<h2>实时日志<span class="logs-drag-hint" aria-hidden="true">⠿</span></h2>' +
      '<div class="logs-tools">' +
      '<label class="check-line"><input type="checkbox" id="log-autoscroll" checked><span>自动滚动</span></label>' +
      '<button class="btn btn--sm" data-act="copy-logs">复制日志</button>' +
      '<button class="btn btn--sm" data-act="collapse-logs" id="log-collapse-btn">收起</button>' +
      '</div></div>' +
      '<div class="logs-wrap"><div class="logs" id="logs"></div></div>' +
      '<div class="logs-foot" id="logs-foot">准备连接实时日志…</div>' +
      '</aside>' +
      '</div>';

    /* ---- 日志 ---- */

    function logKey(l) {
      return [l.ts || '', l.index === null || l.index === undefined ? '' : l.index, l.level || '', l.message || ''].join('|');
    }

    function plainLogText(l) {
      var parts = [];
      parts.push(l.ts ? fmtClock(l.ts) : fmtClock());
      parts.push('[' + String(l.level || 'info').toUpperCase() + ']');
      if (l.index !== null && l.index !== undefined && l.index !== '') parts.push('#' + l.index);
      parts.push(l.message || '');
      return parts.join(' ');
    }

    function appendLog(l) {
      var logsEl = $('#logs', root);
      if (!logsEl || !l) return;
      var key = logKey(l);
      if (l.ts && ui.seen[key]) return;   // 同一条日志（SSE + 详情重拉）只渲染一次
      if (l.ts) ui.seen[key] = true;

      var atBottom = (logsEl.scrollHeight - logsEl.scrollTop - logsEl.clientHeight) < 48;
      var line = '<div class="logline logline--' + levelClass(l.level) + '">' +
        '<span class="log-ts">' + esc(l.ts ? fmtClock(l.ts) : fmtClock()) + '</span>' +
        '<span class="log-lv">' + esc(String(l.level || 'info').toUpperCase()) + '</span>' +
        '<span class="log-idx">' + (l.index === null || l.index === undefined || l.index === '' ? '·' : '#' + esc(l.index)) + '</span>' +
        '<span class="log-msg">' + esc(l.message || '') + '</span>' +
        '</div>';
      logsEl.insertAdjacentHTML('beforeend', line);
      ui.logText.push(plainLogText(l));
      if (ui.autoScroll && atBottom) logsEl.scrollTop = logsEl.scrollHeight;
    }

    function setFoot(text) {
      var el = $('#logs-foot', root);
      if (el) el.textContent = text;
    }

    /* ---- 头部 / 数据 / 步骤渲染 ---- */

    function paintHead(run) {
      var host = $('#run-head', root);
      if (!host) return;
      if (!run) { host.innerHTML = ''; return; }

      var active = isActiveRun(run.status);
      var healed = (run.step_results || []).filter(function (s) { return s.healed; }).length;

      var actions = '';
      if (active) actions += '<button class="btn btn--sm btn--danger" data-act="cancel">取消运行</button>';
      if (healed) actions += '<button class="btn btn--sm" data-act="heal">把自愈定位写回录制（' + healed + '）</button>';
      actions += '<button class="btn btn--sm btn--primary" data-act="rerun">重跑</button>';

      var meta = '<dl class="meta-grid">' +
        '<div><dt>录制</dt><dd>' + (run.recording_id
          ? '<a href="#/recordings/' + encodeURIComponent(run.recording_id) + '">' + esc(run.recording_name || run.recording_id) + '</a>'
          : esc(run.recording_name || '—')) + '</dd></div>' +
        '<div><dt>数据来源</dt><dd><span class="chip">' + esc(sourceLabel(run.dataset_source)) + '</span></dd></div>' +
        '<div><dt>模式</dt><dd class="mono">' + esc(run.mode || 'server') + '</dd></div>' +
        '<div><dt>开始时间</dt><dd class="mono">' + esc(fmtDateTime(run.started_at)) + '</dd></div>' +
        '<div><dt>结束时间</dt><dd class="mono">' + esc(fmtDateTime(run.finished_at)) + '</dd></div>' +
        '<div><dt>耗时</dt><dd class="mono">' + esc(runDuration(run)) + '</dd></div>' +
        '</dl>';

      host.innerHTML =
        '<section class="card run-head">' +
        '<div class="run-head-top">' +
        '<div class="run-title"><h1>运行</h1>' + runStatusBadge(run.status) +
        '<span class="run-id">' + esc(run.id) + '</span></div>' +
        '<div class="head-actions">' + actions + '</div>' +
        '</div>' +
        meta +
        (run.error ? '<div style="padding:0 16px 16px"><div class="alert alert--error"><strong>错误：</strong>' + esc(run.error) + '</div></div>' : '') +
        '</section>';
    }

    function paintDataset(run) {
      var host = $('#run-dataset', root);
      if (!host) return;
      var dataset = (run && run.dataset) || {};
      var keys = Object.keys(dataset);
      if (!keys.length) {
        host.innerHTML = '<section class="card"><div class="card-head"><h2>数据准备结果</h2></div>' +
          emptyCard('本次运行没有数据集', '步骤中没有需要填充的字段，或提交时未传 dataset。') + '</section>';
        return;
      }
      var rows = keys.map(function (k) {
        return '<tr><td class="mono">' + esc(k) + '</td>' +
          '<td class="mono">' + esc(dataset[k]) + '</td>' +
          '<td><span class="chip">' + esc(sourceLabel(run.dataset_source)) + '</span></td></tr>';
      }).join('');
      host.innerHTML = '<section class="card"><div class="card-head"><h2>数据准备结果 <span class="count-pill">' + keys.length + '</span></h2></div>' +
        '<div class="table-wrap"><table class="table">' +
        '<thead><tr><th style="width:30%">key</th><th>value</th><th style="width:140px">来源</th></tr></thead>' +
        '<tbody>' + rows + '</tbody></table></div></section>';
    }

    function stepStatusCell(status) {
      if (status === 'passed') return '<span class="step-mark step-mark--ok" title="通过">✓</span>';
      if (status === 'failed') return '<span class="step-mark step-mark--fail" title="失败">✗</span>';
      return '<span class="step-mark step-mark--skip" title="跳过">-</span>';
    }

    function paintSteps(run) {
      var bodyEl = $('#run-steps-body', root);
      var summaryEl = $('#step-summary', root);
      if (!bodyEl) return;

      var results = (run && run.step_results) || [];
      if (summaryEl) {
        var passed = results.filter(function (r) { return r.status === 'passed'; }).length;
        summaryEl.textContent = results.length ? ('共 ' + results.length + ' 步 · 通过 ' + passed) : '';
      }

      if (!results.length) {
        bodyEl.innerHTML = emptyCard(
          '还没有步骤结果',
          isActiveRun(run.status) ? '执行中，稍等片刻…' : '本次运行没有产生步骤结果。');
        return;
      }

      var rows = results.map(function (r) {
        var shot = r.screenshot
          ? '<button class="btn btn--sm" data-act="shot" data-path="' + esc(r.screenshot) + '">查看截图</button>'
          : '<span class="muted">—</span>';
        return '<tr>' +
          '<td class="mono">' + esc(r.index === null || r.index === undefined ? '' : r.index) + '</td>' +
          '<td>' + stepStatusCell(r.status) + '</td>' +
          '<td><span class="action-tag">' + esc(actionLabel(r.action)) + '</span></td>' +
          '<td>' + esc(r.annotation || '') + '</td>' +
          '<td><code class="loc-code">' + esc(r.locator_used || '—') + '</code>' +
          (r.healed ? ' <span class="badge badge--healed">自愈</span>' : '') + '</td>' +
          '<td class="mono">' + esc(r.value_used === null || r.value_used === undefined || r.value_used === '' ? '—' : r.value_used) + '</td>' +
          '<td class="mono nowrap">' + esc(r.duration_ms === null || r.duration_ms === undefined ? '—' : r.duration_ms + ' ms') + '</td>' +
          '<td>' + (r.message ? '<span class="' + (r.status === 'failed' ? 'msg-error' : 'msg-ok') + '">' + esc(r.message) + '</span>' : '<span class="muted">—</span>') + '</td>' +
          '<td class="actions-cell">' + shot + '</td>' +
          '</tr>';
      }).join('');

      bodyEl.innerHTML = '<div class="table-wrap"><table class="table table--steps">' +
        '<thead><tr>' +
        '<th style="width:48px">序号</th>' +
        '<th style="width:54px">状态</th>' +
        '<th style="width:84px">动作</th>' +
        '<th>备注</th>' +
        '<th style="min-width:160px">实际使用定位</th>' +
        '<th style="min-width:110px">使用值</th>' +
        '<th style="width:80px">耗时</th>' +
        '<th>消息</th>' +
        '<th class="actions-cell" style="width:104px">截图</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
    }

    function paintAll() {
      var run = ui.run;
      if (!run) return;
      paintHead(run);
      paintDataset(run);
      paintSteps(run);
      if (Array.isArray(run.logs) && run.logs.length) {
        run.logs.forEach(function (l) { appendLog(l); });
      }
    }

    /* ---- 详情刷新 ---- */

    async function refresh(opts) {
      opts = opts || {};
      try {
        var run = await api('GET', apiPath, undefined, { silent: opts.silent !== false });
        ui.run = run;
        paintAll();
        if (isActiveRun(run.status)) {
          startTransport();
        } else if (es) {
          try { es.close(); } catch (e) { /* ignore */ }
          es = null;
          setFoot('执行已结束');
        }
        return run;
      } catch (e) {
        if (!ui.run) {
          var host = $('#run-head', root);
          if (host) host.innerHTML = errorCard(e.message || '无法加载运行详情');
        }
        return null;
      }
    }

    function scheduleRefresh() {
      if (refreshTimer) return;
      refreshTimer = setTimeout(function () {
        refreshTimer = null;
        refresh({ silent: true });
      }, 700);
    }

    /* ---- SSE ---- */

    function handleEvent(ev) {
      var type = ev && ev.type ? ev.type : 'log';

      if (type === 'end') {
        eventsEnded = true;
        if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
        setFoot('执行结束');
        refresh({ silent: true });
        return;
      }

      if (type === 'status') {
        if (ui.run && ev.status) {
          ui.run.status = ev.status;
          paintHead(ui.run);
        }
        if (ev.status) {
          var m = RUN_STATUS[ev.status];
          appendLog({
            ts: ev.ts,
            level: ev.status === 'failed' ? 'error' : 'info',
            message: '状态变更：' + (m ? m.label : ev.status) + (ev.message ? '（' + ev.message + '）' : '')
          });
        }
        return;
      }

      if (type === 'step') {
        var sr = ev.step_result || ev.step || null;
        if (sr && typeof sr === 'object') {
          var mark = sr.status === 'passed' ? '✓' : (sr.status === 'failed' ? '✗' : '-');
          appendLog({
            ts: ev.ts,
            level: sr.status === 'failed' ? 'error' : (sr.status === 'passed' ? 'success' : 'info'),
            index: sr.index,
            message: mark + ' ' + (sr.annotation || actionLabel(sr.action)) +
              (sr.duration_ms === null || sr.duration_ms === undefined ? '' : '（' + sr.duration_ms + 'ms）')
          });
        }
        scheduleRefresh();
        return;
      }

      // 默认按日志处理
      var log = ev.log || ev;
      appendLog({
        ts: log.ts,
        level: log.level || 'info',
        index: log.index,
        message: log.message || ''
      });
    }

    function startEvents() {
      var foot = $('#logs-foot', root);
      if (typeof window.EventSource !== 'function') {
        setFoot('当前浏览器不支持 EventSource，已改为轮询（1.5s）');
        startPolling();
        return;
      }
      try {
        es = new EventSource(apiPath + '/events');
      } catch (e) {
        setFoot('实时日志连接失败，已改为轮询（1.5s）');
        startPolling();
        return;
      }
      setFoot('正在连接实时日志…');

      es.onopen = function () {
        if (!alive) return;
        setFoot('实时日志已连接（SSE）');
      };
      es.onmessage = function (event) {
        if (!alive) return;
        var data;
        try { data = JSON.parse(event.data); }
        catch (e) { return; }
        handleEvent(data);
      };
      es.onerror = function () {
        if (!alive || eventsEnded) return;
        if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
        setFoot('实时日志连接中断，已改为轮询刷新（1.5s）');
        startPolling();
      };
    }

    function startPolling() {
      if (pollTimer || eventsEnded) return;
      pollTimer = setInterval(async function () {
        if (!alive) return;
        var run = await refresh({ silent: true });
        if (run && !isActiveRun(run.status)) {
          clearInterval(pollTimer);
          pollTimer = null;
          setFoot('执行已结束（轮询已停止）');
        }
      }, 1500);
    }

    // 只在运行中才建立实时通道
    function startTransport() {
      if (transportStarted || eventsEnded) return;
      transportStarted = true;
      startEvents();
    }

    /* ---- 按钮动作 ---- */

    async function cancelRun() {
      try {
        await api('POST', apiPath + '/cancel', {});
        toast('已请求取消', 'success');
        await refresh({ silent: false });
      } catch (e) { /* api 已提示 */ }
    }

    async function healRun() {
      var ok = await confirmDialog({
        title: '写回自愈定位',
        message: '把本次运行中自愈命中的新定位写回录制？写回后下次执行会直接使用新定位。',
        confirmText: '写回',
        danger: false
      });
      if (!ok) return;
      try {
        var res = await api('POST', apiPath + '/heal', {});
        var n = res && res.updated !== undefined ? res.updated : 0;
        toast('已更新 ' + n + ' 条定位', 'success');
        await refresh({ silent: false });
      } catch (e) { /* api 已提示 */ }
    }

    async function rerun() {
      var run = ui.run;
      if (!run) return;
      if (!run.recording_id) { toast('该运行没有关联录制，无法重跑', 'error'); return; }
      try {
        var created = await api('POST', '/api/runs', {
          recording_id: run.recording_id,
          dataset: run.dataset || {},
          dataset_source: run.dataset_source || 'manual',
          mode: 'server'
        });
        toast('已重新提交执行', 'success');
        if (created && created.id) navigate('#/runs/' + encodeURIComponent(created.id));
      } catch (e) { /* api 已提示 */ }
    }

    function showShot(path) {
      if (!path) return;
      var p = String(path).replace(/^\/+/, '');
      openModal({
        title: '步骤截图',
        width: 960,
        body: '<div class="shot-wrap"><img src="/' + esc(p) + '" alt="步骤截图 ' + esc(p) + '"></div>' +
          '<p class="field-hint" style="margin-top:8px"><span class="mono">' + esc(path) + '</span></p>',
        footer: '<a class="btn" href="/' + esc(p) + '" target="_blank" rel="noopener">新窗口打开</a>' +
          '<button class="btn btn--primary" data-modal-close>关闭</button>'
      });
    }

    async function copyLogs() {
      var text = ui.logText.join('\n');
      if (!text) { toast('暂无日志可复制', 'info'); return; }
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          await navigator.clipboard.writeText(text);
        } else {
          var ta = document.createElement('textarea');
          ta.value = text;
          ta.style.position = 'fixed';
          ta.style.opacity = '0';
          document.body.appendChild(ta);
          ta.select();
          document.execCommand('copy');
          if (ta.parentNode) ta.parentNode.removeChild(ta);
        }
        toast('日志已复制（' + ui.logText.length + ' 行）', 'success');
      } catch (e) {
        toast('复制失败，请手动选择日志文本', 'error');
      }
    }

    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');
      if (act === 'cancel') { cancelRun(); return; }
      if (act === 'heal') { healRun(); return; }
      if (act === 'rerun') { rerun(); return; }
      if (act === 'copy-logs') { copyLogs(); return; }

      if (act === 'collapse-logs') { setLogCollapsed(!logCard.classList.contains('is-collapsed')); return; }
      if (act === 'shot') { showShot(btn.getAttribute('data-path')); return; }
    });

    // 自动滚动开关只改状态，不重绘
    var autoBox = $('#log-autoscroll', root);
    if (autoBox) {
      autoBox.addEventListener('change', function () {
        ui.autoScroll = !!autoBox.checked;
        if (ui.autoScroll) {
          var logsEl = $('#logs', root);
          if (logsEl) logsEl.scrollTop = logsEl.scrollHeight;
        }
      });
    }

    /* ---- 实时日志浮窗：浮动 / 拖动 / 缩放 / 收起 ----
     * 需求：实时日志窗口可以变成悬浮窗并拖动。
     * 实现要点：
     *   1) 只切一个 class + 内联 left/top/width/height，不重建节点（日志是靠 appendLog 累加的，
     *      重建会丢内容、也丢滚动位置）；
     *   2) 位置/尺寸存 localStorage，下次进来还是老位置；拖出可视区会被夹回来；
     *   3) 拖动只认标题栏（按钮/复选框不算），用 pointer 事件，移动中不重排布局。
     */

    var LOG_POS_KEY = 'uitest.logPos';       // {l,t,w,h}
    var logCard = $('#logs-card', root);
    var logHead = $('#logs-head', root);
    var dragState = null;

    function lsGet(key) {
      try { return localStorage.getItem(key); } catch (e) { return null; }
    }
    function lsSet(key, val) {
      try { localStorage.setItem(key, val); } catch (e) { /* 隐私模式忽略 */ }
    }
    function loadPos() {
      try {
        var v = JSON.parse(lsGet(LOG_POS_KEY) || 'null');
        return v && typeof v === 'object' ? v : null;
      } catch (e) { return null; }
    }
    function clampNum(v, min, max) {
      return Math.max(min, Math.min(max, v));
    }

    function isLogFloating() {
      return !!(logCard && logCard.classList.contains('is-floating'));
    }

    function setLogFloating(on, silent) {
      if (!logCard) return;
      if (on) {
        var pos = loadPos() || {};
        var vw = window.innerWidth, vh = window.innerHeight;
        var width = clampNum(pos.w || Math.round(vw * 0.34), 320, Math.max(320, vw - 16));
        var height = clampNum(pos.h || Math.round(vh * 0.42), 220, Math.max(220, vh - 16));
        var left = typeof pos.l === 'number' ? pos.l : (vw - width - 24);
        var top = typeof pos.t === 'number' ? pos.t : (vh - height - 24);
        logCard.classList.add('is-floating');
        logCard.classList.remove('is-collapsed');
        logCard.style.width = width + 'px';
        logCard.style.height = height + 'px';
        logCard.style.left = clampNum(left, 4, Math.max(4, vw - width - 4)) + 'px';
        logCard.style.top = clampNum(top, 4, Math.max(4, vh - height - 4)) + 'px';
        setLogCollapsed(false);
      } else {
        logCard.classList.remove('is-floating');
        logCard.style.width = '';
        logCard.style.height = '';
        logCard.style.left = '';
        logCard.style.top = '';
      }
      // 浮窗脱离文档流后，外层两列网格会留一块空白，这里切成单列让步骤表占满
      var grid = logCard.parentElement;
      if (grid && grid.classList.contains('split')) grid.classList.toggle('has-floating-log', !!on);

      // 浮窗出现/消失后把日志滚到底，避免停在半截
      var logsEl = $('#logs', root);
      if (logsEl && ui.autoScroll) logsEl.scrollTop = logsEl.scrollHeight;
    }

    function setLogCollapsed(on) {
      if (!logCard) return;
      logCard.classList.toggle('is-collapsed', !!on);
      var b = $('#log-collapse-btn', root);
      if (b) b.textContent = on ? '展开' : '收起';
    }

    function saveLogPos() {
      if (!logCard || !isLogFloating()) return;
      var r = logCard.getBoundingClientRect();
      lsSet(LOG_POS_KEY, JSON.stringify({
        l: Math.round(r.left), t: Math.round(r.top),
        w: Math.round(r.width), h: Math.round(r.height)
      }));
    }

    function onLogDragMove(ev) {
      if (!dragState || !logCard) return;
      var rect = logCard.getBoundingClientRect();
      var left = clampNum(ev.clientX - dragState.dx, 4, Math.max(4, window.innerWidth - rect.width - 4));
      var top = clampNum(ev.clientY - dragState.dy, 4, Math.max(4, window.innerHeight - rect.height - 4));
      logCard.style.left = left + 'px';
      logCard.style.top = top + 'px';
    }

    function onLogDragEnd() {
      document.removeEventListener('pointermove', onLogDragMove);
      document.removeEventListener('pointerup', onLogDragEnd);
      document.removeEventListener('pointercancel', onLogDragEnd);
      if (logCard) logCard.classList.remove('is-dragging');
      if (dragState) saveLogPos();
      dragState = null;
    }

    if (logHead && logCard) {
      logHead.addEventListener('pointerdown', function (ev) {
        if (!isLogFloating()) return;            // 只在悬浮时可拖（本面板始终悬浮）
        if (ev.button !== 0) return;
        if (ev.target.closest('button, input, label, a')) return;  // 标题栏上的控件不触发拖动
        var rect = logCard.getBoundingClientRect();
        dragState = { dx: ev.clientX - rect.left, dy: ev.clientY - rect.top };
        logCard.classList.add('is-dragging');
        document.addEventListener('pointermove', onLogDragMove);
        document.addEventListener('pointerup', onLogDragEnd);
        document.addEventListener('pointercancel', onLogDragEnd);
        ev.preventDefault();
      });

      // 双击标题栏：收起 / 展开
      logHead.addEventListener('dblclick', function (ev) {
        if (ev.target.closest('button, input, label, a')) return;
        setLogCollapsed(!logCard.classList.contains('is-collapsed'));
      });

      // 手动拖右下角缩放后记住尺寸
      if (typeof window.ResizeObserver === 'function') {
        var roTimer = null;
        var ro = new ResizeObserver(function () {
          if (!isLogFloating()) return;
          if (roTimer) clearTimeout(roTimer);
          roTimer = setTimeout(saveLogPos, 300);
        });
        ro.observe(logCard);
      }

      // 窗口尺寸变化时把浮窗夹回可视区
      window.addEventListener('resize', function () {
        if (!isLogFloating()) return;
        var r = logCard.getBoundingClientRect();
        logCard.style.left = clampNum(r.left, 4, Math.max(4, window.innerWidth - r.width - 4)) + 'px';
        logCard.style.top = clampNum(r.top, 4, Math.max(4, window.innerHeight - r.height - 4)) + 'px';
      });
    }

    addCleanup(function () {
      onLogDragEnd();
    });

    // 实时日志面板始终悬浮（可拖动/缩放/收起，位置与尺寸记忆）
    setLogFloating(true, true);

    refresh({ silent: false });
  }

  /* ======================================================================
   * 6.5 元素库  #/elements
   * ====================================================================== */

  function renderElements(app) {
    var root = pageRoot(app);
    var state = { items: [], page: '', q: '' };

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>元素库</h1><p class="sub">稳定 ID（data-uitest-id）绑定索引</p></div>' +
      '<div class="head-actions"><button class="btn" data-act="refresh">刷新</button></div>' +
      '</div>' +
      '<section class="card"><div class="card-body">' +
      '<p class="field-hint" style="font-size:12.5px;line-height:1.8">' +
      '元素库是服务端维护的「元素 ↔ 页面」稳定绑定表：录制时插件会把 <span class="mono">data-uitest-id</span>、所在页面、选择器与描述登记到这里；' +
      '执行时如果步骤的 <span class="mono">uid</span> 定位仍然命中，就说明这条绑定还有效（运行详情里会显示绿色「ID 绑定」）。' +
      '若某条元素被标记为「失效」或命中次数长期为 0，建议重新录制该步骤，或改用备用定位（fallbacks）。' +
      '</p></div></section>' +
      '<section class="card">' +
      '<div class="card-toolbar">' +
      '<div class="search"><span class="search-icon" aria-hidden="true">' +
      '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">' +
      '<circle cx="11" cy="11" r="7"></circle><path d="M20 20l-3.6-3.6"></path></svg></span>' +
      '<input id="el-search" class="input" type="search" placeholder="按 key / 描述 / 标签过滤" autocomplete="off"></div>' +
      '<select id="el-page" class="input" style="max-width:280px"><option value="">全部页面</option></select>' +
      '<div class="toolbar-right muted" id="el-count"></div>' +
      '</div>' +
      '<div class="table-wrap"><table class="table">' +
      '<thead><tr>' +
      '<th style="width:150px">key</th>' +
      '<th style="width:22%">页面</th>' +
      '<th>描述 descriptor</th>' +
      '<th style="width:190px">标签</th>' +
      '<th class="num" style="width:80px">命中次数</th>' +
      '<th style="width:120px">最近使用</th>' +
      '<th style="width:90px">状态</th>' +
      '<th class="actions-cell" style="width:80px">操作</th>' +
      '</tr></thead>' +
      '<tbody id="el-body"><tr><td colspan="8" class="cell-state">加载中…</td></tr></tbody>' +
      '</table></div>' +
      '</section>';

    var tbody = $('#el-body', root);
    var countEl = $('#el-count', root);
    var pageSel = $('#el-page', root);
    var searchEl = $('#el-search', root);

    function filtered() {
      var kw = String(state.q || '').trim().toLowerCase();
      return state.items.filter(function (it) {
        if (state.page && it.page !== state.page) return false;
        if (!kw) return true;
        var hay = [it.key, it.descriptor, it.label, it.tag, it.type, it.selector, it.page].join(' ').toLowerCase();
        return hay.indexOf(kw) >= 0;
      });
    }

    function rowHtml(el) {
      var tags = [];
      if (el.tag) tags.push(el.tag);
      if (el.type) tags.push(el.type);
      if (el.label) tags.push(el.label);
      return '<tr>' +
        '<td class="mono">' + esc(el.key) + '</td>' +
        '<td class="mono" style="word-break:break-all">' + esc(el.page || '—') + '</td>' +
        '<td>' + esc(el.descriptor || el.selector || '—') +
        (el.selector && el.descriptor ? '<div class="cell-sub mono">' + esc(el.selector) + '</div>' : '') + '</td>' +
        '<td>' + (tags.length ? tags.map(function (t) { return '<span class="chip">' + esc(t) + '</span>'; }).join('') : '<span class="muted">—</span>') + '</td>' +
        '<td class="num mono">' + esc(el.hits === null || el.hits === undefined ? 0 : el.hits) + '</td>' +
        '<td class="nowrap muted" title="' + esc(fmtDateTime(el.last_seen)) + '">' + esc(relTime(el.last_seen)) + '</td>' +
        '<td>' + (el.broken
          ? '<span class="badge badge--failed"><i class="dot"></i>失效</span>'
          : '<span class="badge badge--passed"><i class="dot"></i>正常</span>') + '</td>' +
        '<td class="actions-cell"><button class="btn btn--sm btn--danger" data-act="del" data-key="' + esc(el.key) + '">删除</button></td>' +
        '</tr>';
    }

    function paintPageOptions() {
      var pages = [];
      state.items.forEach(function (it) {
        if (it.page && pages.indexOf(it.page) < 0) pages.push(it.page);
      });
      pages.sort();
      var html = '<option value="">全部页面</option>' + pages.map(function (p) {
        return '<option value="' + esc(p) + '"' + (p === state.page ? ' selected' : '') + '>' + esc(p) + '</option>';
      }).join('');
      pageSel.innerHTML = html;
    }

    function paint() {
      var items = filtered();
      countEl.textContent = state.items.length ? ('共 ' + items.length + ' / ' + state.items.length + ' 条') : '';
      if (!state.items.length) {
        tbody.innerHTML = '<tr><td colspan="8" class="cell-state" style="padding:0">' +
          emptyCard('元素库还是空的',
            '在浏览器里用插件录制一次，插件会顺手把操作过的元素登记到元素库。',
            '<a class="btn btn--primary" href="#/recordings">去录制列表</a>') + '</td></tr>';
        return;
      }
      if (!items.length) {
        tbody.innerHTML = '<tr><td colspan="8" class="cell-state">没有匹配的元素，换个关键词或页面筛选项。</td></tr>';
        return;
      }
      tbody.innerHTML = items.map(rowHtml).join('');
    }

    async function load() {
      try {
        var res = await api('GET', '/api/elements');
        state.items = apiItems(res).slice().sort(function (a, b) {
          var ta = toDate(a.last_seen);
          var tb = toDate(b.last_seen);
          return (tb ? tb.getTime() : 0) - (ta ? ta.getTime() : 0);
        });
        paintPageOptions();
        paint();
      } catch (e) {
        tbody.innerHTML = '<tr><td colspan="8" class="cell-state cell-state--error">' +
          esc(e.message || '加载失败') +
          '　<button class="btn btn--sm" data-act="refresh">重试</button></td></tr>';
      }
    }

    async function removeElement(key) {
      var ok = await confirmDialog({
        title: '删除元素绑定',
        message: '确定删除元素「' + key + '」的绑定吗？删除后使用该 uid 的步骤需要重新录制或依赖备用定位。',
        confirmText: '删除'
      });
      if (!ok) return;
      try {
        await api('DELETE', '/api/elements/' + encodeURIComponent(key));
        toast('已删除', 'success');
        await load();
      } catch (e) { /* api 已提示 */ }
    }

    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');
      if (act === 'refresh') { load(); return; }
      if (act === 'del') { removeElement(btn.getAttribute('data-key')); return; }
    });

    searchEl.addEventListener('input', function () {
      state.q = searchEl.value;
      paint();
    });
    pageSel.addEventListener('change', function () {
      state.page = pageSel.value;
      paint();
    });

    load();
  }

  /* ======================================================================
   * 6.6 设置  #/settings
   * ====================================================================== */

  function renderSettings(app) {
    var root = pageRoot(app);
    var state = { data: null, backups: null };

    root.innerHTML =
      '<div class="page-head">' +
      '<div><h1>设置</h1><p class="sub">AI 数据生成与内置浏览器参数，保存后立即生效</p></div>' +
      '<div class="head-actions"><button class="btn" data-act="reload">重新读取</button></div>' +
      '</div>' +
      '<div id="settings-body">' + loadingCard() + '</div>' +
      '<section class="card">' +
      '<div class="card-head"><h2>怎么用（4 步）</h2></div>' +
      '<div class="card-body">' +
      '<ol class="steps-list">' +
      '<li><b>装插件录制</b>：浏览器加载录制插件（MV3），填录制名称后开始操作业务页面，步骤与元素会自动上报到服务端。</li>' +
      '<li><b>生成数据</b>：进入录制详情，在「数据准备」里写一句指令（如「价格 99-299」），点「AI 生成测试数据」；没配大模型会用内置规则生成。</li>' +
      '<li><b>执行</b>：确认字段覆盖率后点「执行测试」，服务端用内置浏览器逐步执行（多级定位 + 自愈）。</li>' +
      '<li><b>看运行详情</b>：在运行详情里看逐步结果、实际使用定位、截图与实时日志；有自愈命中可一键写回录制。</li>' +
      '</ol>' +
      '</div></section>';

    var bodyEl = $('#settings-body', root);

    function settingsFormHtml(s) {
      var ai = s.ai || {};
      var br = s.browser || {};
      var sv = s.server || {};
      var keyBadge = ai.has_key
        ? '<span class="badge badge--ok"><i class="dot"></i>API Key 已配置</span>'
        : '<span class="badge badge--draft"><i class="dot"></i>未配置 API Key</span>';

      return '<div class="settings-grid">' +
        '<section class="card">' +
        '<div class="card-head"><h2>AI 数据生成</h2>' + keyBadge + '</div>' +
        '<div class="card-body">' +
        field('base_url',
          '<input class="input input--mono" id="s-base-url" value="' + esc(ai.base_url || '') + '" placeholder="https://api.openai.com/v1">',
          '兼容 OpenAI 协议的接口根地址。') +
        field('model',
          '<input class="input input--mono" id="s-model" value="' + esc(ai.model || '') + '" placeholder="gpt-4o-mini">') +
        field('api_key',
          '<input class="input input--mono" id="s-api-key" type="password" autocomplete="new-password" value="" placeholder="' +
          (ai.has_key ? '已配置（不显示）' : '未配置，留空表示不修改') + '">',
          '只写不读：留空则不提交该字段，服务端保持原值。') +
        field('timeout_sec',
          '<input class="input input--mono" id="s-timeout" type="number" min="1" step="1" value="' +
          esc(ai.timeout_sec === null || ai.timeout_sec === undefined ? '' : ai.timeout_sec) + '" placeholder="30">',
          '调用大模型的超时时间（秒）。') +
        (sv.port ? '<div class="field-hint">服务端端口：<span class="mono">' + esc(sv.port) + '</span></div>' : '') +
        '</div></section>' +

        '<section class="card">' +
        '<div class="card-head"><h2>内置浏览器</h2></div>' +
        '<div class="card-body">' +
        field('executable',
          '<input class="input input--mono" id="s-exec" value="' + esc(br.executable || '') + '" placeholder="留空 = 自动探测 Edge / Chrome">',
          '例如 C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe') +
        '<div class="field"><label>headless</label>' +
        '<label class="check-line"><input type="checkbox" id="s-headless"' + (br.headless ? ' checked' : '') + '>' +
        '<span>无头模式运行（不弹出窗口）</span></label></div>' +
        '<div class="form-row">' +
        '<div class="field"><label for="s-step-delay">step_delay_ms</label>' +
        '<input class="input input--mono" id="s-step-delay" type="number" min="0" step="50" value="' +
        esc(br.step_delay_ms === null || br.step_delay_ms === undefined ? '' : br.step_delay_ms) + '" placeholder="200"></div>' +
        '<div class="field"><label for="s-action-timeout">action_timeout_ms</label>' +
        '<input class="input input--mono" id="s-action-timeout" type="number" min="0" step="500" value="' +
        esc(br.action_timeout_ms === null || br.action_timeout_ms === undefined ? '' : br.action_timeout_ms) + '" placeholder="8000"></div>' +
        '</div>' +
        field('kernel_dir',
          '<input class="input input--mono" id="s-kernel-dir" value="' + esc(br.kernel_dir || '') + '" placeholder="留空 = 用脚本自动探测的系统浏览器；例：D:\\browser\\chrome-win64">',
          '自带内核目录：把 Chrome for Testing 解压到这里（或用 scripts/fetch-browser.ps1 一键下载），服务端优先用它，完全不依赖系统浏览器。') +
        field('debug_url',
          '<input class="input input--mono" id="s-debug-url" value="' + esc(br.debug_url || '') + '" placeholder="留空 = 由服务端自己拉起浏览器；例：http://127.0.0.1:9400">',
          '连接一个已在运行的浏览器（先手工执行 msedge --remote-debugging-port=9400），服务端不再自己启动浏览器。') +
        '<div class="card-sub">' +
        '<button class="btn btn--sm" data-act="diagnose">诊断浏览器</button>' +
        '<span class="field-hint">真实打开一个页面，检查渲染进程、内核路径与启动参数</span>' +
        '</div>' +
        '<pre id="diagnose-out" class="diagnose-out hidden"></pre>' +
        '<div class="field"><label>no_sandbox</label>' +
        '<label class="check-line"><input type="checkbox" id="s-nosandbox"' + (br.no_sandbox ? ' checked' : '') + '>' +
        '<span>启动时追加 --no-sandbox（渲染进程一直崩溃时再打开）</span></label></div>' +
        '</div></section>' +
        assistFormHtml(s) +
        '</div>' +
        backupCardHtml() +

        '<section class="card"><div class="card-body head-actions">' +
        '<button class="btn btn--primary" data-act="save">保存设置</button>' +
        '<button class="btn" data-act="reload">取消并重新读取</button>' +
        '</div></section>';
    }

    /** 辅助功能开关：三项都能单独关掉，关掉不影响主流程 */
    function assistFormHtml(s) {
      var a = s.assist || {};
      function row(id, title, desc, on) {
        return '<div class="field">' +
          '<label class="check-line"><input type="checkbox" id="' + id + '"' + (on ? ' checked' : '') + '>' +
          '<span><b>' + title + '</b></span></label>' +
          '<span class="field-hint">' + desc + '</span>' +
          '</div>';
      }
      return '<section class="card">' +
        '<div class="card-head"><h2>辅助功能</h2><span class="count-pill">可单独开关</span></div>' +
        '<div class="card-body">' +
        row('s-assist-datakey', '中文标题自动转字段名',
          '录制时按业务词典把中文标签转成英文字段名：记住密码 → remember_password、商品名称 → product_name。' +
          '关掉后纯中文标签会退化成 field_25 这种键。', a.auto_data_key !== false) +
        row('s-assist-testid', '缺 data-testid 的元素标红',
          '在录制详情里把"没有 data-testid / data-test / data-cy 一类稳定锚点"的步骤标红，提醒前端补齐。' +
          '补齐后定位最稳，重新打包也不怕。', a.stable_id_hint !== false) +
        row('s-assist-rebind', '执行时自动刷新过期的 uid',
          '某一步的 uid 已过期、但靠兜底策略（标题/按钮文字/表格行）命中了元素时，自动把新 uid 写回录制。' +
          '关掉后只提示、不落盘。', a.auto_rebind_uid !== false) +
        '</div></section>';
    }

    /** 数据快照：每次启动都会留一份，误删/误改可以一键恢复 */
    function backupCardHtml() {
      var items = state.backups;
      if (!items) {
        return '<section class="card"><div class="card-head"><h2>数据快照</h2></div>' +
          '<div class="card-body"><div class="field-hint">读取中…</div></div></section>';
      }
      var rows = items.map(function (b) {
        return '<tr>' +
          '<td class="mono">' + esc(b.name) + '</td>' +
          '<td>' + esc(fmtTime(b.created_at)) + '</td>' +
          '<td>' + esc(b.recordings) + ' 条录制 / ' + esc(b.runs) + ' 次运行</td>' +
          '<td class="mono">' + Math.round((b.size || 0) / 1024) + ' KB</td>' +
          '<td><button class="btn btn--sm" data-act="restore-backup" data-name="' + esc(b.name) + '">恢复</button></td>' +
          '</tr>';
      }).join('');
      return '<section class="card">' +
        '<div class="card-head"><h2>数据快照</h2>' +
        '<div class="card-tools"><button class="btn btn--sm" data-act="reload-backups">刷新列表</button></div></div>' +
        '<div class="card-body">' +
        '<div class="field-hint">服务端每次启动都会把 data/uitest.json 快照到 data/backups（保留最近 5 份）。' +
        '恢复会用快照覆盖当前数据，覆盖前会先把当前状态再存一份，所以恢复错了也能退回。</div>' +
        (items.length
          ? '<div class="table-wrap"><table class="table"><thead><tr><th>文件</th><th>时间</th><th>内容</th><th>大小</th><th>操作</th></tr></thead><tbody>' + rows + '</tbody></table></div>'
          : '<div class="field-hint" style="margin-top:10px">还没有快照（重启一次服务端就会生成）。</div>') +
        '</div></section>';
    }

    function paint() {
      if (!state.data) { bodyEl.innerHTML = loadingCard(); return; }
      bodyEl.innerHTML = settingsFormHtml(state.data);
    }

    async function load() {
      bodyEl.innerHTML = '<div class="card state-card"><span class="spinner"></span><span>加载中…</span></div>';
      try {
        state.data = await api('GET', '/api/settings');
        paint();
        loadBackups();
      } catch (e) {
        bodyEl.innerHTML = errorCard(e.message || '无法读取设置');
      }
    }

    async function loadBackups() {
      try {
        var r = await api('GET', '/api/backups', undefined, { silent: true });
        state.backups = (r && r.items) || [];
      } catch (e) {
        state.backups = [];
      }
      paint();
    }

    async function save() {
      var read = function (elId) {
        var el = $('#' + elId, root);
        return el ? String(el.value).trim() : '';
      };
      var readNum = function (elId, fallback) {
        var v = read(elId);
        if (v === '') return fallback;
        var n = Number(v);
        return isFinite(n) ? n : fallback;
      };

      var payload = {
        ai: {
          base_url: read('s-base-url'),
          model: read('s-model'),
          timeout_sec: readNum('s-timeout', 0)
        },
        browser: {
          executable: read('s-exec'),
          headless: !!($('#s-headless', root) && $('#s-headless', root).checked),
          step_delay_ms: readNum('s-step-delay', 0),
          action_timeout_ms: readNum('s-action-timeout', 0),
          debug_url: read('s-debug-url'),
          kernel_dir: read('s-kernel-dir'),
          no_sandbox: !!($('#s-nosandbox', root) && $('#s-nosandbox', root).checked)
        }
      };
      var apiKey = read('s-api-key');
      if (apiKey) payload.ai.api_key = apiKey;   // 留空 → 不提交，保持原值
      payload.assist = {
        auto_data_key: !!($('#s-assist-datakey', root) && $('#s-assist-datakey', root).checked),
        stable_id_hint: !!($('#s-assist-testid', root) && $('#s-assist-testid', root).checked),
        auto_rebind_uid: !!($('#s-assist-rebind', root) && $('#s-assist-rebind', root).checked)
      };

      try {
        var res = await api('PUT', '/api/settings', payload);
        if (res && res.ai) state.data = res;
        toast('设置已保存', 'success');
        paint();
      } catch (e) { /* api 已提示 */ }
    }

    root.addEventListener('click', function (e) {
      var t = e.target && e.target.nodeType === 1 ? e.target : null;
      if (!t || !t.closest) return;
      var btn = t.closest('[data-act]');
      if (!btn) return;
      var act = btn.getAttribute('data-act');
      if (act === 'reload') { load(); return; }
      if (act === 'save') { save(); return; }
      if (act === 'diagnose') { diagnose(); return; }
      if (act === 'reload-backups') { loadBackups(); return; }
      if (act === 'restore-backup') {
        var name = btn.getAttribute('data-name') || '';
        confirmDialog({
          title: '恢复数据快照',
          message: '用「' + name + '」覆盖当前数据？\n当前状态会先自动再存一份快照，可以退回。',
          confirmText: '恢复'
        }).then(async function (ok) {
          if (!ok) return;
          try {
            var r = await api('POST', '/api/backups/' + encodeURIComponent(name) + '/restore', {});
            toast((r && r.message) || '已恢复', 'success');
            await load();
            await refreshHealth();
          } catch (e) { /* api 已提示 */ }
        });
        return;
      }
    });

    /** 诊断：真实开一个页面，把内核/参数/探测结果摊出来 */
    async function diagnose() {
      var out = $('#diagnose-out', root);
      if (!out) return;
      out.classList.remove('hidden');
      out.textContent = '诊断中…（会真实打开一个页面）';
      try {
        var r = await api('GET', '/api/browser/diagnose?url=' + encodeURIComponent('http://127.0.0.1:8490/demo/'));
        var p = r.probe || {};
        var lines = [
          '内核路径   : ' + (r.executable || '（未探测到）'),
          '内核版本   : ' + (r.version || '-') + (r.attached ? '  [外部浏览器]' : ''),
          '自带内核   : ' + (r.kernel_dir || '（默认 <程序目录>/browser）'),
          '启动参数   : ' + ((r.launch_flags && r.launch_flags.length) ? r.launch_flags.join(' ') : '（默认参数）'),
          '调试地址   : ' + (r.debug_url || '-'),
          '渲染探测   : ' + (p.ok ? '正常' : '失败') + (p.duration_ms ? '（' + p.duration_ms + 'ms）' : ''),
          '页面标题   : ' + (p.title || '-'),
          'readyState : ' + (p.ready_state || '-'),
          p.error ? '错误       : ' + p.error : '',
          '日志文件   : ' + (r.log_file || '-'),
          '',
          '诊断结论   : ' + (r.hint || '-'),
          '',
          r.log_tail ? '—— 浏览器日志尾部 ——\n' + r.log_tail : ''
        ].filter(function (x) { return x !== ''; });
        out.textContent = lines.join('\n');
      } catch (e) {
        out.textContent = '诊断失败：' + (e.message || e);
      }
    }

    load();
  }

  /* ======================================================================
   * 启动
   * ====================================================================== */

  function boot() {
    bindHealthControls();
    refreshHealth();
    Health.timer = setInterval(refreshHealth, 10000);

    window.addEventListener('hashchange', router);

    // 空 hash / 无 hash 时默认进录制列表
    if (!location.hash || location.hash === '#') {
      location.hash = '#/recordings';
    } else {
      router();
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
