/*
 * UI 自动化录制器 —— content script（录制内容脚本）
 *
 * 设计要点：
 *   1) 只读状态、只发消息：所有网络请求与状态写入都在 background.js 里完成。
 *   2) 与执行器共用 runtime/uitest-keys.js（挂载在 window.__uitestKeys，同一隔离世界已先加载），
 *      因此录制时算出的 data-uitest-id 与服务端回放时算出的完全一致。
 *   3) 监听用捕获阶段且只绑定一次，SPA 路由变化后依然有效。
 *   4) 视觉反馈全部放在 Shadow DOM 浮层里，不影响页面布局，也不被自己录进去。
 */
(function () {
  'use strict';

  // 幂等守卫：scripting.executeScript 补注入会与 manifest 注入重叠，必须只生效一次
  if (window.__uitestRecorderLoaded) return;
  window.__uitestRecorderLoaded = true;

  var KEYS = window.__uitestKeys || null;        // 共享运行时；缺失时只做兜底提示
  var OVERLAY_HOST_ID = 'uitest-overlay-host';   // 浮层 host id（用于排除自身事件）
  var ENTER_STORAGE_KEY = 'uitest_record_enter'; // popup 的"回车键也记录"开关
  var INPUT_DEBOUNCE_MS = 500;                   // 输入防抖，避免粘贴/中文输入产生中间态
  var STATE_POLL_MS = 3000;                      // 状态兜底轮询
  var TEXT_FALLBACK_MAX = 40;                    // text 兜底定位的文本长度上限
  var DATA_KEY_MAX = 40;                         // data_key 长度上限
  // 只在顶层框架显示浮层（all_frames 下每个 iframe 都会注入一份脚本，浮层不能重复）
  var IS_TOP = (function () {
    try { return window.top === window.self; } catch (e) { return false; }
  })();

  /* ---------------- 内部状态 ---------------- */

  var state = {
    serverUrl: 'http://127.0.0.1:8490',
    active: false,
    recordingId: '',
    recordingName: '',
    startUrl: '',
    tabId: null,
    stepCount: 0,
    lastStepId: '',
    lastStepAnnotation: '',
    recent: []
  };
  var selfTabId = null;         // 本框架所在标签页 id（由 background 告知）
  var recordingHere = false;    // 当前页是否就是录制标签页
  var localStepCount = 0;       // 本地步数（浮层显示用）
  var overlay = null;           // 浮层对象 {host, shadow, label, err, stop}
  var inputTimers = new WeakMap(); // 元素 -> 防抖定时器
  var boundKeys = new Set();    // 已上报元素登记的 key，本地去重
  var listenersBound = false;

  /* ---------------- 通用小工具 ---------------- */

  /** 把任意状态补齐成完整结构（只用于本地判断，不写回） */
  function normalizeState(raw) {
    var s = raw && typeof raw === 'object' ? raw : {};
    return {
      serverUrl: String(s.serverUrl || 'http://127.0.0.1:8490'),
      active: s.active === true,
      recordingId: String(s.recordingId || ''),
      recordingName: String(s.recordingName || ''),
      startUrl: String(s.startUrl || ''),
      tabId: typeof s.tabId === 'number' ? s.tabId : null,
      stepCount: Number(s.stepCount) || 0,
      lastStepId: String(s.lastStepId || ''),
      lastStepAnnotation: String(s.lastStepAnnotation || ''),
      recent: Array.isArray(s.recent) ? s.recent : []
    };
  }

  /** 本地时间 ISO 字符串（带时区偏移） */
  function nowIso() {
    var d = new Date();
    function pad(n) { return String(Math.floor(Math.abs(n))).padStart(2, '0'); }
    var off = -d.getTimezoneOffset();
    var sign = off >= 0 ? '+' : '-';
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      'T' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) +
      sign + pad(off / 60) + ':' + pad(Math.abs(off) % 60);
  }

  /** 向 background 发消息（统一兜住 lastError 与扩展重载后的异常） */
  function sendMessage(msg, cb) {
    try {
      chrome.runtime.sendMessage(msg, function (resp) {
        var err = chrome.runtime.lastError ? chrome.runtime.lastError.message : '';
        if (cb) cb(resp, err);
      });
    } catch (e) {
      if (cb) cb(null, (e && e.message) || String(e));
    }
  }

  /* ---------------- 状态同步 ---------------- */

  /** 主动向 background 要一次状态（顺带拿到自己的 tabId） */
  function askState() {
    sendMessage({ type: 'state' }, function (resp) {
      if (!resp || !resp.ok) return;
      if (typeof resp.selfTabId === 'number') selfTabId = resp.selfTabId;
      applyState(resp.state);
    });
  }

  /** 应用状态：只有"正在录制 + 是本标签页"才显示浮层并录事件 */
  function applyState(next) {
    if (!next) return;
    state = normalizeState(next);
    localStepCount = state.stepCount;

    var wasHere = recordingHere;
    recordingHere = !!(state.active && selfTabId !== null && state.tabId === selfTabId);

    if (recordingHere) {
      ensureOverlay();
      renderOverlay();
    } else if (wasHere || overlay) {
      removeOverlay();
    }
  }

  /* ---------------- 浮层（Shadow DOM，右下角） ---------------- */

  var OVERLAY_CSS = [
    ':host{position:fixed;top:0;left:0;width:0;height:0;pointer-events:none;z-index:2147483647}',
    '.wrap{position:fixed;right:16px;bottom:16px;display:flex;align-items:center;gap:8px;max-width:320px;',
    'padding:8px 10px;background:#111827;color:#f8fafc;border-radius:8px;box-shadow:0 10px 30px rgba(15,23,42,.35);',
    'pointer-events:none;font:600 13px/1.4 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}',
    '.label{white-space:nowrap;pointer-events:none}',
    '.err{pointer-events:none;color:#fca5a5;font-size:11px;font-weight:400;max-width:180px;display:none}',
    '.stop{pointer-events:auto;cursor:pointer;border:0;border-radius:6px;background:#ef4444;color:#fff;',
    'font:600 12px/1 -apple-system,"Segoe UI",sans-serif;padding:6px 10px}',
    '.stop:hover{background:#dc2626}'
  ].join('');

  /** 创建浮层（幂等）；返回 {host, label, err, stop}；iframe 内不创建 */
  function ensureOverlay() {
    if (!IS_TOP) return null;
    if (overlay) return overlay;
    var existing = document.getElementById(OVERLAY_HOST_ID);
    if (existing && existing.__uitestOverlay) {
      overlay = existing.__uitestOverlay;
      return overlay;
    }
    if (!document.documentElement) return null; // 极端早期（还没有 <html>）

    var host = document.createElement('div');
    host.id = OVERLAY_HOST_ID;
    host.setAttribute('data-uitest-ignore', '1');   // 自己产生的元素一律不录

    var shadow = host.attachShadow({ mode: 'open' });

    var styleEl = document.createElement('style');
    styleEl.textContent = OVERLAY_CSS;

    var wrap = document.createElement('div');
    wrap.className = 'wrap';

    var label = document.createElement('span');
    label.className = 'label';

    var err = document.createElement('span');
    err.className = 'err';

    var stopBtn = document.createElement('button');
    stopBtn.className = 'stop';
    stopBtn.type = 'button';
    stopBtn.textContent = '停止';

    wrap.appendChild(label);
    wrap.appendChild(err);
    wrap.appendChild(stopBtn);
    shadow.appendChild(styleEl);
    shadow.appendChild(wrap);

    document.documentElement.appendChild(host);

    overlay = { host: host, shadow: shadow, label: label, err: err, stop: stopBtn };
    host.__uitestOverlay = overlay;

    // 停止按钮：只响应自己，不产生 click 步骤（stopPropagation + 浮层路径过滤）
    stopBtn.addEventListener('click', function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      stopRecording();
    }, true);

    return overlay;
  }

  /** 更新浮层文案（步数实时变化） */
  function renderOverlay() {
    var ov = ensureOverlay();
    if (!ov) return;
    if (!KEYS) {
      ov.label.textContent = '录制运行库 uitest-keys.js 未加载，请重新加载扩展';
      return;
    }
    ov.label.textContent = '● 录制中 · ' + localStepCount + ' 步';
    ov.host.title = state.recordingName ? ('录制：' + state.recordingName) : '';
  }

  /** 移除浮层 */
  function removeOverlay() {
    if (overlay && overlay.host) {
      if (overlay.host.parentNode) overlay.host.parentNode.removeChild(overlay.host);
      delete overlay.host.__uitestOverlay;
    }
    overlay = null;
  }

  /** 浮层里短暂显示错误（不弹 alert，不打断页面） */
  function showOverlayError(text) {
    var ov = ensureOverlay();
    if (!ov) return;
    ov.err.textContent = String(text || '出错了');
    ov.err.style.display = 'block';
    setTimeout(function () {
      if (overlay && overlay.err) {
        overlay.err.style.display = 'none';
        overlay.err.textContent = '';
      }
    }, 4000);
  }

  /** 点击浮层"停止"：交给 background 收尾 */
  function stopRecording() {
    sendMessage({ type: 'stop' }, function (resp, err) {
      if (err) { showOverlayError('与后台通信失败：' + err); return; }
      if (resp && resp.ok === false && resp.error) showOverlayError(resp.error);
      askState();
    });
  }

  /* ---------------- 命中高亮 ---------------- */

  /** 录制到元素时闪一下 2px 靛蓝描边（绝对定位 div，300ms 后移除） */
  function flash(el) {
    try {
      var rect = el.getBoundingClientRect();
      if (!rect || (rect.width <= 0 && rect.height <= 0)) return;
      var box = document.createElement('div');
      box.setAttribute('data-uitest-ignore', '1');
      box.style.cssText =
        'position:absolute;z-index:2147483646;pointer-events:none;box-sizing:border-box;' +
        'border:2px solid #4f46e5;border-radius:3px;' +
        'left:' + (rect.left + window.scrollX) + 'px;' +
        'top:' + (rect.top + window.scrollY) + 'px;' +
        'width:' + Math.max(rect.width, 1) + 'px;' +
        'height:' + Math.max(rect.height, 1) + 'px;';
      var parent = document.body || document.documentElement;
      if (!parent) return;
      parent.appendChild(box);
      setTimeout(function () {
        if (box.parentNode) box.parentNode.removeChild(box);
      }, 300);
    } catch (e) {
      /* 高亮失败不影响录制 */
    }
  }

  /* ---------------- 元素语义辅助 ---------------- */

  var TAG_CN = {
    input: '输入框', textarea: '文本域', select: '下拉框', button: '按钮', a: '链接',
    label: '标签', summary: '折叠标题', li: '列表项', td: '单元格', th: '表头单元格',
    div: '区块', span: '文本', p: '段落', form: '表单', img: '图片'
  };

  var TEXT_INPUT_TYPES = {
    text: 1, search: 1, tel: 1, email: 1, url: 1, password: 1,
    number: 1, date: 1, 'datetime-local': 1, month: 1, week: 1, time: 1
  };

  /** 读取元素 type（统一小写） */
  function inputType(el) {
    return String((el.getAttribute && el.getAttribute('type')) || '').toLowerCase();
  }

  /** tag 的中文名（input 进一步细分） */
  function tagCn(target) {
    var tag = (target && target.tag) || '';
    var cn = TAG_CN[tag] || tag || '元素';
    if (tag === 'input') {
      var t = String((target && target.type) || '').toLowerCase();
      if (t === 'checkbox') cn = '复选框';
      else if (t === 'radio') cn = '单选框';
      else if (t === 'file') cn = '文件上传框';
      else if (t === 'password') cn = '密码框';
      else if (t === 'submit' || t === 'button' || t === 'reset') cn = '按钮';
    }
    return cn;
  }

  /** 判断是否"按钮类"（annotation 与 text 兜底定位都用它） */
  function isButtonLike(el, target) {
    var tag = (target && target.tag) || (el.tagName || '').toLowerCase();
    if (tag === 'button' || tag === 'a' || tag === 'summary') return true;
    if (tag === 'input') {
      var t = String((target && target.type) || '').toLowerCase();
      if (t === 'submit' || t === 'button' || t === 'reset' || t === 'checkbox' || t === 'radio') return true;
    }
    var role = (target && target.role) || '';
    if (role === 'button' || role === 'link' || role === 'menuitem' || role === 'tab') return true;
    if (el.getAttribute && el.getAttribute('onclick')) return true;
    var cls = el.className && typeof el.className === 'string' ? el.className : '';
    if (/(^|[\s_-])(btn|button|link)([\s_-]|$)/i.test(cls)) return true;
    return false;
  }

  /** 是否是"输入型"控件（input 文本类 / textarea / contenteditable） */
  function isTextInput(el) {
    if (!el || el.nodeType !== 1) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'textarea') return true;
    if (el.isContentEditable) return true;
    if (tag !== 'input') return false;
    var t = inputType(el) || 'text';
    return TEXT_INPUT_TYPES[t] === 1;
  }

  /** 读取控件当前值 */
  function elemValue(el) {
    if (el.isContentEditable) return String(el.innerText || el.textContent || '');
    if (el.value === undefined || el.value === null) return '';
    return String(el.value);
  }

  /** label/name/placeholder -> snake_case（ASCII 化） */
  function toSnake(value) {
    var s = String(value || '')
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '_')
      .replace(/_+/g, '_')
      .replace(/^_+|_+$/g, '');
    if (s.length > DATA_KEY_MAX) s = s.slice(0, DATA_KEY_MAX).replace(/_+$/, '');
    return s;
  }

  /** 推导业务字段名：优先 label，其次 name，再次 placeholder；都不可用则 field_序号 */
  function deriveDataKey(el, target, seq) {
    var cands = [
      target.label,
      (el.getAttribute && el.getAttribute('name')) || '',
      target.placeholder
    ];
    for (var i = 0; i < cands.length; i++) {
      var v = toSnake(cands[i]);
      if (v && !/^[0-9]+$/.test(v)) return v;
    }
    return 'field_' + (seq || 1);
  }

  /* ---------------- 步骤构造 ---------------- */

  /**
   * 组装 locator：uid 首选，兜底按「重建后仍然可靠」的顺序排。
   *
   * Vue / React 打包后 hash 类名、data-v-*、自动 id 都会变，所以这里的顺序是：
   *   uid → attr(data-testid 一类) → id/name（仅稳定时）→ label（表单项标题）
   *   → near（标题→同表单项控件）→ role（角色+按钮文字）→ cell（行文字+按钮）
   *   → placeholder → css（纯结构，不带 class）→ text
   * 这些兜底让"页面改版但业务语义没变"的情况下依然能定位。
   */
  function buildLocator(el, target, key) {
    var locator = {
      strategy: 'uid',
      value: key,
      fallbacks: [],
      frame_url: IS_TOP ? '' : String(location.href || '')
    };
    var fb = locator.fallbacks;

    function add(strategy, value) {
      var v = value === null || value === undefined ? '' : String(value);
      if (!v) return;
      for (var i = 0; i < fb.length; i++) {
        if (fb[i].strategy === strategy && fb[i].value === v) return;
      }
      fb.push({ strategy: strategy, value: v });
    }

    var tag = (el.tagName || '').toLowerCase();
    var buttonLike = isButtonLike(el, target);

    if (target.test_attr) add('attr', target.test_attr);      // 团队自己写的稳定属性
    if (target.stable_id) add('id', target.stable_id);        // 只放"看起来稳定"的 id
    if (target.name) add('name', target.name);

    if (buttonLike) {
      // 按钮/链接：靠「行文字+按钮文字」或「角色+按钮文字」定位。
      // 绝不能走 label/near——按钮文字会被当成表单项标题，从而抓到旁边的输入框。
      if (target.column && target.row_text && target.text) {
        add('cell', target.row_text + '>>' + target.text);    // 表格行内操作：最稳
      }
      if (target.text && target.text.length <= TEXT_FALLBACK_MAX) {
        add('role', (target.role || 'button') + '|' + target.text);
        add('text', target.text);
      }
    } else {
      // 表单控件：标题 → 同一表单项里的控件
      if (target.label) {
        add('label', target.label);
        var tagSel = '';
        if (target.tag === 'textarea') tagSel = 'textarea';
        else if (target.tag === 'select') tagSel = 'select';
        else if (target.tag === 'input') tagSel = 'input:not([type=hidden])';
        add('near', target.label + '>>' + tagSel);
      }
      if (target.placeholder) add('placeholder', target.placeholder);
    }
    var css = KEYS.uniqueSelector(el);
    if (css) add('css', css);                                 // 纯结构路径，最后兜底
    return locator;
  }

  /** 自动生成中文备注 */
  function buildAnnotation(action, el, target, value) {
    var label = target.label || '';
    var text = target.text || '';
    var ph = target.placeholder || '';
    var name = (el.getAttribute && el.getAttribute('name')) || '';
    var tag = tagCn(target);
    var fieldLabel = label || ph || name || tag;

    switch (action) {
      case 'click':
        if (isButtonLike(el, target)) return '点击【' + (text || label || tag) + '】';
        return '点击【' + (label || text || tag) + '】';
      case 'input':
        return '在【' + fieldLabel + '】输入内容';
      case 'select':
        return '在【' + fieldLabel + '】选择「' + (value || '') + '」';
      case 'check':
        return '勾选【' + (label || text || tag) + '】';
      case 'uncheck':
        return '取消勾选【' + (label || text || tag) + '】';
      case 'upload':
        return '上传文件到【' + fieldLabel + '】';
      case 'key':
        return '在【' + fieldLabel + '】按 ' + (value || 'Enter') + ' 键';
      case 'navigate':
        return '打开页面 ' + (value || '');
      default:
        return action + '【' + fieldLabel + '】';
    }
  }

  /**
   * 构造一个完整 Step（id / recording_id / index 由服务端生成，这里不填）。
   * @param {string} action 动作
   * @param {Element} el 目标元素
   * @param {object} [opts] {value, note, annotation, dataKey}
   */
  function buildStep(action, el, opts) {
    opts = opts || {};
    // keyOf 会先 markAll 再读属性值：这样同签名元素的 _2/_3 去重后缀也能拿到，
    // 保证"录制时算出的 key"与"执行时 markAll 算出的 key"完全一致。
    var key = KEYS.keyOf ? KEYS.keyOf(el) : KEYS.computeKey(el);
    try {
      el.setAttribute(KEYS.KEY_ATTR, key); // 落盘稳定 id，页面上可见、执行器可直接取
    } catch (e) {
      /* 属性写入失败不阻断录制 */
    }
    var target = KEYS.describe(el);
    var locator = buildLocator(el, target, key);
    var value = opts.value === undefined || opts.value === null ? '' : String(opts.value);

    var step = {
      action: action,
      annotation: opts.annotation || buildAnnotation(action, el, target, value),
      locator: locator,
      target: target,
      data_key: action === 'input' ? deriveDataKey(el, target, localStepCount + 1) : String(opts.dataKey || ''),
      value: value,
      expect: '',
      wait_ms: 0,
      optional: false,
      note: String(opts.note || ''),
      created_at: nowIso()
    };

    registerElement(el, key, target);
    return step;
  }

  /** 元素登记：同一个 key 只上报一次 */
  function registerElement(el, key, target) {
    if (!key || boundKeys.has(key)) return;
    boundKeys.add(key);
    var label = target.label || target.text || '';
    var descriptor = (label ? label + ' ' : '') + tagCn(target);
    sendMessage({
      type: 'element',
      element: {
        page: target.page,
        key: key,
        selector: KEYS.uniqueSelector(el),
        tag: target.tag,
        type: target.type,
        label: target.label || '',
        descriptor: descriptor
      }
    }, function () { /* 元素登记失败不影响录制 */ });
  }

  /** 发送步骤给 background，成功后刷新浮层步数 */
  function sendStep(step, el) {
    if (el) flash(el); // 先给视觉反馈，网络往返不影响手感
    sendMessage({ type: 'step', step: step }, function (resp, err) {
      if (err) { showOverlayError('与后台通信失败：' + err); return; }
      if (!resp) { showOverlayError('后台无响应'); return; }
      if (!resp.ok) { showOverlayError(resp.error || '步骤保存失败'); return; }
      localStepCount = Number(resp.stepCount) || localStepCount;
      state.stepCount = localStepCount;
      renderOverlay();
    });
  }

  /* ---------------- 事件过滤 ---------------- */

  /** 事件路径上是否包含我们自己的浮层 */
  function pathHasOverlay(ev) {
    if (!overlay || !overlay.host) return false;
    if (ev.composedPath) {
      var path = ev.composedPath();
      if (path && path.indexOf(overlay.host) >= 0) return true;
    }
    var t = ev.target;
    while (t && t.nodeType === 1) {
      if (t === overlay.host || (t.id && t.id === OVERLAY_HOST_ID)) return true;
      t = t.parentNode;
    }
    return false;
  }

  /** 带 data-uitest-ignore 标记（我们自己画的元素）一律不录 */
  function isIgnored(el) {
    if (!el || el.nodeType !== 1) return true;
    if (el.hasAttribute && el.hasAttribute('data-uitest-ignore')) return true;
    if (el.closest && el.closest('[data-uitest-ignore]')) return true;
    return false;
  }

  /** 录制开关统一判断 */
  function canRecord() {
    return recordingHere && !!KEYS;
  }

  /* ---------------- 事件处理 ---------------- */

  /** click：跳过 html/body/浮层/忽略元素，找真正的可交互目标 */
  function onClick(ev) {
    if (!canRecord()) return;
    if (pathHasOverlay(ev)) return;

    var raw = ev.target;
    if (raw && raw.nodeType === 3) raw = raw.parentElement; // 文本节点
    if (!raw || raw.nodeType !== 1) return;
    if (isIgnored(raw)) return;

    var el = KEYS.clickTargetOf(raw);
    if (!el || el.nodeType !== 1) return;

    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'html' || tag === 'body') return;   // 点空白处不记录
    if (isIgnored(el)) return;

    // 下拉框、勾选框、文件框由 change 事件负责，这里不再产生重复的 click 步骤
    if (tag === 'select') return;
    if (tag === 'textarea') return;                 // 文本域点击只是聚焦，由 input 事件记录
    if (tag === 'input') {
      var t = inputType(el);
      if (t === 'checkbox' || t === 'radio' || t === 'file') return;
      // 文本/数字/日期等输入框：点击只用于聚焦，真正的输入由 input 事件记录，
      // 否则每个字段都会多出一条无意义的「点击」步骤
      if (t !== 'submit' && t !== 'button' && t !== 'reset' && t !== 'image') return;
    }

    sendStep(buildStep('click', el), el);
  }

  /** input：文本类控件按元素 500ms 防抖，只记最终结果 */
  function onInput(ev) {
    if (!canRecord()) return;
    if (pathHasOverlay(ev)) return;

    var el = ev.target;
    if (!el || el.nodeType !== 1) return;
    if (isIgnored(el)) return;
    if (!isTextInput(el)) return;

    scheduleInput(el);
  }

  /** 按元素排布防抖定时器 */
  function scheduleInput(el) {
    var prev = inputTimers.get(el);
    if (prev) clearTimeout(prev);
    var timer = setTimeout(function () {
      inputTimers.delete(el);
      if (!recordingHere) return;
      if (el.isConnected === false) return; // 元素已被移除（弹窗关闭等）
      emitInputStep(el);
    }, INPUT_DEBOUNCE_MS);
    inputTimers.set(el, timer);
  }

  /** 真正产出一条 input 步骤（密码框脱敏） */
  function emitInputStep(el) {
    var type = inputType(el);
    var value = elemValue(el);
    var note = '';
    if (type === 'password') {
      value = '{{password}}';
      note = '敏感字段：录制时已脱敏，执行时由数据准备注入';
    }
    sendStep(buildStep('input', el, { value: value, note: note }), el);
  }

  /** change：select / checkbox / radio / file；文本输入由 input 事件处理，避免重复 */
  function onChange(ev) {
    if (!canRecord()) return;
    if (pathHasOverlay(ev)) return;

    var el = ev.target;
    if (!el || el.nodeType !== 1) return;
    if (isIgnored(el)) return;

    var tag = (el.tagName || '').toLowerCase();
    var type = inputType(el);

    if (tag === 'select') {
      var opt = el.options && el.selectedIndex >= 0 ? el.options[el.selectedIndex] : null;
      var text = opt ? (KEYS.norm(opt.textContent) || opt.value || '') : '';
      if (!text) text = String(el.value || '');
      sendStep(buildStep('select', el, { value: text }), el);
      return;
    }

    if (tag === 'input' && type === 'file') {
      var files = el.files || [];
      var fileName = files.length ? files[0].name : '';
      if (!fileName) return;
      sendStep(buildStep('upload', el, {
        value: fileName,
        note: '文件上传需在服务端执行时提供本地文件路径'
      }), el);
      return;
    }

    if (tag === 'input' && (type === 'checkbox' || type === 'radio')) {
      sendStep(buildStep(el.checked ? 'check' : 'uncheck', el), el);
      return;
    }
    // 其余（文本框 change）：交给 input 防抖逻辑，这里不重复记录
  }

  /** keydown：只有 popup 打开"回车键也记录"且目标是 input 时才记录 Enter */
  function onKeydown(ev) {
    if (!canRecord()) return;
    if (ev.key !== 'Enter') return;
    if (pathHasOverlay(ev)) return;

    var el = ev.target;
    if (!el || el.nodeType !== 1) return;
    if (isIgnored(el)) return;
    if ((el.tagName || '').toLowerCase() !== 'input') return;
    if (!isTextInput(el)) return;

    // 开关状态每次读取，保证 popup 改动立即生效
    try {
      chrome.storage.local.get(ENTER_STORAGE_KEY, function (data) {
        if (chrome.runtime.lastError) return;
        if (!data || data[ENTER_STORAGE_KEY] !== true) return;
        if (!recordingHere) return;
        sendStep(buildStep('key', el, { value: 'Enter' }), el);
      });
    } catch (e) {
      /* 扩展重载等异常忽略 */
    }
  }

  /* ---------------- 初始化 ---------------- */

  /** 捕获阶段绑定，只绑一次（SPA 也能用） */
  function bindListeners() {
    if (listenersBound) return;
    listenersBound = true;
    document.addEventListener('click', onClick, true);
    document.addEventListener('input', onInput, true);
    document.addEventListener('change', onChange, true);
    document.addEventListener('keydown', onKeydown, true);
    // 明确不监听：paste（只认防抖结果）、scroll、mousemove
  }

  /** 接收 background 广播的状态 */
  chrome.runtime.onMessage.addListener(function (msg) {
    if (!msg || msg.type !== 'uitest:state') return;
    applyState(msg.state);
  });

  bindListeners();
  // 报到：background 据此判断"录制脚本是否就位"，popup 会显示连接状态
  sendMessage({ type: 'ready', url: location.href, top: IS_TOP });
  askState();
  setInterval(askState, STATE_POLL_MS); // 兜底轮询，防止广播丢失

  if (!KEYS) {
    // 共享运行时缺失：只做兜底提示，不抛错、不影响页面
    console.warn('[uitest] 未找到 window.__uitestKeys（uitest-keys.js 未加载），录制功能不可用。请确认扩展文件完整后重新加载扩展并刷新页面。');
  }
})();
