package browser

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/cdp"
)

// UiActionEvent 页面里录到的一次 UI 操作（由注入脚本通过 Runtime.addBinding 回传）。
//
// 这是「接口采集顺带录 UI」的原始数据：元素定位 + 动作 + 用户填的值，
// 服务端会把它转成 UI 自动化步骤（可回放 / 可当前置登录）。
type UiActionEvent struct {
	Action      string `json:"action"`
	Selector    string `json:"selector"`
	Fallback    string `json:"fallback"`
	StableID    string `json:"stable_id"`
	Tag         string `json:"tag"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Text        string `json:"text"`
	Placeholder string `json:"placeholder"`
	Value       string `json:"value"`
	Masked      bool   `json:"masked"`
	PageURL     string `json:"page_url"`
	TS          int64  `json:"ts"`
}

// uiBindingName 页面脚本往 Go 回传事件用的全局函数名。
const uiBindingName = "__uitestUiAction"

// UiCapture 一个标签页上的 UI 操作录制器。
type UiCapture struct {
	page   *Page
	cb     func(UiActionEvent)
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	stop   bool
	subs   []func()
	script string
}

// StartUiCapture 在这个标签页上开始录制 UI 操作。
//
// 做法：Runtime.addBinding 暴露一个全局函数，页面里的监听脚本把每次点击 / 输入 /
// 下拉 / 勾选 / 路由变化序列化成 JSON 调它；同时用 addScriptToEvaluateOnNewDocument
// 保证页面跳转后（登录跳转、SPA 重新加载）仍然有效。
func (p *Page) StartUiCapture(cb func(UiActionEvent)) (*UiCapture, error) {
	c := &UiCapture{page: p, cb: cb, done: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	_ = p.call(ctx, "Runtime.enable", nil, nil)
	// 重复调用无害：已经注册过会报 "already registered"，忽略即可
	if err := p.call(ctx, "Runtime.addBinding", map[string]any{"name": uiBindingName}, nil); err != nil {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "already") && !strings.Contains(msg, "exists") {
			return nil, err
		}
	}

	var add struct {
		Identifier string `json:"identifier"`
	}
	params := map[string]any{"source": uiRecorderJS, "runImmediately": true}
	if err := p.call(ctx, "Page.addScriptToEvaluateOnNewDocument", params, &add); err != nil {
		// 老版本内核没有 runImmediately：退回普通注入 + 当前文档手动执行一次
		if err2 := p.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": uiRecorderJS}, &add); err2 != nil {
			return nil, err2
		}
	}
	c.script = add.Identifier
	// 当前文档也立刻装上（addScriptToEvaluateOnNewDocument 只对之后的导航生效）
	_, _ = p.Eval(ctx, uiRecorderJS)

	ch, unsub := p.client.Subscribe("Runtime.bindingCalled")
	c.subs = append(c.subs, unsub)
	go c.loop(ch)
	return c, nil
}

func (c *UiCapture) loop(ch <-chan cdp.Event) {
	for {
		select {
		case <-c.done:
			return
		case ev := <-ch:
			c.mu.Lock()
			stop := c.stop
			c.mu.Unlock()
			if stop {
				return
			}
			if ev.SessionID != "" && c.page.SessionID != "" && ev.SessionID != c.page.SessionID {
				continue
			}
			var p struct {
				Name    string `json:"name"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				continue
			}
			if p.Name != uiBindingName || p.Payload == "" {
				continue
			}
			var act UiActionEvent
			if err := json.Unmarshal([]byte(p.Payload), &act); err != nil {
				continue
			}
			if strings.TrimSpace(act.Action) == "" {
				continue
			}
			if c.cb != nil {
				c.cb(act)
			}
		}
	}
}

// Stop 停止录制：解除订阅并移除「新文档自动注入」的脚本。
func (c *UiCapture) Stop() {
	c.once.Do(func() {
		c.mu.Lock()
		c.stop = true
		c.mu.Unlock()
		close(c.done)
		for _, f := range c.subs {
			f()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if c.script != "" {
			_ = c.page.call(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": c.script}, nil)
		}
		cancel()
	})
}

// uiRecorderJS 注入到页面的 UI 操作录制脚本。
//
// 约束：
//   - 只用普通字符串拼接（不能出现反引号/模板字符串，Go 里是原始字符串）；
//   - 多次注入必须幂等（window.__uitestUiRec）；
//   - 任何异常都不能影响页面本身（全部 try/catch 吞掉）。
const uiRecorderJS = `(function () {
  if (window.__uitestUiRec) { return; }
  window.__uitestUiRec = 1;
  var BIND = '__uitestUiAction';

  function send(o) {
    try {
      var f = window[BIND];
      if (typeof f === 'function') { f(JSON.stringify(o)); }
    } catch (e) {}
  }
  function clean(s) {
    return String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  }
  function esc(s) {
    try { return CSS.escape ? CSS.escape(s) : String(s).replace(/([^\w-])/g, '\\$1'); }
    catch (e) { return String(s).replace(/([^\w-])/g, '\\$1'); }
  }
  function stableId(v) {
    if (!v) { return ''; }
    if (/^\d/.test(v)) { return ''; }
    if (/[0-9a-f]{8,}/i.test(v)) { return ''; }        // 组件库自动 id / hash
    if (/^(el|vu|ant|rc|mui)-?\d+$/i.test(v)) { return ''; }
    return v;
  }
  function stableClass(c) {
    if (!c) { return false; }
    if (c.length > 32) { return false; }
    if (/^(css-|sc-|jsx-|v-|svelte-)/.test(c)) { return false; }
    if (/\d{4,}/.test(c)) { return false; }
    return true;
  }
  function attrSel(tag, name, val) {
    return tag + '[' + name + '="' + String(val).replace(/"/g, '\\"') + '"]';
  }
  function segment(el) {
    var tag = el.tagName.toLowerCase();
    var tid = el.getAttribute('data-testid') || el.getAttribute('data-test-id') || el.getAttribute('data-uitest-id');
    if (tid) { return attrSel(tag, 'data-testid', tid); }
    var id = stableId(el.getAttribute('id'));
    if (id) { return tag + '#' + esc(id); }
    var attr = el.getAttribute('name') || el.getAttribute('formcontrolname');
    if (attr) { return attrSel(tag, el.getAttribute('name') ? 'name' : 'formcontrolname', attr); }
    var cls = [];
    try {
      for (var i = 0; i < el.classList.length && cls.length < 2; i++) {
        if (stableClass(el.classList[i])) { cls.push(el.classList[i]); }
      }
    } catch (e) {}
    var s = tag + cls.map(function (c) { return '.' + esc(c); }).join('');
    var parent = el.parentElement;
    if (parent) {
      var same = [];
      for (var j = 0; j < parent.children.length; j++) {
        var kid = parent.children[j];
        if (kid.tagName !== el.tagName) { continue; }
        if (!cls.length) { same.push(kid); continue; }
        var hit = cls.every(function (c) { return kid.classList && kid.classList.contains(c); });
        if (hit) { same.push(kid); }
      }
      if (same.length > 1) { s += ':nth-of-type(' + (Array.prototype.indexOf.call(parent.children, el) + 1) + ')'; }
    }
    return s;
  }
  function selectorFor(el) {
    var parts = [];
    var cur = el;
    var hops = 0;
    while (cur && cur.nodeType === 1 && hops++ < 6) {
      var seg = segment(cur);
      parts.unshift(seg);
      var sel = parts.join(' > ');
      try {
        if (document.querySelectorAll(sel).length === 1) { return sel; }
      } catch (e) {}
      if (seg.indexOf('#') >= 0 || seg.indexOf('data-testid=') >= 0) { return sel; }
      var p = cur.parentElement;
      if (!p || p === document.body || p === document.documentElement) { break; }
      cur = p;
    }
    return parts.join(' > ');
  }
  function fallbackFor(el) {
    var t = clean(el.innerText || el.value || el.getAttribute('aria-label'));
    if (t && t.length <= 40 && el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA') { return 'text=' + t; }
    var ph = el.getAttribute('placeholder');
    if (ph) { return 'placeholder=' + ph; }
    var lb = labelOf(el);
    if (lb) { return 'label=' + lb; }
    return '';
  }
  function labelOf(el) {
    try {
      var aria = clean(el.getAttribute('aria-label'));
      if (aria) { return aria; }
      var lb = el.closest('label');
      if (lb) { var t1 = clean(lb.innerText); if (t1) { return t1; } }
      var id = el.getAttribute('id');
      if (id) {
        var forLb = document.querySelector('label[for="' + String(id).replace(/"/g, '\\"') + '"]');
        if (forLb) { var t2 = clean(forLb.innerText); if (t2) { return t2; } }
      }
      // 只在「离元素最近的表单项容器」里找标题：
      // 往上无脑 querySelector('label') 会把整个 form 的第一个 label（账号）安到密码框 / 按钮上，
      // 回放时 label=账号 会命中错的元素。
      var box = null;
      try { box = el.closest('.el-form-item, .ant-form-item, .arco-form-item, .ivu-form-item, [class*="form-item"], .form-group, .field, td'); } catch (e2) {}
      if (box) {
        var cand = box.querySelector('.el-form-item__label, .form-label, .ant-form-item-label, label');
        if (cand && cand !== el) {
          var t3 = clean(cand.innerText);
          if (t3) { return t3; }
        }
      }
      var prev = el.previousElementSibling;
      if (prev && prev.tagName === 'LABEL') {
        var t4 = clean(prev.innerText);
        if (t4) { return t4; }
      }
    } catch (e) {}
    return '';
  }
  function info(el) {
    return {
      selector: selectorFor(el),
      fallback: fallbackFor(el),
      stable_id: el.getAttribute('data-testid') || el.getAttribute('data-uitest-id') || stableId(el.getAttribute('id')) || '',
      tag: el.tagName.toLowerCase(),
      type: (el.getAttribute('type') || '').toLowerCase(),
      name: el.getAttribute('name') || el.getAttribute('formcontrolname') || '',
      label: labelOf(el),
      text: clean(el.innerText || '').slice(0, 60),
      placeholder: el.getAttribute('placeholder') || '',
      page_url: location.href
    };
  }
  function emit(action, el, extra) {
    var o = info(el);
    o.action = action;
    o.ts = Date.now();
    if (extra) { for (var k in extra) { o[k] = extra[k]; } }
    send(o);
  }
  function isTextField(el) {
    if (!el || el.nodeType !== 1) { return false; }
    var tag = el.tagName;
    if (tag === 'TEXTAREA') { return true; }
    if (tag !== 'INPUT') { return false; }
    var t = (el.getAttribute('type') || 'text').toLowerCase();
    return ['text', 'password', 'email', 'number', 'search', 'tel', 'url', 'date', 'datetime-local', 'month', 'time', 'week'].indexOf(t) >= 0;
  }
  function interactive(el) {
    if (!el || el.nodeType !== 1) { return null; }
    var t = null;
    try {
      t = el.closest('button, a, input, select, textarea, [role="button"], [role="menuitem"], [role="tab"], [role="checkbox"], [role="radio"], [role="switch"], [contenteditable="true"]');
    } catch (e) {}
    if (t) { return t; }
    try {
      return el.closest('.el-button, .el-checkbox, .el-radio, .el-switch, .ant-btn, .ant-checkbox, .ant-radio, .btn, .dropdown-item, .el-select-dropdown__item');
    } catch (e) { return null; }
  }
  function routeEmit() {
    send({ action: 'navigate', selector: '', fallback: '', stable_id: '', tag: '', type: '', name: '', label: '', text: '', placeholder: '', value: location.href, masked: false, page_url: location.href, ts: Date.now() });
  }

  var lastClick = { sel: '', at: 0 };
  var timers = new WeakMap();

  document.addEventListener('click', function (ev) {
    try {
      var el = interactive(ev.target);
      if (!el) { return; }
      if (isTextField(el) || el.tagName === 'SELECT' || el.tagName === 'TEXTAREA') { return; }
      var o = info(el);
      if (lastClick.sel === o.selector && Date.now() - lastClick.at < 400) { return; }
      lastClick = { sel: o.selector, at: Date.now() };
      var t = (el.getAttribute('type') || '').toLowerCase();
      if (t === 'checkbox' || t === 'radio') {
        emit(el.checked ? 'check' : 'uncheck', el, { text: o.label || o.text });
        return;
      }
      emit('click', el);
    } catch (e) {}
  }, true);

  document.addEventListener('input', function (ev) {
    try {
      var el = ev.target;
      if (!isTextField(el)) { return; }
      var old = timers.get(el);
      if (old) { clearTimeout(old); }
      timers.set(el, setTimeout(function () { emitInput(el); }, 500));
    } catch (e) {}
  }, true);

  function emitInput(el) {
    try {
      timers.delete(el);
      var t = (el.getAttribute('type') || '').toLowerCase();
      if (t === 'password') {
        emit('input', el, { value: '', masked: true });
        return;
      }
      var v = el.value == null ? el.textContent : el.value;
      emit('input', el, { value: String(v == null ? '' : v).slice(0, 200) });
    } catch (e) {}
  }

  document.addEventListener('change', function (ev) {
    try {
      var el = ev.target;
      if (!el || el.nodeType !== 1) { return; }
      if (el.tagName === 'SELECT') {
        var txt = '';
        try { txt = el.options[el.selectedIndex] ? el.options[el.selectedIndex].text : ''; } catch (e2) {}
        emit('select', el, { value: clean(el.value), text: clean(txt) || clean(el.value) });
        return;
      }
      if (isTextField(el)) {
        var old = timers.get(el);
        if (old) { clearTimeout(old); }
        emitInput(el);
      }
    } catch (e) {}
  }, true);

  document.addEventListener('keydown', function (ev) {
    try {
      if (ev.key !== 'Enter') { return; }
      var el = ev.target;
      if (!isTextField(el)) { return; }
      emit('key', el, { value: 'Enter' });
    } catch (e) {}
  }, true);

  document.addEventListener('submit', function (ev) {
    try {
      var f = ev.target;
      if (!f || f.tagName !== 'FORM') { return; }
      // 点「提交」按钮时已经记过一条 click，这里再补 Enter 会让回放重复提交一次。
      if (Date.now() - lastClick.at < 800) { return; }
      emit('key', f, { value: 'Enter', text: '提交表单' });
    } catch (e) {}
  }, true);

  window.addEventListener('hashchange', routeEmit);
  window.addEventListener('popstate', routeEmit);
  try {
    var push = history.pushState;
    var replace = history.replaceState;
    history.pushState = function () {
      var r = push.apply(this, arguments);
      setTimeout(routeEmit, 60);
      return r;
    };
    history.replaceState = function () {
      var r = replace.apply(this, arguments);
      setTimeout(routeEmit, 60);
      return r;
    };
  } catch (e) {}

  // 页面一加载就补一条「打开这一页」：回放流程能自己走到当前页面，
  // 登录后整页跳转（location.href）也能被记成一步。
  try {
    setTimeout(function () {
      if (!/^https?:/i.test(String(location.href || ''))) { return; }
      routeEmit();
    }, 30);
  } catch (e) {}
})();`
