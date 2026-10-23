/*
 * UI 自动化录制器 —— popup 逻辑
 *
 * 原则：popup 不直接读写"会话状态"，只通过消息与 background 交互；
 *      本地只自行管理"回车键也记录"这一个开关（storage: uitest_record_enter）。
 */
'use strict';

(function () {
  var ENTER_STORAGE_KEY = 'uitest_record_enter'; // 与 content.js 约定一致
  var POLL_MS = 1000;                            // popup 打开期间的状态刷新间隔
  var HEALTH_MS = 10000;                         // 服务端可用性复检间隔
  var DEFAULT_SERVER_URL = 'http://127.0.0.1:8490';

  /* ---------------- DOM 引用 ---------------- */

  var el = {
    healthDot: document.getElementById('healthDot'),
    healthText: document.getElementById('healthText'),
    serverUrl: document.getElementById('serverUrl'),
    testBtn: document.getElementById('testBtn'),
    recName: document.getElementById('recName'),
    startBtn: document.getElementById('startBtn'),
    stopBtn: document.getElementById('stopBtn'),
    recMeta: document.getElementById('recMeta'),
    recorderRow: document.getElementById('recorderRow'),
    recorderText: document.getElementById('recorderText'),
    injectBtn: document.getElementById('injectBtn'),
    note: document.getElementById('note'),
    noteBtn: document.getElementById('noteBtn'),
    enterSwitch: document.getElementById('enterSwitch'),
    recent: document.getElementById('recent'),
    error: document.getElementById('error'),
    consoleLink: document.getElementById('consoleLink')
  };

  var current = null;          // 最近一次拿到的会话状态
  var serverUrlDirty = false;  // 用户手动改过地址输入框，不再被状态覆盖
  var lastAnnotation = '';     // 上次渲染到备注框的内容，避免覆盖用户正在输入的文字

  /* ---------------- 基础工具 ---------------- */

  /** 发消息给 background，统一转成 Promise */
  function send(msg) {
    return new Promise(function (resolve) {
      try {
        chrome.runtime.sendMessage(msg, function (resp) {
          var err = chrome.runtime.lastError ? chrome.runtime.lastError.message : '';
          if (err) { resolve({ ok: false, error: err }); return; }
          resolve(resp || { ok: false, error: '后台无响应' });
        });
      } catch (e) {
        resolve({ ok: false, error: (e && e.message) || String(e) });
      }
    });
  }

  /** 显示/隐藏红字错误 */
  function showError(text) {
    if (text) {
      el.error.textContent = String(text);
      el.error.classList.remove('is-hidden');
    } else {
      el.error.textContent = '';
      el.error.classList.add('is-hidden');
    }
  }

  /** 更新连接状态圆点与说明文字 */
  function setHealth(ok, text) {
    el.healthDot.classList.remove('dot-ok', 'dot-bad', 'dot-unknown');
    el.healthDot.classList.add(ok ? 'dot-ok' : 'dot-bad');
    el.healthText.textContent = text || (ok ? '服务端已连接' : '服务端不可用');
  }

  /** 服务端地址（去掉尾部斜杠） */
  function serverBase() {
    var url = (current && current.serverUrl) || el.serverUrl.value || DEFAULT_SERVER_URL;
    return String(url).replace(/\/+$/, '');
  }

  /** 在新标签页打开控制台页面 */
  function openConsole(hash) {
    chrome.tabs.create({ url: serverBase() + hash });
  }

  /* ---------------- 渲染 ---------------- */

  /** 根据状态渲染整个弹窗 */
  function render(state) {
    if (!state) return;
    current = state;

    // 服务端地址
    if (!serverUrlDirty && document.activeElement !== el.serverUrl) {
      el.serverUrl.value = state.serverUrl || DEFAULT_SERVER_URL;
    }

    // 录制按钮 / 实时步数
    var active = state.active === true;
    el.startBtn.classList.toggle('is-hidden', active);
    el.stopBtn.classList.toggle('is-hidden', !active);
    el.recMeta.classList.toggle('is-hidden', !active);
    el.recorderRow.classList.toggle('is-hidden', !active);
    el.recName.disabled = active;
    if (active) {
      el.recMeta.textContent = (state.recordingName || '未命名录制') + ' · ' + (state.stepCount || 0) + ' 步';
    }

    // 上一步备注：状态里有值且用户没在编辑时同步显示
    var annotation = String(state.lastStepAnnotation || '');
    if (annotation !== lastAnnotation && document.activeElement !== el.note) {
      el.note.value = annotation;
      lastAnnotation = annotation;
    }

    renderRecent(state);
  }

  /** 渲染"录制脚本是否就位"
   *  未就位 = 页面在装/更新插件之前就已打开，或页面不允许注入（chrome:// 等系统页）。
   *  这种情况过去只会录到 background 自动补的 navigate 步骤（locator=css=html），
   *  看起来就像"定位全是 css=html"。
   */
  function renderRecorder(info) {
    if (!info) return;
    var ok = info.readyInRecordingTab === true;
    el.recorderText.textContent = ok
      ? '录制脚本：已就位，操作会被记录'
      : '录制脚本：未就位 → 刷新页面，或点右侧「注入录制脚本」';
    el.recorderText.style.color = ok ? '' : '#b45309';
    el.injectBtn.classList.toggle('is-hidden', ok);
  }

  /** 渲染最近录制列表 */
  function renderRecent(state) {
    var items = Array.isArray(state.recent) ? state.recent : [];
    el.recent.textContent = '';

    if (!items.length) {
      var li0 = document.createElement('li');
      li0.className = 'empty';
      li0.textContent = '暂无录制记录';
      el.recent.appendChild(li0);
      return;
    }

    items.forEach(function (item) {
      if (!item || !item.id) return;
      var li = document.createElement('li');
      li.className = 'recent-item';
      li.title = '在控制台打开：' + (item.name || item.id);

      var nameSpan = document.createElement('span');
      nameSpan.className = 'recent-name';
      nameSpan.textContent = item.name || item.id;

      var countSpan = document.createElement('span');
      countSpan.className = 'recent-count';
      countSpan.textContent = (Number(item.step_count) || 0) + ' 步';

      li.appendChild(nameSpan);
      li.appendChild(countSpan);
      li.addEventListener('click', function () {
        openConsole('/#/recordings/' + encodeURIComponent(item.id));
      });
      el.recent.appendChild(li);
    });
  }

  /* ---------------- 状态与健康检查 ---------------- */

  /** 拉一次状态 */
  async function refreshState() {
    var resp = await send({ type: 'state' });
    if (resp && resp.state) render(resp.state);
    if (resp && resp.recorder) renderRecorder(resp.recorder);
  }

  /** ping：拿状态 + 探测 /api/health */
  async function checkHealth() {
    var resp = await send({ type: 'ping' });
    if (!resp) { setHealth(false, '后台无响应'); return; }
    if (resp.state) render(resp.state);

    if (resp.serverOk) {
      var version = resp.health && resp.health.version ? (' · v' + resp.health.version) : '';
      var counts = resp.health && typeof resp.health.recordings === 'number'
        ? (' · 录制 ' + resp.health.recordings + ' 条') : '';
      setHealth(true, '服务端已连接' + version + counts);
      showError('');
    } else {
      setHealth(false, '服务端不可用：' + (resp.error || '无法连接') + '，请检查 8490 端口与服务端地址');
    }
  }

  /* ---------------- 交互事件 ---------------- */

  /** 地址输入框：标记为已手动修改 */
  el.serverUrl.addEventListener('input', function () {
    serverUrlDirty = true;
  });

  /** 测试连接：先保存地址，再 ping */
  el.testBtn.addEventListener('click', async function () {
    showError('');
    el.testBtn.disabled = true;
    try {
      var saved = await send({ type: 'setServer', url: el.serverUrl.value });
      if (!saved || !saved.ok) {
        showError((saved && saved.error) || '地址保存失败');
        return;
      }
      if (saved.state) {
        current = saved.state;
        el.serverUrl.value = saved.state.serverUrl;
        serverUrlDirty = false;
      }
      await checkHealth();
    } finally {
      el.testBtn.disabled = false;
    }
  });

  /** 开始录制 */
  el.startBtn.addEventListener('click', async function () {
    showError('');
    el.startBtn.disabled = true;
    try {
      var resp = await send({ type: 'start', name: el.recName.value.trim(), startUrl: '' });
      if (!resp || !resp.ok) {
        showError((resp && resp.error) || '开始录制失败');
        return;
      }
      el.recName.value = '';
      await refreshState();
    } finally {
      el.startBtn.disabled = false;
    }
  });

  /** 停止录制 */
  el.stopBtn.addEventListener('click', async function () {
    showError('');
    el.stopBtn.disabled = true;
    try {
      var resp = await send({ type: 'stop' });
      await refreshState();
      if (!resp || !resp.ok) {
        showError((resp && resp.error) || '停止录制失败');
        return;
      }
      if (resp.warn) showError(resp.warn);
      el.note.value = '';
      lastAnnotation = '';
    } finally {
      el.stopBtn.disabled = false;
    }
  });

  /** 保存上一步备注 */
  el.noteBtn.addEventListener('click', async function () {
    showError('');
    var text = el.note.value.trim();
    el.noteBtn.disabled = true;
    try {
      var resp = await send({ type: 'annotate', text: text });
      if (!resp || !resp.ok) {
        showError((resp && resp.error) || '备注保存失败');
        return;
      }
      lastAnnotation = text;
      if (resp.step && resp.step.annotation !== undefined) {
        // 服务端可能对备注做规范化，回填服务端口径
        el.note.value = resp.step.annotation || text;
        lastAnnotation = el.note.value;
      }
      await refreshState();
    } finally {
      el.noteBtn.disabled = false;
    }
  });

  /** 回车键开关（本地 storage，content 每次按键读取） */
  el.injectBtn.addEventListener('click', async function () {
    showError('');
    el.injectBtn.disabled = true;
    try {
      var resp = await send({ type: 'inject' });
      if (!resp || !resp.ok) {
        showError((resp && resp.error) || '注入失败：该页面不允许注入脚本（chrome:// 等系统页）');
        return;
      }
      await refreshState();
    } finally {
      el.injectBtn.disabled = false;
    }
  });

  el.enterSwitch.addEventListener('change', function () {
    showError('');
    var on = el.enterSwitch.checked === true;
    try {
      chrome.storage.local.set({ [ENTER_STORAGE_KEY]: on }, function () {
        if (chrome.runtime.lastError) showError('保存开关失败：' + chrome.runtime.lastError.message);
      });
    } catch (e) {
      showError('保存开关失败：' + ((e && e.message) || String(e)));
    }
  });

  /** 打开控制台 */
  el.consoleLink.addEventListener('click', function (ev) {
    ev.preventDefault();
    openConsole('/#/recordings');
  });

  /** 接收 background 广播（popup 打开时即时刷新） */
  chrome.runtime.onMessage.addListener(function (msg) {
    if (msg && msg.type === 'uitest:state' && msg.state) render(msg.state);
  });

  /* ---------------- 初始化 ---------------- */

  async function init() {
    // 1) 回车键开关
    try {
      var data = await chrome.storage.local.get(ENTER_STORAGE_KEY);
      el.enterSwitch.checked = data && data[ENTER_STORAGE_KEY] === true;
    } catch (e) {
      el.enterSwitch.checked = false;
    }

    // 2) 首次状态与健康检查
    await refreshState();
    await checkHealth();

    // 3) 定时刷新（打开期间每 1000ms）
    setInterval(function () { refreshState().catch(function () { /* 忽略瞬时错误 */ }); }, POLL_MS);
    setInterval(function () { checkHealth().catch(function () { /* 忽略瞬时错误 */ }); }, HEALTH_MS);
  }

  init().catch(function (e) {
    showError('弹窗初始化失败：' + ((e && e.message) || String(e)));
  });
})();
