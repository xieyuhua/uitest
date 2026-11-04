/*
 * 接口录制器 —— 页面侧网络钩子（MAIN world）
 *
 * 为什么需要它：MV3 的 content script 跑在隔离世界里，看不到页面自己的 fetch/XHR；
 * 而 chrome.webRequest 拿不到响应体。所以在页面主世界里包一层 fetch / XMLHttpRequest，
 * 把「方法 / URL / 状态 / 请求头 / 请求体 / 响应体（截断）」抓下来，
 * 通过 window.postMessage 交给 content script，再由 background 批量上报服务端。
 *
 * 约定：
 *   - 只做观测，不改写请求/响应，也不吞异常（原始 Promise / 事件原样返回）；
 *   - 默认关闭，收到 { source: 'uitest-netctl', enable: true } 才开始记录；
 *   - 只记录文本类响应（JSON/HTML/JS/XML/表单），二进制不抓；
 *   - 大响应体截断，避免把内存和带宽吃满。
 */
(function () {
  'use strict';

  if (window.__uitestNetHook) return;
  window.__uitestNetHook = true;

  var SOURCE = 'uitest-nethook';
  var CTL = 'uitest-netctl';
  var MAX_BODY = 262144;      // 响应体最多保留 256KB
  var MAX_REQ_BODY = 65536;   // 请求体最多保留 64KB

  var cfg = { on: false, skipPrefixes: ['chrome-extension://'], maxBody: MAX_BODY };
  var seq = 0;

  function absUrl(u) {
    try { return new URL(String(u), location.href).href; } catch (e) { return String(u || ''); }
  }

  function shouldSkip(url) {
    if (!url) return true;
    for (var i = 0; i < cfg.skipPrefixes.length; i++) {
      if (cfg.skipPrefixes[i] && url.indexOf(cfg.skipPrefixes[i]) === 0) return true;
    }
    return false;
  }

  function trim(s, max) {
    if (typeof s !== 'string') return '';
    return s.length > max ? s.slice(0, max) : s;
  }

  function isTextLike(ct) {
    if (!ct) return true; // 没给 content-type 的按文本试一下（截断在下面兜底）
    return /json|text|xml|javascript|x-www-form-urlencoded|graphql/i.test(ct);
  }

  function headersToObj(h) {
    var out = {};
    if (!h) return out;
    try {
      if (typeof Headers !== 'undefined' && h instanceof Headers) {
        h.forEach(function (v, k) { out[k] = v; });
        return out;
      }
      if (Array.isArray(h)) {
        h.forEach(function (pair) { if (pair && pair.length >= 2) out[String(pair[0])] = String(pair[1]); });
        return out;
      }
      if (typeof h === 'object') {
        Object.keys(h).forEach(function (k) {
          if (h[k] !== undefined && h[k] !== null) out[k] = String(h[k]);
        });
      }
    } catch (e) { /* 忽略 */ }
    return out;
  }

  function post(type, item) {
    try {
      window.postMessage({ source: SOURCE, type: type, item: item || null }, '*');
    } catch (e) { /* 页面被 CSP 限制时静默 */ }
  }

  function emit(rec) {
    if (!cfg.on) return;
    seq++;
    rec.id = rec.id || ('h' + Date.now().toString(36) + '-' + seq);
    rec.at = new Date().toISOString();
    rec.page_url = location.href;
    if (rec.url) rec.url = absUrl(rec.url);
    if (shouldSkip(rec.url)) return;
    post('request', rec);
  }

  /* ------------------------------------------------------------ fetch */

  var origFetch = window.fetch;
  if (typeof origFetch === 'function') {
    window.fetch = function (input, init) {
      var started = Date.now();
      var info = { method: 'GET', url: '', reqHeaders: {}, postData: '' };
      try {
        if (typeof input === 'string') info.url = input;
        else if (input && typeof input.url === 'string') info.url = input.url;
        info.method = String((init && init.method) || (input && input.method) || 'GET').toUpperCase();
        info.reqHeaders = headersToObj((init && init.headers) || (input && input.headers));
        if (init && typeof init.body === 'string') info.postData = trim(init.body, MAX_REQ_BODY);
        else if (input && typeof input.body === 'string') info.postData = trim(input.body, MAX_REQ_BODY);
      } catch (e) { /* 忽略取值异常 */ }

      var p = origFetch.apply(this, arguments);
      if (!cfg.on || shouldSkip(absUrl(info.url))) return p;

      try {
        p.then(function (resp) {
          var ct = '';
          try { ct = resp.headers.get('content-type') || ''; } catch (e) { }
          var item = {
            method: info.method,
            url: info.url,
            type: 'Fetch',
            status: resp.status || 0,
            mime_type: ct,
            duration_ms: Date.now() - started,
            req_headers: info.reqHeaders,
            post_data: info.postData
          };
          if (!isTextLike(ct)) { emit(item); return; }
          resp.clone().text().then(function (text) {
            item.body_preview = trim(text, cfg.maxBody);
            item.body_size = text.length;
            item.has_body = text.length > 0;
            item.body_truncated = text.length > cfg.maxBody;
            item.bytes = text.length;
            emit(item);
          }).catch(function () { emit(item); });
        }, function (err) {
          emit({
            method: info.method, url: info.url, type: 'Fetch', status: 0,
            duration_ms: Date.now() - started, req_headers: info.reqHeaders,
            post_data: info.postData, error: (err && err.message) || 'network error'
          });
        });
      } catch (e) { /* 观测失败不影响页面 */ }
      return p;
    };
  }

  /* -------------------------------------------------------------- XHR */

  var XHR = window.XMLHttpRequest;
  if (XHR && XHR.prototype) {
    var origOpen = XHR.prototype.open;
    var origSetHeader = XHR.prototype.setRequestHeader;
    var origSend = XHR.prototype.send;

    XHR.prototype.open = function (method, url) {
      try {
        this.__uitestNet = {
          method: String(method || 'GET').toUpperCase(),
          url: url,
          headers: {},
          started: 0,
          postData: ''
        };
      } catch (e) { /* 忽略 */ }
      return origOpen.apply(this, arguments);
    };

    XHR.prototype.setRequestHeader = function (k, v) {
      try {
        if (this.__uitestNet) this.__uitestNet.headers[String(k)] = String(v);
      } catch (e) { /* 忽略 */ }
      return origSetHeader.apply(this, arguments);
    };

    XHR.prototype.send = function (body) {
      var xhr = this;
      try {
        var meta = xhr.__uitestNet;
        if (meta && cfg.on) {
          meta.started = Date.now();
          if (typeof body === 'string') meta.postData = trim(body, MAX_REQ_BODY);
          var done = false;
          var handler = function () {
            if (done) return;
            done = true;
            var ct = '';
            try { ct = xhr.getResponseHeader('content-type') || ''; } catch (e) { }
            var text = '';
            try {
              if (!xhr.responseType || xhr.responseType === 'text') text = xhr.responseText || '';
            } catch (e) { text = ''; }
            var item = {
              method: meta.method,
              url: meta.url,
              type: 'XHR',
              status: xhr.status || 0,
              mime_type: ct,
              duration_ms: Date.now() - meta.started,
              req_headers: meta.headers,
              post_data: meta.postData
            };
            if (text) {
              item.body_preview = trim(text, cfg.maxBody);
              item.body_size = text.length;
              item.has_body = true;
              item.body_truncated = text.length > cfg.maxBody;
              item.bytes = text.length;
            }
            emit(item);
          };
          xhr.addEventListener('loadend', handler);
        }
      } catch (e) { /* 忽略 */ }
      return origSend.apply(this, arguments);
    };
  }

  /* ---------------------------------------------------------- 开关控制 */

  window.addEventListener('message', function (ev) {
    var d = ev.data;
    if (!d || d.source !== CTL) return;
    cfg.on = d.enable === true;
    if (Array.isArray(d.skipPrefixes) && d.skipPrefixes.length) {
      cfg.skipPrefixes = d.skipPrefixes;
    }
    if (typeof d.maxBody === 'number' && d.maxBody > 0) {
      cfg.maxBody = d.maxBody;
    }
  }, false);

  // 注入后主动报个到，让 content script 把当前开关状态推下来
  post('ready');
})();
