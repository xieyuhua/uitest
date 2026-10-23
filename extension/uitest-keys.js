/*
 * uitest 共享运行时（唯一实现，插件与服务端执行器共用同一份文件）
 * 载体：window.__uitestKeys        版本：2.0（重建安全版）
 *
 * 为什么要有 2.0：Vue / React 项目每次打包都会变一批东西——
 *   · CSS Modules / scoped 的 hash 类名（.product-card_1a2b3c、.css-9f8e7d）
 *   · 作用域属性 data-v-7ba5bd90
 *   · 自动生成的 id（Element Plus 的 el-id-3812-12、React 18 的 :r1:、input-3、rc_select_7）
 *   · 被打乱/替换的包装 div 层级
 * 这些「构建产物」一律不参与定位；只认业务语义：标签/类型/name/label/placeholder/aria/表单项标题/表格列/可见文本。
 *
 * 定位优先级（从最稳到最不稳）：
 *   uid(data-uitest-id，由本文件的确定性算法算出)
 *   → attr(data-testid/data-test/data-cy 等团队自己写的稳定属性)
 *   → id/name（仅在"看起来稳定"时使用，hash 样式的自动 id 会被跳过）
 *   → label（表单项标题）→ near（标题 → 同一表单项里的控件）
 *   → role（ARIA 角色 + 可访问名）→ placeholder → text（按钮/链接文字）
 *   → cell（表格行文字 + 按钮文字）→ css（纯结构路径，不带 class）→ xpath
 * 全部失败时用元素指纹做自愈打分，唯一最高分才认。
 */
(function () {
  'use strict';
  if (window.__uitestKeys && window.__uitestKeys.version === '2.0') return;

  var KEY_ATTR = 'data-uitest-id';
  var VERSION = '2.0';

  /* ---------------- 基础工具 ---------------- */

  function norm(s) {
    if (s === null || s === undefined) return '';
    return String(s).replace(/[\s\u00a0]+/g, ' ').trim();
  }

  function fnv1a32(str) {
    var h = 0x811c9dc5;
    for (var i = 0; i < str.length; i++) {
      h ^= str.charCodeAt(i);
      h += (h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24);
      h = h >>> 0;
    }
    return ('0000000' + h.toString(16)).slice(-8);
  }

  function sleep(ms) {
    return new Promise(function (r) { setTimeout(r, ms); });
  }

  function isVisible(el) {
    if (!el || !el.getBoundingClientRect) return false;
    var st;
    try { st = window.getComputedStyle(el); } catch (e) { return false; }
    if (!st || st.display === 'none' || st.visibility === 'hidden' || st.opacity === '0') return false;
    var r = el.getBoundingClientRect();
    if (r.width > 0 || r.height > 0) return true;
    // 尺寸为 0 有两种情况：真的零尺寸，或者页面在后台没渲染（CDP 后台标签页拿不到 layout）。
    // 后者很常见，不能当成"不可见"，否则语义定位在后台标签页里会整体失效。
    return true;
  }

  function textOf(node, limit) {
    if (!node) return '';
    // innerText 依赖渲染：后台标签页里常常是空串。这里以 innerText 为主，
    // 取不到就退回 textContent（不依赖布局），保证语义匹配在任何状态下都成立。
    var t = norm(node.innerText || '');
    if (!t) t = norm(node.textContent || '');
    if (limit && t.length > limit) t = t.slice(0, limit);
    return t;
  }

  /* ---------------- 「构建产物」识别：这些值不参与定位 ---------------- */

  // 作者自己写的、跨构建稳定的属性（有就用，优先级仅次于 uid）
  var STABLE_ATTRS = ['data-testid', 'data-test-id', 'data-test', 'data-cy', 'data-qa', 'data-e2e'];

  // 形如哈希 / 自动增长的 token：出现在 id/name 里就认为"打包后会变"
  var UNSTABLE_RES = [
    /^[a-f0-9]{6,}$/i,                       // 纯十六进制哈希
    /^[a-z]+[-_]?[a-f0-9]{6,}$/i,            // product-card_1a2b3c
    /^css-[a-z0-9]+$/i,                      // emotion
    /^sc-[a-zA-Z0-9]+$/,                     // styled-components
    /^jsx-\d+$/i,                            // styled-jsx
    /^el-id-\d+(-\d+)?$/i,                   // Element Plus useId
    /^:r[0-9a-z]+:$/i,                       // React 18 useId
    /^(mui|radix|headlessui|downshift|rc|ant)-?[-_]?[a-z0-9-]*\d+$/i,
    /^(input|field|select|radio|checkbox|form|text|textarea|button|comp|node|item|row|col|div|span|el)[-_]?\d+$/i,
    /^\d+$/,                                 // 纯数字 id
    /^(ember|ng|vue|react|svelte)[-_]?\d+[-_]?\d*$/i,
    /[_-][a-f0-9]{5,}$/i                     // 尾巴带 hash
  ];

  function isUnstableToken(v) {
    var s = norm(v);
    if (!s) return true;
    if (s.length > 64) return false;
    for (var i = 0; i < UNSTABLE_RES.length; i++) {
      if (UNSTABLE_RES[i].test(s)) return true;
    }
    return false;
  }

  function stableId(el) {
    var id = norm(el && el.getAttribute && el.getAttribute('id'));
    return id && !isUnstableToken(id) ? id : '';
  }

  function stableName(el) {
    var nm = norm(el && el.getAttribute && el.getAttribute('name'));
    return nm && !isUnstableToken(nm) ? nm : '';
  }

  function stableAttr(el) {
    if (!el || !el.getAttribute) return null;
    for (var i = 0; i < STABLE_ATTRS.length; i++) {
      var v = norm(el.getAttribute(STABLE_ATTRS[i]));
      if (v) return { name: STABLE_ATTRS[i], value: v };
    }
    return null;
  }

  /* ---------------- 业务语义提取 ---------------- */

  var LABELISH_SEL = 'label, .el-form-item__label, .ant-form-item-label, .arco-form-label-item, .ivu-form-item-label, legend, th, dt';

  // 最近一层「表单项 / 行」容器：组件库里标题和控件是兄弟节点，靠容器把二者绑起来。
  // 注意顺序：必须先试组件库的精确类名，再用模糊匹配——
  // 否则 .el-form-item__content 这种"内部容器"会抢先命中，导致拿不到表单项标题。
  function formItemOf(el) {
    if (!el) return null;
    var exact = '.el-form-item, .ant-form-item, .arco-form-item, .ivu-form-item, .van-field, fieldset, tr';
    var node = el.closest ? el.closest(exact) : null;
    if (node) return node;
    var fuzzy = el.closest ? el.closest('[class*="form-item"], [class*="formItem"], [class*="field"]') : null;
    if (fuzzy) return fuzzy;
    return el.parentElement && el.parentElement.parentElement ? el.parentElement.parentElement : null;
  }

  function labelOf(el) {
    if (!el) return '';
    var v = norm(el.getAttribute && el.getAttribute('aria-label'));
    if (v) return v;

    var tag = (el.tagName || '').toLowerCase();
    var role = norm(el.getAttribute && el.getAttribute('role'));
    // 按钮/链接：自身可见文本就是名字，不去祖先里找（否则会捡到表头、列表文字）
    if (tag === 'button' || tag === 'a' || tag === 'summary' || role === 'button' || role === 'link' || role === 'tab') {
      var own = textOf(el, 60);
      if (own) return own;
    }
    // label[for=id]：即使 id 不稳定也能靠 label 文字定位
    var id = norm(el.getAttribute && el.getAttribute('id'));
    if (id) {
      var lab = document.querySelector('label[for="' + cssEscape(id) + '"]');
      if (lab) { v = textOf(lab, 60); if (v) return v; }
    }
    var wrap = el.closest ? el.closest('label') : null;
    if (wrap) { v = textOf(wrap, 60); if (v) return v; }

    var item = formItemOf(el);
    if (item) {
      var cands = item.querySelectorAll(LABELISH_SEL);
      for (var i = 0; i < cands.length; i++) {
        if (cands[i].contains(el)) continue;
        v = textOf(cands[i], 60);
        if (v && v.length <= 40) return v.replace(/[:：]\s*$/, '');
      }
    }
    var prev = el.previousElementSibling, hop = 0;
    while (prev && hop < 2) {
      v = textOf(prev, 60);
      if (v && v.length <= 40) return v.replace(/[:：]\s*$/, '');
      prev = prev.previousElementSibling; hop++;
    }
    v = norm(el.getAttribute && el.getAttribute('placeholder'));
    return v ? v.replace(/^请输入|^请选择/, '') : '';
  }

  /** 表格列的列名（表头文字）——行内按钮靠「列名 + 行文字」定位非常稳 */
  function columnOf(el) {
    var cell = el && el.closest ? el.closest('td,th') : null;
    if (!cell) return '';
    var table = cell.closest ? cell.closest('table') : null;
    if (!table) return '';
    var idx = 0, sib = cell;
    while ((sib = sib.previousElementSibling)) idx += (sib.colSpan || 1);
    var rows = table.querySelectorAll('thead tr');
    var headerRow = rows.length ? rows[rows.length - 1] : table.querySelector('tr');
    if (!headerRow) return '';
    var cells = headerRow.children, pos = 0;
    for (var i = 0; i < cells.length; i++) {
      var span = cells[i].colSpan || 1;
      if (idx >= pos && idx < pos + span) return textOf(cells[i], 40);
      pos += span;
    }
    return '';
  }

  /** 同一行/同一表单项里的首个文本（表格行指纹） */
  function rowTextOf(el) {
    var row = el && el.closest ? el.closest('tr, [role="row"], li') : null;
    if (!row) return '';
    var cells = row.querySelectorAll('td, [role="cell"]');
    if (cells.length) {
      var first = textOf(cells[0], 40);
      if (first) return first;
    }
    return textOf(row, 40);
  }

  var KIND_RULES = [
    [/价格|金额|单价|售价|费用|price|amount|money|fee/i, 'price'],
    [/库存|数量|件数|次数|人数|stock|count|qty|quantity|num/i, 'number'],
    [/手机|电话|联系方式|mobile|phone|tel/i, 'phone'],
    [/邮箱|邮件|email|mail/i, 'email'],
    [/网址|链接|地址|url|link|href/i, 'url'],
    [/时间|日期|开始|结束|date|time/i, 'datetime'],
    [/图片|照片|封面|图标|上传|附件|image|photo|cover|upload|file/i, 'file'],
    [/描述|详情|介绍|备注|内容|说明|desc|remark|content|note/i, 'textarea'],
    [/密码|password|pwd/i, 'password']
  ];

  function fieldKind(el, label) {
    var tag = (el.tagName || '').toLowerCase();
    var type = (el.getAttribute && el.getAttribute('type') || '').toLowerCase();
    if (tag === 'select') return 'select';
    if (tag === 'textarea') return 'textarea';
    if (type === 'checkbox' || type === 'radio') return 'checkbox';
    if (type === 'file') return 'file';
    if (type === 'date' || type === 'datetime-local' || type === 'month' || type === 'time') return 'date';
    if (type === 'number') return 'number';
    if (type === 'password') return 'password';
    if (type === 'email') return 'email';
    var hay = norm(label + ' ' + (el.getAttribute('name') || '') + ' ' + (el.getAttribute('placeholder') || '') + ' ' + (el.id || ''));
    for (var i = 0; i < KIND_RULES.length; i++) {
      if (KIND_RULES[i][0].test(hay)) return KIND_RULES[i][1];
    }
    return 'text';
  }

  function implicitRole(el) {
    var tag = (el.tagName || '').toLowerCase();
    var type = (el.getAttribute && el.getAttribute('type') || '').toLowerCase();
    if (tag === 'button') return 'button';
    if (tag === 'a' && el.getAttribute('href')) return 'link';
    if (tag === 'select') return 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'input') {
      if (type === 'checkbox' || type === 'radio') return type;
      if (type === 'submit' || type === 'button') return 'button';
      return 'textbox';
    }
    return '';
  }

  /** 元素指纹：录制时存进 step.target，执行时用于自愈打分 */
  function describe(el) {
    var label = labelOf(el);
    var options = [];
    if ((el.tagName || '').toLowerCase() === 'select' && el.options) {
      for (var i = 0; i < el.options.length && options.length < 20; i++) {
        var t = norm(el.options[i].textContent);
        if (t && !/^请选择|^请输/.test(t)) options.push(t);
      }
    }
    var sa = stableAttr(el);
    var item = formItemOf(el);
    // 表格行内的元素：用"行首单元格文字"当行指纹，
    // 不要用整行文本——改版加一列就会变，key 就跟着漂。
    var itemText = '';
    if (item) {
      itemText = (item.tagName === 'TR' || (item.getAttribute && item.getAttribute('role') === 'row'))
        ? rowTextOf(el)
        : textOf(item, 60);
    }
    return {
      tag: (el.tagName || '').toLowerCase(),
      type: (el.getAttribute && el.getAttribute('type') || '').toLowerCase(),
      label: label,
      text: textOf(el, 120),
      placeholder: norm(el.getAttribute && el.getAttribute('placeholder')),
      aria: norm(el.getAttribute && el.getAttribute('aria-label')),
      role: norm(el.getAttribute && el.getAttribute('role')) || implicitRole(el),
      href: (el.getAttribute && el.getAttribute('href')) || '',
      page: location.pathname + location.hash,
      field_kind: fieldKind(el, label),
      options: options,
      // ↓ 2.0 新增：重建安全定位需要的语义
      name: stableName(el),                                   // 只在 name 稳定时给
      stable_id: stableId(el),                                // 只在 id 稳定时给
      test_attr: sa ? sa.name + '=' + sa.value : '',           // data-testid 一类
      form_item: itemText.length > 40 ? '' : itemText,
      column: columnOf(el),
      row_text: rowTextOf(el)
    };
  }

  /* ---------------- CSS / XPath（只走结构，不带 class） ---------------- */

  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/["\\]/g, '\\$&');
  }

  function cssPath(el) {
    if (!el || el.nodeType !== 1) return '';
    var parts = [];
    var node = el;
    while (node && node.nodeType === 1 && node !== document.documentElement) {
      var sid = stableId(node);
      if (sid) { parts.unshift('#' + cssEscape(sid)); break; }   // 只认稳定 id
      var tag = node.tagName.toLowerCase();
      var idx = 1, sib = node;
      while ((sib = sib.previousElementSibling)) {
        if (sib.tagName === node.tagName) idx++;
      }
      parts.unshift(tag + ':nth-of-type(' + idx + ')');
      node = node.parentElement;
    }
    return parts.length ? parts.join(' > ') : '';
  }

  function uniqueSelector(el) {
    if (!el) return '';
    var sid = stableId(el);
    if (sid && document.querySelectorAll('#' + cssEscape(sid)).length === 1) return '#' + cssEscape(sid);
    var nm = stableName(el);
    if (nm) {
      var sel = el.tagName.toLowerCase() + '[name="' + nm.replace(/"/g, '\\"') + '"]';
      if (document.querySelectorAll(sel).length === 1) return sel;
    }
    return cssPath(el);
  }

  /* ---------------- 稳定 key 计算 ---------------- */

  function computeKey(el) {
    var tag = (el.tagName || '').toLowerCase();
    var d = describe(el);
    var sig = [
      tag,
      d.type,
      d.stable_id,      // 只放"看起来稳定"的 id
      d.name,           // 只放稳定的 name
      d.aria,
      d.placeholder,
      d.label,
      d.form_item,
      d.column
    ].filter(function (x) { return !!x; }).join('|');
    var basis = (sig.length > tag.length + 1) ? sig : cssPath(el);
    return 'ut_' + fnv1a32(basis);
  }

  var CANDIDATE_SEL = [
    'a', 'button', 'input', 'select', 'textarea', 'label', 'summary',
    '[role="button"]', '[role="link"]', '[role="tab"]', '[role="menuitem"]', '[role="switch"]', '[role="checkbox"]',
    '[onclick]', '[contenteditable="true"]', '[tabindex]:not([tabindex="-1"])',
    '[data-testid]', '[data-test]', '[data-test-id]', '[data-cy]'
  ].join(',');

  /**
   * 给页面上所有可交互元素补齐 data-uitest-id（幂等）。
   * 同签名元素加 _2 / _3 后缀去重，保证 key 在本页唯一、跨构建顺序一致。
   */
  function markAll(root) {
    var scope = root || document;
    var list = scope.querySelectorAll(CANDIDATE_SEL);
    var seen = {};
    var n = 0;
    for (var i = 0; i < list.length; i++) {
      var el = list[i];
      var existing = el.getAttribute(KEY_ATTR);
      if (existing) {
        seen[existing] = (seen[existing] || 0) + 1;
        continue;
      }
      var k = computeKey(el);
      var count = seen[k] || 0;
      seen[k] = count + 1;
      if (count > 0) k = k + '_' + (count + 1);
      el.setAttribute(KEY_ATTR, k);
      n++;
    }
    return n;
  }

  /** 元素的稳定 key（先 markAll，才能拿到去重后的值） */
  function keyOf(el) {
    if (!el) return '';
    markAll();
    return el.getAttribute(KEY_ATTR) || computeKey(el);
  }

  function clickTargetOf(el) {
    var node = el, hop = 0;
    while (node && node !== document.body && hop < 5) {
      if (node.matches && node.matches(CANDIDATE_SEL)) return node;
      node = node.parentElement; hop++;
    }
    return el;
  }

  /* ---------------- 各定位策略 ---------------- */

  var INTERACTIVE_SEL = 'input:not([type=hidden]), select, textarea, button, a[href], [role="button"], [contenteditable="true"]';

  function pickVisible(list) {
    var first = null;
    for (var i = 0; i < list.length; i++) {
      if (!list[i] || list[i].nodeType !== 1) continue;
      if (!first) first = list[i];
      if (isVisible(list[i])) return list[i];
    }
    return first;
  }

  function byUid(v) {
    try {
      return document.querySelector('[' + KEY_ATTR + '="' + String(v).replace(/"/g, '\\"') + '"]');
    } catch (e) { return null; }
  }

  function byAttr(spec) {
    var p = String(spec).indexOf('=');
    if (p <= 0) return null;
    var name = String(spec).slice(0, p).trim();
    var value = String(spec).slice(p + 1).trim();
    if (!name) return null;
    try {
      return pickVisible(document.querySelectorAll('[' + name + '="' + value.replace(/"/g, '\\"') + '"]'));
    } catch (e) { return null; }
  }

  function byIdOrName(strategy, v) {
    try {
      if (strategy === 'id') {
        return document.getElementById(v) || document.querySelector('[id="' + String(v).replace(/"/g, '\\"') + '"]');
      }
      return pickVisible(document.querySelectorAll('[name="' + String(v).replace(/"/g, '\\"') + '"]'));
    } catch (e) { return null; }
  }

  function byCss(v) {
    try { return pickVisible(document.querySelectorAll(v)); } catch (e) { return null; }
  }

  function byXPath(v) {
    try {
      var r = document.evaluate(v, document, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
      for (var i = 0; i < r.snapshotLength; i++) {
        var n = r.snapshotItem(i);
        if (n && n.nodeType === 1 && isVisible(n)) return n;
      }
      return r.snapshotLength ? r.snapshotItem(0) : null;
    } catch (e) { return null; }
  }

  function byPlaceholder(v) {
    try {
      return pickVisible(document.querySelectorAll('[placeholder="' + String(v).replace(/"/g, '\\"') + '"]'));
    } catch (e) { return null; }
  }

  function byText(v) {
    var want = norm(v);
    if (!want) return null;
    var cands = document.querySelectorAll(CANDIDATE_SEL);
    var exact = null, fuzzy = null;
    for (var i = 0; i < cands.length; i++) {
      var el = cands[i];
      if (!isVisible(el)) continue;
      var t = textOf(el, 120);
      if (!t) continue;
      if (t === want) { if (!exact) exact = el; }
      else if (!fuzzy && t.indexOf(want) === 0 && t.length <= want.length + 24) fuzzy = el;
    }
    return exact || fuzzy;
  }

  /**
   * label：表单项标题 → 同一表单项里的控件。
   * 只走「标题 → 控件」这条关系，不退化到通用容器搜索，
   * 否则像「编辑」这种按钮文字会被当成表单项标题，抓到第一个输入框。
   */
  function byLabel(v) {
    var want = norm(v);
    if (!want) return null;
    // 1) label[for=id] / 包裹型 label
    var labels = document.querySelectorAll('label');
    for (var i = 0; i < labels.length; i++) {
      var lb = labels[i];
      var t = textOf(lb, 60);
      if (!t || t.indexOf(want) < 0) continue;
      var f = lb.getAttribute && lb.getAttribute('for');
      if (f) { var e1 = document.getElementById(f); if (e1) return e1; }
      var inner = lb.querySelector(INTERACTIVE_SEL);
      if (inner) return inner;
    }
    // 2) 组件库表单项：容器里的"标题元素"文字匹配 → 容器里的控件
    var items = document.querySelectorAll('.el-form-item, .ant-form-item, .arco-form-item, .ivu-form-item, .van-field, [class*="form-item"], fieldset');
    var best = null, bestCount = 1 << 30;
    for (var j = 0; j < items.length; j++) {
      var item = items[j];
      if (!isVisible(item)) continue;
      var lab = item.querySelector(LABELISH_SEL);
      if (!lab || lab.contains(item)) continue;
      var lt = textOf(lab, 60);
      if (!lt || lt.indexOf(want) < 0) continue;
      var ctl = item.querySelector(INTERACTIVE_SEL);
      if (!ctl || !isVisible(ctl)) continue;
      var count = item.querySelectorAll('*').length;
      if (count < bestCount) { best = ctl; bestCount = count; }
    }
    return best;
  }

  /**
   * near："标题文字>>tag" —— 在包含该标题文字的【最内层】容器里找控件。
   *
   * 必须取最内层：表单/表格的祖先容器（#app、body、.el-form）里也含有这段文字，
   * 按文档顺序取第一个会直接抓到第一个输入框，造成「填价格却填到商品名称」的错位。
   * 判据：在所有"文字能匹配、且里面确实有目标控件"的容器里，取后代元素最少的那个。
   */
  function byNear(spec, fallbackTag) {
    var parts = String(spec).split('>>');
    var want = norm(parts[0]);
    var tagSel = norm(parts[1]) || fallbackTag || '';
    if (!want) return null;
    var sel = tagSel || INTERACTIVE_SEL;
    var containers = document.querySelectorAll(
      '.el-form-item, .ant-form-item, .arco-form-item, .ivu-form-item, .van-field, [class*="form-item"], ' +
      'label, fieldset, tr, td, li, dl, dt, dd, div, span'
    );
    var best = null, bestCount = 1 << 30;
    for (var i = 0; i < containers.length; i++) {
      var c = containers[i];
      if (c === document.body || c === document.documentElement) continue;
      if (!isVisible(c)) continue;
      var t = textOf(c, 300);
      if (!t || t.indexOf(want) < 0) continue;
      var cands = c.querySelectorAll(sel);
      var hit = null;
      for (var j = 0; j < cands.length; j++) {
        if (cands[j] !== c && isVisible(cands[j])) { hit = cands[j]; break; }
      }
      if (!hit) continue;
      var count = c.querySelectorAll('*').length;
      if (count < bestCount) { best = hit; bestCount = count; }
    }
    return best;
  }

  function accessibleName(el) {
    if (!el) return '';
    var v = norm(el.getAttribute && el.getAttribute('aria-label'));
    if (v) return v;
    var lb = el.getAttribute && el.getAttribute('aria-labelledby');
    if (lb) {
      var parts = [], ids = lb.split(/\s+/);
      for (var i = 0; i < ids.length; i++) {
        var n = document.getElementById(ids[i]);
        if (n) parts.push(textOf(n, 60));
      }
      v = norm(parts.join(' '));
      if (v) return v;
    }
    var tag = (el.tagName || '').toLowerCase();
    var type = (el.getAttribute && el.getAttribute('type') || '').toLowerCase();
    if (tag === 'input' && (type === 'submit' || type === 'button')) return norm(el.value);
    return norm(el.title || textOf(el, 60));
  }

  /** role："button|保存商品" —— ARIA 角色 + 可访问名 */
  function byRole(spec) {
    var s = String(spec);
    var p = s.indexOf('|');
    var role = p > 0 ? norm(s.slice(0, p)) : 'button';
    var wantName = p > 0 ? norm(s.slice(p + 1)) : norm(s);
    var sel = '[role="' + role + '"]';
    if (role === 'button') sel += ', button, input[type=submit], input[type=button]';
    if (role === 'link') sel += ', a[href]';
    if (role === 'checkbox') sel += ', input[type=checkbox]';
    if (role === 'combobox') sel += ', select';
    var cands = document.querySelectorAll(sel);
    if (!wantName) return pickVisible(cands);
    for (var i = 0; i < cands.length; i++) {
      if (isVisible(cands[i]) && accessibleName(cands[i]) === wantName) return cands[i];
    }
    for (var j = 0; j < cands.length; j++) {
      if (isVisible(cands[j]) && accessibleName(cands[j]).indexOf(wantName) >= 0) return cands[j];
    }
    return null;
  }

  /** cell："行文字>>按钮文字" —— 表格/列表行内操作 */
  function byCell(spec) {
    var s = String(spec);
    var p = s.indexOf('>>');
    var rowWant = norm(p >= 0 ? s.slice(0, p) : s);
    var btnWant = norm(p >= 0 ? s.slice(p + 2) : '');
    if (!rowWant) return null;
    var rows = document.querySelectorAll('tr, [role="row"], li, .el-table__row');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      if (!isVisible(row)) continue;
      var t = textOf(row, 400);
      if (!t || t.indexOf(rowWant) < 0) continue;
      if (row.querySelectorAll('*').length > 300) continue;
      var acts = row.querySelectorAll('button, a[href], [role="button"], [class*="btn"], [class*="link"]');
      if (btnWant) {
        for (var j = 0; j < acts.length; j++) {
          if (isVisible(acts[j]) && accessibleName(acts[j]) === btnWant) return acts[j];
        }
        for (var k = 0; k < acts.length; k++) {
          if (isVisible(acts[k]) && accessibleName(acts[k]).indexOf(btnWant) >= 0) return acts[k];
        }
      }
      for (var m = 0; m < acts.length; m++) {
        if (isVisible(acts[m])) return acts[m];
      }
    }
    return null;
  }

  /* ---------------- 自愈打分 ---------------- */

  function healBy(target) {
    if (!target) return null;
    var wantLabel = norm(target.label), wantPh = norm(target.placeholder),
        wantText = norm(target.text), wantAria = norm(target.aria),
        wantName = norm(target.name), wantId = norm(target.stable_id),
        wantItem = norm(target.form_item), wantCol = norm(target.column),
        wantTag = (target.tag || '').toLowerCase(), wantType = (target.type || '').toLowerCase(),
        wantAttr = norm(target.test_attr);
    if (!wantLabel && !wantPh && !wantText && !wantAria && !wantName && !wantAttr && !wantItem && !wantCol) return null;

    var cands = document.querySelectorAll(CANDIDATE_SEL);
    var best = [], bestScore = 0;
    for (var i = 0; i < cands.length; i++) {
      var el = cands[i];
      if (!isVisible(el)) continue;
      var s = 0, why = [];

      var tag = (el.tagName || '').toLowerCase();
      if (wantTag && tag === wantTag) { s += 2; why.push('标签一致'); }
      var type = (el.getAttribute && el.getAttribute('type') || '').toLowerCase();
      if (wantType && type === wantType) s += 1;

      var sa = stableAttr(el);
      if (wantAttr && sa && (sa.name + '=' + sa.value) === wantAttr) { s += 6; why.push('测试属性一致'); }
      if (wantId && stableId(el) === wantId) { s += 5; why.push('稳定 id 一致'); }
      if (wantName && stableName(el) === wantName) { s += 4; why.push('name 一致'); }

      var lb = norm(labelOf(el)), ph = norm(el.getAttribute && el.getAttribute('placeholder')),
          tx = textOf(el, 120), ar = norm(el.getAttribute && el.getAttribute('aria-label'));
      if (wantLabel) {
        if (lb === wantLabel) { s += 5; why.push('标签文字完全一致'); }
        else if (lb && (lb.indexOf(wantLabel) >= 0 || wantLabel.indexOf(lb) >= 0)) { s += 3; why.push('标签文字包含'); }
      }
      if (wantPh) {
        if (ph === wantPh) { s += 4; why.push('placeholder 一致'); }
        else if (ph && (ph.indexOf(wantPh) >= 0 || wantPh.indexOf(ph) >= 0)) s += 2;
      }
      if (wantAria && ar === wantAria) { s += 3; why.push('aria 一致'); }
      if (wantText) {
        if (tx === wantText) { s += 3; why.push('文本一致'); }
        else if (tx && (tx.indexOf(wantText) >= 0 || wantText.indexOf(tx) >= 0)) s += 1;
      }
      // 跨构建最抗打的两个语义：表单项标题、表格列名
      if (wantItem) {
        var item = formItemOf(el);
        var itemText = item ? textOf(item, 200) : '';
        if (itemText && itemText.indexOf(wantItem) >= 0) { s += 4; why.push('同一表单项'); }
      }
      if (wantCol) {
        var col = columnOf(el);
        if (col && col === wantCol) { s += 3; why.push('同一表格列'); }
      }

      if (s > bestScore) { bestScore = s; best = [{ el: el, why: why }]; }
      else if (s === bestScore && s > 0) best.push({ el: el, why: why });
    }
    if (bestScore < 5 || best.length !== 1) return null;
    return { el: best[0].el, score: bestScore, why: best[0].why.join('、') };
  }

  /* ---------------- 定位总入口 ---------------- */

  function resolve(locator, target) {
    var tried = [];
    var plan = [];
    if (locator && locator.strategy) plan.push({ strategy: locator.strategy, value: locator.value });
    var fb = (locator && locator.fallbacks) || [];
    for (var i = 0; i < fb.length; i++) if (fb[i] && fb[i].strategy) plan.push({ strategy: fb[i].strategy, value: fb[i].value });

    for (var j = 0; j < plan.length; j++) {
      var st = plan[j].strategy, v = plan[j].value, el = null;
      switch (st) {
        case 'uid': el = byUid(v); break;
        case 'attr': el = byAttr(v); break;
        case 'id': el = byIdOrName('id', v); break;
        case 'name': el = byIdOrName('name', v); break;
        case 'label': el = byLabel(v); break;
        case 'near': el = byNear(v); break;
        case 'role': el = byRole(v); break;
        case 'cell': el = byCell(v); break;
        case 'placeholder': el = byPlaceholder(v); break;
        case 'text': el = byText(v); break;
        case 'css': el = byCss(v); break;
        case 'xpath': el = byXPath(v); break;
        default: el = null;
      }
      if (el) return { el: el, healed: false, used: st + '=' + v, tried: tried };
      tried.push(st + '=' + v);
    }
    markAll();
    var h = healBy(target);
    if (h) {
      return {
        el: h.el, healed: true,
        used: 'uid=' + (h.el.getAttribute(KEY_ATTR) || '') + ' (自愈:' + h.why + ')',
        tried: tried
      };
    }
    return { el: null, healed: false, used: '', tried: tried };
  }

  /* ---------------- 动作执行 ---------------- */

  function setValue(el, value) {
    var v = value === null || value === undefined ? '' : String(value);
    el.focus && el.focus();
    if (el.isContentEditable) {
      el.textContent = v;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return;
    }
    var proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    var desc = Object.getOwnPropertyDescriptor(proto, 'value');
    if (desc && desc.set) desc.set.call(el, v); else el.value = v;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    el.dispatchEvent(new Event('blur', { bubbles: false }));
  }

  function pickSelect(el, value) {
    var want = norm(value);
    var opts = el.options || [];
    for (var i = 0; i < opts.length; i++) {
      if (norm(opts[i].value) === want || norm(opts[i].textContent) === want) return opts[i].value;
    }
    for (var j = 0; j < opts.length; j++) {
      if (norm(opts[j].textContent).indexOf(want) >= 0) return opts[j].value;
    }
    return null;
  }

  function pressKey(key) {
    var el = document.activeElement || document.body;
    var evs = [
      new KeyboardEvent('keydown', { key: key, code: key, bubbles: true, cancelable: true }),
      new KeyboardEvent('keypress', { key: key, code: key, bubbles: true, cancelable: true }),
      new KeyboardEvent('keyup', { key: key, code: key, bubbles: true, cancelable: true })
    ];
    for (var i = 0; i < evs.length; i++) el.dispatchEvent(evs[i]);
    if (key === 'Enter' && el.form && el.form.requestSubmit) {
      try { el.form.requestSubmit(); } catch (e) { /* ignore */ }
    }
  }

  function checkExpect(expect) {
    var e = norm(expect);
    if (!e) return { ok: true };
    if (e.indexOf('url:') === 0) {
      var part = e.slice(4).trim();
      return { ok: location.href.indexOf(part) >= 0, message: '当前 URL: ' + location.href + '，期望包含: ' + part };
    }
    if (e.indexOf('js:') === 0) {
      try {
        var ok = !!(new Function('return (' + e.slice(3) + ')')).call(window);
        return { ok: ok, message: ok ? '' : '断言表达式为假: ' + e.slice(3) };
      } catch (err) {
        return { ok: false, message: '断言表达式报错: ' + err.message };
      }
    }
    var text = document.body ? (document.body.innerText || '') : '';
    var hit = text.indexOf(e) >= 0;
    return { ok: hit, message: hit ? '' : '页面未出现期望文本: ' + e, snippet: hit ? '' : norm(text).slice(0, 200) };
  }

  async function runAction(req) {
    var action = req.action, value = req.value, timeoutMs = req.timeoutMs || 8000;
    var locator = req.locator, target = req.target;

    if (action === 'navigate') return { ok: true, message: '导航到 ' + value, navigate: value };
    if (action === 'script') {
      try { new Function(value).call(window); return { ok: true, message: '执行脚本' }; }
      catch (e) { return { ok: false, message: '脚本报错: ' + e.message }; }
    }
    if (action === 'assert') {
      var r0 = checkExpect(value);
      return { ok: r0.ok, message: r0.message || '断言通过', snippet: r0.snippet };
    }
    if (action === 'wait') {
      var v = norm(value);
      if (/^\d+$/.test(v)) { await sleep(parseInt(v, 10)); return { ok: true, message: '等待 ' + v + 'ms' }; }
      var t0 = Date.now();
      while (Date.now() - t0 < timeoutMs) {
        if (document.body && (document.body.innerText || '').indexOf(v) >= 0) return { ok: true, message: '等待到文本: ' + v };
        await sleep(200);
      }
      return { ok: false, message: '等待超时，未出现文本: ' + v };
    }

    markAll();   // 先补齐 uid，保证第一优先级能命中
    var found = resolve(locator, target);

    // 只定位不操作：给"批量刷新 uid"用
    if (action === 'resolve') {
      if (!found.el) {
        return { ok: false, message: '元素未找到（已尝试 ' + (found.tried.length ? found.tried.join(' → ') : '无定位') + '）', tried: found.tried, url: location.href };
      }
      var el0 = found.el;
      var d0 = describe(el0);
      return {
        ok: true, used: found.used, healed: found.healed, tried: found.tried,
        uid: el0.getAttribute(KEY_ATTR) || '', css: uniqueSelector(el0),
        tag: d0.tag, label: d0.label, test_attr: d0.test_attr || '', url: location.href
      };
    }
    if (!found.el) {
      return {
        ok: false,
        message: '元素未找到（已尝试 ' + (found.tried.length ? found.tried.join(' → ') : '无定位') + '，自愈也未命中）',
        tried: found.tried,
        url: location.href
      };
    }
    var el = found.el;
    el.scrollIntoView && el.scrollIntoView({ block: 'center', inline: 'nearest' });
    var info = describe(el);
    var base = {
      used: found.used, healed: found.healed, tried: found.tried,
      tag: info.tag, label: info.label, css: uniqueSelector(el), url: location.href,
      uid: el.getAttribute(KEY_ATTR) || '',       // 命中元素的稳定 uid（供自动回写）
      test_attr: info.test_attr || ''              // 有没有 data-testid 一类锚点
    };

    function withOk(ok, message, extra) {
      var o = { ok: ok, message: message };
      for (var k in base) o[k] = base[k];
      if (extra) for (var k2 in extra) o[k2] = extra[k2];
      return o;
    }

    try {
      switch (action) {
        case 'click': {
          if (el.disabled) return withOk(false, '元素处于禁用状态');
          if (!isVisible(el)) return withOk(false, '元素不可见，无法点击');
          el.click();
          return withOk(true, '点击 ' + (info.text && info.text.length <= 30 ? info.text : (info.label || info.tag)));
        }
        case 'input': {
          if (el.disabled || el.readOnly) return withOk(false, '输入框禁用或只读');
          setValue(el, value);
          await sleep(60);
          var actual = el.value === undefined ? el.textContent : el.value;
          if (String(actual) !== String(value === undefined ? '' : value)) {
            return withOk(false, '输入后取值不一致：期望 ' + value + '，实际 ' + actual);
          }
          return withOk(true, '输入「' + value + '」到 ' + (info.label || info.placeholder || info.tag));
        }
        case 'select': {
          var picked = pickSelect(el, value);
          if (picked === null) return withOk(false, '下拉框未找到选项: ' + value);
          el.value = picked;
          el.dispatchEvent(new Event('input', { bubbles: true }));
          el.dispatchEvent(new Event('change', { bubbles: true }));
          return withOk(true, '选择「' + value + '」到 ' + (info.label || info.tag));
        }
        case 'check':
        case 'uncheck': {
          var want = action === 'check';
          if (!!el.checked === want) return withOk(true, '已处于' + (want ? '勾选' : '未勾选') + '状态');
          el.click();
          return withOk(true, (want ? '勾选' : '取消勾选') + ' ' + (info.label || info.tag));
        }
        case 'hover': {
          ['mouseover', 'mouseenter', 'mousemove'].forEach(function (t) {
            el.dispatchEvent(new MouseEvent(t, { bubbles: t !== 'mouseenter', cancelable: true }));
          });
          return withOk(true, '悬停 ' + (info.label || info.text || info.tag));
        }
        case 'key': {
          pressKey(norm(value) || 'Enter');
          return withOk(true, '按键 ' + (norm(value) || 'Enter'));
        }
        case 'upload':
          return withOk(true, '上传目标已定位');
        default:
          return withOk(false, '未知动作: ' + action);
      }
    } catch (e) {
      return withOk(false, '执行异常: ' + (e && e.message ? e.message : String(e)));
    }
  }

  window.__uitestKeys = {
    version: VERSION,
    KEY_ATTR: KEY_ATTR,
    fnv1a32: fnv1a32,
    norm: norm,
    computeKey: computeKey,
    keyOf: keyOf,
    markAll: markAll,
    resolve: resolve,
    describe: describe,
    labelOf: labelOf,
    formItemOf: formItemOf,
    columnOf: columnOf,
    fieldKind: fieldKind,
    clickTargetOf: clickTargetOf,
    uniqueSelector: uniqueSelector,
    cssPath: cssPath,
    isVisible: isVisible,
    isUnstableToken: isUnstableToken,
    stableId: stableId,
    stableAttr: stableAttr,
    runAction: runAction,
    checkExpect: checkExpect
  };
})();
