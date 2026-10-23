/*
 * UI 自动化录制器 —— background（MV3 service worker）
 *
 * 职责（本文件是会话状态的唯一写入者）：
 *   1) 会话状态读写：chrome.storage.local 中的 "uitest_state"，结构见 README。
 *   2) 所有 HTTP 请求的唯一出口：内容脚本只发消息，不直接 fetch，避免页面 CSP / 跨域干扰。
 *   3) 标签页导航与关闭监听：自动补 navigate 步骤、关闭录制标签页时自动结束录制。
 *   4) 角标（badge）显示当前步骤数，并向内容脚本 / 弹窗广播状态。
 *
 * 接口契约见 docs/API.md，字段名统一 snake_case。
 */
'use strict';

/* ---------------- 常量 ---------------- */

const STATE_KEY = 'uitest_state';          // 对外会话状态
const INTERNAL_KEY = 'uitest_internal';    // 内部簿记（输入合并 / 导航去重），不属于对外状态
const DEFAULT_SERVER_URL = 'http://127.0.0.1:8490';
const RECENT_LIMIT = 10;                   // 最近录制条数上限
const BADGE_COLOR = '#4f46e5';

/** 默认会话状态（字段与 popup 展示一致） */
const DEFAULT_STATE = {
  serverUrl: DEFAULT_SERVER_URL,
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

/** 内部簿记默认值：用于判断"上一步是不是同一个输入框"和"导航是否重复" */
const DEFAULT_INTERNAL = {
  lastStepAction: '',
  lastStepLocatorValue: '',
  lastNavUrl: '',
  readyTabId: null,   // 最近一次内容脚本报到来自哪个标签页
  readyAt: 0          // 报到时间戳（用于判断"录制脚本是否就位"）
};

/* ---------------- 小工具 ---------------- */

/** 规范化服务端地址：补协议、去掉尾部斜杠 */
function normalizeServerUrl(url) {
  let u = String(url || '').trim();
  if (!u) return DEFAULT_SERVER_URL;
  if (!/^https?:\/\//i.test(u)) u = 'http://' + u;
  return u.replace(/\/+$/, '');
}

/** 本地时间 ISO 字符串（带时区偏移，如 2026-02-01T10:00:00+08:00） */
function nowIso() {
  const d = new Date();
  const pad = (n) => String(Math.floor(Math.abs(n))).padStart(2, '0');
  const off = -d.getTimezoneOffset();               // 分钟，东八区为 +480
  const sign = off >= 0 ? '+' : '-';
  return (
    d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
    'T' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) +
    sign + pad(off / 60) + ':' + pad(Math.abs(off) % 60)
  );
}

/** 录制默认名：未命名录制 02-01 10:00 */
function formatStamp() {
  const d = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

/* ---------------- 状态读写 ---------------- */

/** 把任意来源的状态对象补齐成完整结构 */
function normalizeState(raw) {
  const src = raw && typeof raw === 'object' ? raw : {};
  const s = Object.assign({}, DEFAULT_STATE, src);
  s.serverUrl = normalizeServerUrl(s.serverUrl);
  s.stepCount = Number(s.stepCount) || 0;
  s.recordingId = String(s.recordingId || '');
  s.recordingName = String(s.recordingName || '');
  s.startUrl = String(s.startUrl || '');
  s.lastStepId = String(s.lastStepId || '');
  s.lastStepAnnotation = String(s.lastStepAnnotation || '');
  s.active = s.active === true;
  if (typeof s.tabId !== 'number') s.tabId = null;
  s.recent = Array.isArray(src.recent)
    ? src.recent
      .filter((it) => it && it.id)
      .slice(0, RECENT_LIMIT)
      .map((it) => ({ id: String(it.id), name: String(it.name || ''), step_count: Number(it.step_count) || 0 }))
    : [];
  return s;
}

/** 读取会话状态（总是以 storage 为准，避免 service worker 被回收后脏缓存） */
async function getState() {
  const data = await chrome.storage.local.get(STATE_KEY);
  return normalizeState(data[STATE_KEY]);
}

/**
 * 写入会话状态，并同步角标与广播。
 * @param {object} next 新状态
 * @param {number} [tabIdHint] 需要通知录制标签页的 id（停止录制时 state.tabId 已清空，需显式传入）
 */
async function saveState(next, tabIdHint) {
  const state = normalizeState(next);
  await chrome.storage.local.set({ [STATE_KEY]: state });
  updateBadge(state);
  pushToPopup(state);
  const tid = typeof tabIdHint === 'number' ? tabIdHint : (typeof state.tabId === 'number' ? state.tabId : null);
  if (tid !== null) pushToTab(tid, state);
  return state;
}

/** 读取内部簿记 */
async function getInternal() {
  const data = await chrome.storage.local.get(INTERNAL_KEY);
  const v = data[INTERNAL_KEY];
  return Object.assign({}, DEFAULT_INTERNAL, v && typeof v === 'object' ? v : {});
}

/** 写入内部簿记 */
async function saveInternal(next) {
  await chrome.storage.local.set({ [INTERNAL_KEY]: Object.assign({}, DEFAULT_INTERNAL, next || {}) });
}

/* ---------------- 角标与广播 ---------------- */

/** 角标：录制中显示步骤数，未录制清空 */
function updateBadge(state) {
  try {
    chrome.action.setBadgeBackgroundColor({ color: BADGE_COLOR });
    chrome.action.setBadgeText({ text: state.active ? String(state.stepCount || 0) : '' });
  } catch (e) {
    /* 角标异常不影响录制流程 */
  }
}

/** 广播给 popup（打开时才会收到）以及所有扩展页面；content 收到后会用 tabId 自行比对 */
function pushToPopup(state) {
  try {
    chrome.runtime.sendMessage({ type: 'uitest:state', state }, () => { void chrome.runtime.lastError; });
  } catch (e) {
    /* 弹窗未打开时忽略 */
  }
}

/** 定向广播给某个标签页里的 content script */
function pushToTab(tabId, state) {
  if (typeof tabId !== 'number') return;
  try {
    chrome.tabs.sendMessage(tabId, { type: 'uitest:state', state }, () => { void chrome.runtime.lastError; });
  } catch (e) {
    /* 标签页已关闭或没有 content script 时忽略 */
  }
}

/* ---------------- HTTP 统一出口 ---------------- */

/**
 * 调用服务端接口：拼接 serverUrl + path，JSON 收发，非 2xx 抛出服务端 {error} 文本。
 * @param {string} path 例如 "/api/health"
 * @param {string} [method] 默认 GET
 * @param {object} [body] 有值时按 JSON 提交
 */
async function api(path, method, body) {
  const state = await getState();
  const url = state.serverUrl + path;
  const init = { method: method || 'GET', headers: { 'Content-Type': 'application/json' } };
  if (body !== undefined && body !== null) init.body = JSON.stringify(body);

  let resp;
  try {
    resp = await fetch(url, init);
  } catch (err) {
    throw new Error('无法连接服务端 ' + state.serverUrl + '（' + ((err && err.message) || '网络错误') + '）');
  }

  const text = await resp.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (e) {
      data = null;
    }
  }

  if (!resp.ok) {
    const msg = data && data.error ? data.error : (resp.status + ' ' + (resp.statusText || '请求失败'));
    throw new Error(msg);
  }
  return data;
}

/** 取当前窗口的活动标签页 */
async function currentTab() {
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    return tabs && tabs[0] ? tabs[0] : null;
  } catch (e) {
    return null;
  }
}

/* ---------------- 消息处理 ---------------- */

/** ping：返回状态并顺带探测 /api/health */
async function handlePing() {
  const state = await getState();
  let serverOk = false;
  let health = null;
  let error = '';
  try {
    health = await api('/api/health', 'GET');
    serverOk = !(health && health.ok === false);
  } catch (e) {
    error = e.message;
  }
  return { ok: true, state, serverOk, health, error };
}

/* ---------------- 内容脚本注入 ---------------- */

/**
 * 把录制脚本注入指定标签页。
 * 为什么需要它：content_scripts 只在"页面加载时"注入，所以
 *   - 装/更新插件前就打开的页面不会录制；
 *   - 录制中途新开的标签页也不会录制。
 * 这两种情况过去只会录到 background 自动补的 navigate 步骤（locator=css=html），
 * 看起来就像"定位全是 css=html"。这里用 scripting 权限补注入，content.js 内部有
 * 幂等守卫，重复注入不会重复记录。
 */
async function injectRecorder(tabId) {
  if (typeof tabId !== 'number') return { ok: false, error: '缺少 tabId' };
  try {
    await chrome.scripting.executeScript({
      target: { tabId: tabId, allFrames: true },
      files: ['uitest-keys.js', 'content.js']
    });
    return { ok: true };
  } catch (e) {
    // chrome:// 、扩展商店、PDF 等页面注入必然失败，属于预期
    return { ok: false, error: (e && e.message) || String(e) };
  }
}

/** 录制中把「当前活动标签页」切换为录制标签页（用户在别的标签页继续操作时不再丢步骤） */
async function adoptTab(tabId, tab) {
  const state = await getState();
  if (!state.active || !state.recordingId) return;
  const url = (tab && tab.url) || '';
  if (!/^https?:\/\//i.test(url)) return;          // chrome:// 等页面不接管
  if (state.tabId === tabId) return;
  state.tabId = tabId;
  await saveState(state, tabId);
  await injectRecorder(tabId);
  pushToPopup(state);
}

/** start：创建录制并在本地进入录制态 */
async function handleStart(msg) {
  const state0 = await getState();
  if (state0.active) {
    return { ok: false, error: '已有正在进行的录制：' + (state0.recordingName || state0.recordingId) + '，请先停止' };
  }

  let tabId = typeof msg.tabId === 'number' ? msg.tabId : null;
  let startUrl = String(msg.startUrl || '').trim();
  if (tabId === null || !startUrl) {
    const tab = await currentTab();
    if (tab) {
      if (tabId === null && typeof tab.id === 'number') tabId = tab.id;
      if (!startUrl) startUrl = tab.url || '';
    }
  }

  const name = String(msg.name || '').trim() || ('未命名录制 ' + formatStamp());
  const recording = await api('/api/recordings', 'POST', {
    name,
    start_url: startUrl,
    tags: [],
    description: ''
  });

  const next = Object.assign({}, state0, {
    active: true,
    recordingId: (recording && recording.id) || '',
    recordingName: (recording && recording.name) || name,
    startUrl,
    tabId,
    stepCount: 0,
    lastStepId: '',
    lastStepAnnotation: ''
  });
  if (!next.recordingId) {
    return { ok: false, error: '服务端未返回录制 id，请检查服务端版本' };
  }

  await saveState(next, tabId);
  // 起始页视为"已记录"，避免第一条 tabs.onUpdated 重复记录起始 URL
  await saveInternal({ lastStepAction: '', lastStepLocatorValue: '', lastNavUrl: startUrl });
  // 关键：立即把录制脚本注入录制标签页，用户无需手动刷新页面
  const injected = await injectRecorder(tabId);
  return { ok: true, recording, injected };
}

/** 把内存里的步骤清洗成 API 要求的入参（去掉 id / recording_id / index） */
function cleanStepPayload(step) {
  const out = {
    action: String(step.action || ''),
    annotation: String(step.annotation || ''),
    locator: step.locator || { strategy: 'css', value: '', fallbacks: [], frame_url: '' },
    target: step.target || {},
    data_key: String(step.data_key || ''),
    value: step.value === undefined || step.value === null ? '' : String(step.value),
    expect: String(step.expect || ''),
    wait_ms: Number(step.wait_ms) || 0,
    optional: step.optional === true,
    note: String(step.note || '')
  };
  if (step.created_at) out.created_at = step.created_at;
  return out;
}

/**
 * step：写入一个步骤。
 * 连续输入同一个输入框（同一 uid）时合并到上一步，避免一个字段产生几十条步骤。
 */
async function handleStep(msg) {
  const step = msg && msg.step;
  if (!step || !step.action) return { ok: false, error: '步骤数据为空' };

  const state = await getState();
  if (!state.active || !state.recordingId) return { ok: false, error: '当前没有正在进行的录制' };

  const internal = await getInternal();
  const locatorValue = step.locator && step.locator.value ? String(step.locator.value) : '';
  const canMerge = step.action === 'input' &&
    !!state.lastStepId &&
    internal.lastStepAction === 'input' &&
    !!locatorValue &&
    locatorValue === internal.lastStepLocatorValue;

  const base = '/api/recordings/' + encodeURIComponent(state.recordingId);

  if (canMerge) {
    const body = { value: step.value === undefined || step.value === null ? '' : String(step.value) };
    if (step.annotation) body.annotation = String(step.annotation);
    if (step.note !== undefined) body.note = String(step.note || '');
    if (step.data_key) body.data_key = String(step.data_key);
    const saved = await api(base + '/steps/' + encodeURIComponent(state.lastStepId), 'PATCH', body);
    state.lastStepAnnotation = String(body.annotation || state.lastStepAnnotation || '');
    await saveState(state, state.tabId);
    await saveInternal({
      lastStepAction: 'input',
      lastStepLocatorValue: locatorValue,
      lastNavUrl: internal.lastNavUrl
    });
    return { ok: true, step: saved || null, stepCount: state.stepCount, merged: true };
  }

  const payload = cleanStepPayload(step);
  const saved = await api(base + '/steps', 'POST', payload);
  state.stepCount = (state.stepCount || 0) + 1;
  state.lastStepId = (saved && saved.id) || '';
  state.lastStepAnnotation = payload.annotation;
  await saveState(state, state.tabId);
  await saveInternal({
    lastStepAction: payload.action,
    lastStepLocatorValue: (payload.locator && payload.locator.value) || '',
    lastNavUrl: internal.lastNavUrl
  });
  return { ok: true, step: saved || null, stepCount: state.stepCount, merged: false };
}

/** annotate：给上一步补备注（同时写 annotation 与 note） */
async function handleAnnotate(msg) {
  const state = await getState();
  if (!state.active || !state.recordingId) return { ok: false, error: '当前没有正在进行的录制' };
  if (!state.lastStepId) return { ok: false, error: '还没有记录到任何步骤，无法保存备注' };

  const text = String(msg.text || '').trim();
  const saved = await api(
    '/api/recordings/' + encodeURIComponent(state.recordingId) + '/steps/' + encodeURIComponent(state.lastStepId),
    'PATCH',
    { annotation: text, note: text }
  );
  state.lastStepAnnotation = text;
  await saveState(state, state.tabId);
  return { ok: true, step: saved || null };
}

/** element：登记元素到元素库（key = data-uitest-id） */
async function handleElement(msg) {
  const element = msg && msg.element;
  if (!element) return { ok: false, error: '元素数据为空' };
  const body = {
    page: String(element.page || ''),
    selector: String(element.selector || ''),
    tag: String(element.tag || ''),
    type: String(element.type || ''),
    label: String(element.label || ''),
    descriptor: String(element.descriptor || '')
  };
  if (element.key) body.key = String(element.key);
  const saved = await api('/api/elements/bind', 'POST', body);
  return { ok: true, element: saved || null };
}

/** state：返回状态，并附带请求方的标签页 id（content 用来判断自己是不是录制标签页） */
/** ready：内容脚本注入成功后的报到（用于 popup 显示"录制脚本是否就位"） */
async function handleReady(sender) {
  const state = await getState();
  const selfTabId = sender && sender.tab && typeof sender.tab.id === 'number' ? sender.tab.id : null;
  const internal = await getInternal();
  await saveInternal(Object.assign({}, internal, {
    readyTabId: selfTabId,
    readyAt: Date.now()
  }));
  return {
    ok: true,
    state,
    selfTabId,
    recordingHere: !!(state.active && selfTabId !== null && state.tabId === selfTabId)
  };
}

/** inject：把录制脚本补注入到当前活动标签页（页面在装插件之前就打开时的补救） */
async function handleInject(msg) {
  let tabId = typeof msg.tabId === 'number' ? msg.tabId : null;
  if (tabId === null) {
    const tab = await currentTab();
    if (tab && typeof tab.id === 'number') tabId = tab.id;
  }
  if (tabId === null) return { ok: false, error: '找不到当前标签页' };
  const state = await getState();
  if (state.active) {
    // 顺便把录制标签页切到这一页
    state.tabId = tabId;
    await saveState(state, tabId);
  }
  const res = await injectRecorder(tabId);
  return Object.assign({ ok: res.ok, error: res.error || '', tabId: tabId, state: await getState() }, res);
}

async function handleState(sender) {
  const state = await getState();
  const selfTabId = sender && sender.tab && typeof sender.tab.id === 'number' ? sender.tab.id : null;
  const internal = await getInternal();
  return {
    ok: true,
    state,
    selfTabId,
    recordingHere: !!(state.active && selfTabId !== null && state.tabId === selfTabId),
    recorder: {
      readyTabId: typeof internal.readyTabId === 'number' ? internal.readyTabId : null,
      readyAt: internal.readyAt || 0,
      readyInRecordingTab: !!(state.active && typeof internal.readyTabId === 'number' &&
        internal.readyTabId === state.tabId && (Date.now() - (internal.readyAt || 0) < 15000))
    }
  };
}

/** setServer：保存服务端地址 */
async function handleSetServer(msg) {
  const state = await getState();
  state.serverUrl = normalizeServerUrl(msg.url);
  await saveState(state, state.tabId);
  return { ok: true, state };
}

/**
 * 结束录制（popup 点停止 / 录制标签页被关闭 / 重复点击）。
 * @param {string} reason 结束原因，仅用于回传提示
 */
async function finishRecording(reason) {
  const state = await getState();
  if (!state.active) {
    return { ok: true, recordingId: state.recordingId, stepCount: state.stepCount, alreadyStopped: true, reason: reason || '' };
  }

  const recordingId = state.recordingId;
  const recordingName = state.recordingName;
  const stepCount = state.stepCount;
  const oldTabId = state.tabId;

  let patchError = '';
  if (recordingId) {
    try {
      await api('/api/recordings/' + encodeURIComponent(recordingId), 'PATCH', { status: 'draft' });
    } catch (e) {
      patchError = e.message;
    }
  }

  // recent：新录制置顶、按 id 去重、最多 10 条
  const recent = [];
  if (recordingId) recent.push({ id: recordingId, name: recordingName, step_count: stepCount });
  for (let i = 0; i < state.recent.length && recent.length < RECENT_LIMIT; i++) {
    const it = state.recent[i];
    if (!it || !it.id) continue;
    if (recent.some((x) => x.id === it.id)) continue;
    recent.push({ id: it.id, name: it.name, step_count: it.step_count });
  }

  const next = Object.assign({}, state, { active: false, tabId: null, recent });
  await saveState(next, oldTabId);            // oldTabId 让内容脚本能收到"已停止"并移除浮层
  await saveInternal(DEFAULT_INTERNAL);

  const warn = stepCount > 0 ? '' :
    '这次没有录到任何操作步骤：被测页面可能在安装/更新插件之前就已打开，' +
    '请在录制状态下点「注入录制脚本」或刷新页面后再操作一次。';

  if (patchError) {
    return {
      ok: false,
      error: '本机已停止录制，但服务端保存失败：' + patchError,
      recordingId: recordingId,
      stepCount: stepCount,
      warn: warn,
      reason: reason || ''
    };
  }
  return { ok: true, recordingId: recordingId, stepCount: stepCount, warn: warn, reason: reason || '' };
}

/* ---------------- 导航步骤 ---------------- */

/** 构造 navigate 步骤（locator 标为 url：执行时只按 value 跳转，不做元素定位） */
function buildNavigateStep(url) {
  let page = url;
  try {
    const u = new URL(url);
    page = u.pathname + u.hash;
  } catch (e) {
    page = url;
  }
  return {
    action: 'navigate',
    annotation: '打开页面 ' + url,
    locator: { strategy: 'url', value: url, fallbacks: [], frame_url: '' },
    target: {
      tag: 'html',
      type: '',
      label: '',
      text: '',
      placeholder: '',
      aria: '',
      role: '',
      href: url,
      page: page,
      field_kind: 'other'
    },
    data_key: '',
    value: url,
    expect: '',
    wait_ms: 0,
    optional: false,
    note: '',
    created_at: nowIso()
  };
}

/** 录制标签页加载完成时补一条 navigate 步骤（同一 URL 连续重复不重复记录） */
async function recordNavigate(tabId, tab) {
  const state = await getState();
  if (!state.active || state.tabId !== tabId || !state.recordingId) return;

  const url = (tab && tab.url) || '';
  if (!url) return;

  const internal = await getInternal();
  if (internal.lastNavUrl === url) return;

  const payload = cleanStepPayload(buildNavigateStep(url));
  try {
    const saved = await api('/api/recordings/' + encodeURIComponent(state.recordingId) + '/steps', 'POST', payload);
    state.stepCount = (state.stepCount || 0) + 1;
    state.lastStepId = (saved && saved.id) || '';
    state.lastStepAnnotation = payload.annotation;
    await saveState(state, tabId);
  } catch (e) {
    // 导航步骤写入失败不打断录制，只更新去重标记，避免同一 URL 反复重试
    pushToPopup(state);
  }
  await saveInternal({ lastStepAction: 'navigate', lastStepLocatorValue: '', lastNavUrl: url });
}

/* ---------------- 消息分发 ---------------- */

/** 按消息类型分发，统一返回 Promise（响应对象形如 {ok, ...}） */
async function dispatch(msg, sender) {
  switch (msg.type) {
    case 'ping':
      return handlePing();
    case 'start':
      return handleStart(msg);
    case 'stop':
      return finishRecording('popup');
    case 'step':
      return handleStep(msg);
    case 'annotate':
      return handleAnnotate(msg);
    case 'element':
      return handleElement(msg);
    case 'state':
      return handleState(sender);
    case 'ready':
      return handleReady(sender);
    case 'inject':
      return handleInject(msg);
    case 'setServer':
      return handleSetServer(msg);
    default:
      return { ok: false, error: '未知消息类型：' + String(msg.type) };
  }
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  // 广播消息（uitest:state）不是请求，直接忽略；没有 type 的也忽略
  if (!msg || !msg.type || msg.type === 'uitest:state') return false;
  dispatch(msg, sender)
    .then((res) => sendResponse(res))
    .catch((err) => sendResponse({ ok: false, error: (err && err.message) || String(err) }));
  return true; // 异步回复
});

/* ---------------- 标签页事件 ---------------- */

chrome.tabs.onUpdated.addListener((tabId, changeInfo, tab) => {
  if (!changeInfo || changeInfo.status !== 'complete') return;
  recordNavigate(tabId, tab).catch(() => { /* 静默失败，不打断用户操作 */ });
  // 页面整页刷新后内容脚本会重新注入，这里不做任何事；但如果是录制标签页刚加载完，补一次注入兜底
  getState().then((state) => {
    if (state.active && state.tabId === tabId) injectRecorder(tabId).catch(() => {});
  }).catch(() => {});
});

// 录制中切换标签页：把录制标签页跟过去（否则新标签页里的操作全都录不到）
chrome.tabs.onActivated.addListener((info) => {
  if (!info || typeof info.tabId !== 'number') return;
  chrome.tabs.get(info.tabId)
    .then((tab) => adoptTab(info.tabId, tab))
    .catch(() => { /* 标签页可能已关闭 */ });
});

chrome.tabs.onRemoved.addListener((tabId) => {
  (async () => {
    const state = await getState();
    if (!state.active || state.tabId !== tabId) return;
    await finishRecording('tab-closed');   // 录制标签页被关闭 -> 自动结束并保留进度
  })().catch(() => { /* 忽略 */ });
});

/* ---------------- 生命周期 ---------------- */

chrome.runtime.onInstalled.addListener(() => {
  (async () => {
    const state = await getState();
    await saveState(state);                // 首次安装时把默认状态落盘，popup 才有值可读
  })().catch(() => { /* 忽略 */ });
});

chrome.runtime.onStartup.addListener(() => {
  (async () => {
    // 浏览器重启后 service worker 可能残留"录制中"状态，但内容脚本已不在原页面，统一复位
    const state = await getState();
    if (state.active) {
      await saveState(Object.assign({}, state, { active: false, tabId: null }));
      await saveInternal(DEFAULT_INTERNAL);
    } else {
      updateBadge(state);
    }
  })().catch(() => { /* 忽略 */ });
});
