'use strict';
(function () {
// ui2.js —— 交互增强层：设置面板、连接管理、多标签编辑器、视觉主题。
// 桌面与团队模式加载的是同一份文件；与 app.js 共用全局 state / API / $ / api /
// showNotice / selectConnection / newConnection / loadConnections / dialog / path。
// 刻意不改动 app.js 里已验收的行为：这里只追加新入口与视觉，不重做执行链路。

var ui2 = { settings: null, tabs: [], active: 0, seq: 2 };

function u2(id) { return document.getElementById(id); }

// ---------- 运行形态 ----------
// 桌面模式由 host.DesktopHandler 注入 window.DBRoverBootstrapToken；
// serve 模式没有，令牌来自 URL 片段 #t=。
function detectMode() {
  var el = u2('mode');
  if (!el) return;
  var desktop = typeof window !== 'undefined' && !!window.DBRoverBootstrapToken;
  el.textContent = desktop ? '桌面' : '服务';
  el.title = desktop ? '桌面应用：进程内窗口，不监听端口' : '团队模式：dbrover serve + 浏览器';
}

// ---------- 设置 ----------
function applySettings(s) {
  if (!s) return;
  ui2.settings = s;
  document.body.dataset.theme = s.theme || 'dark';
  document.documentElement.style.setProperty('--ui-font-size', (s.font_size || 13) + 'px');
  document.documentElement.style.setProperty('--tab-size', String(s.editor_tab_size || 2));
  var dot = u2('settings-indicator');
  if (dot) dot.hidden = !(s.api_key_configured || (s.agent_base_url && s.agent_model));
}

async function loadSettings() {
  if (typeof api !== 'function' || typeof API === 'undefined') return;
  try {
    var s = await api(API.settings);
    applySettings(s);
  } catch (e) {
    // 401：令牌门还没过，进入后会再拉一次；不阻断界面。
    if (e.status !== 401 && typeof showNotice === 'function') {
      showNotice('设置读取失败：' + e.message, 'warn');
    }
  }
}

function opt(val, text, cur) {
  return '<option value="' + val + '"' + (String(val) === String(cur) ? ' selected' : '') + '>' + text + '</option>';
}

function esc(v) {
  return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/"/g, '&quot;')
    .replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function settingsForm(s) {
  var h = '';
  h += '<div class="set-group"><h3>外观</h3>';
  h += '<label class="set-row"><span>主题</span><select id="set-theme">' + opt('dark', '深色', s.theme) + opt('light', '浅色', s.theme) + '</select></label>';
  var fs = ''; for (var i = 12; i <= 20; i += 2) fs += opt(i, i + ' px', s.font_size);
  h += '<label class="set-row"><span>字号</span><select id="set-font">' + fs + '</select></label>';
  var ts = ''; for (var j = 2; j <= 8; j++) ts += opt(j, j + ' 空格', s.editor_tab_size);
  h += '<label class="set-row"><span>缩进</span><select id="set-tab">' + ts + '</select></label>';
  h += '</div>';
  h += '<div class="set-group"><h3>模型服务</h3>';
  h += '<label class="set-row"><span>Base URL</span><input id="set-base" style="width:100%" value="' + esc(s.agent_base_url || '') + '" placeholder="https://api.deepseek.com/v1" autocomplete="off"></label>';
  h += '<label class="set-row"><span>模型</span><input id="set-model" style="width:100%" value="' + esc(s.agent_model || '') + '" placeholder="deepseek-chat" autocomplete="off"></label>';
  h += '<label class="set-row"><span>上下文窗口</span><input id="set-ctx" type="number" min="0" style="width:100%" value="' + (s.agent_context_tokens || 0) + '" placeholder="0 = 未知"></label>';
  h += '<label class="set-row"><span>API Key</span><input id="set-key" type="password" style="width:100%" autocomplete="off" placeholder="' + (s.api_key_configured ? '已配置，留空则不变' : '未配置') + '"></label>';
  h += '<label class="set-row"><span></span><label class="inline"><input type="checkbox" id="set-key-clear"> 清除已保存的 Key</label></label>';
  h += '<p class="set-hint">Key 只写入系统凭据库，不进设置文件、不回显。环境变量可临时覆盖。</p>';
  h += '</div>';
  h += '<div class="set-group"><h3>快捷键</h3><div class="keys">';
  h += '<div><kbd>Ctrl</kbd> + <kbd>Enter</kbd><span>执行当前语句</span></div>';
  h += '<div><kbd>Ctrl</kbd> + <kbd>Shift</kbd> + <kbd>Enter</kbd><span>执行整个脚本</span></div>';
  h += '<div><kbd>Ctrl</kbd> + <kbd>S</kbd><span>保存设置</span></div>';
  h += '<div>点结果标题<span>跳回对应语句</span></div>';
  h += '</div></div>';
  return h;
}

function openSettings() {
  var s = ui2.settings || { theme: 'dark', font_size: 13, editor_tab_size: 2,
    agent_base_url: '', agent_model: '', agent_context_tokens: 0, api_key_configured: false };
  u2('settings-body').innerHTML = settingsForm(s);
  u2('settings-drawer').hidden = false;
}
function closeSettings() { u2('settings-drawer').hidden = true; }

async function saveSettings() {
  var p = {};
  p.theme = u2('set-theme').value;
  p.font_size = Number(u2('set-font').value);
  p.editor_tab_size = Number(u2('set-tab').value);
  p.agent_base_url = u2('set-base').value.trim();
  p.agent_model = u2('set-model').value.trim();
  p.agent_context_tokens = Number(u2('set-ctx').value) || 0;
  var key = u2('set-key').value;
  var clear = u2('set-key-clear').checked;
  if (clear) p.api_key = '';
  else if (key) p.api_key = key;
  try {
    var out = await api(API.settings, { method: 'PATCH', body: JSON.stringify(p) });
    applySettings(out);
    closeSettings();
    if (typeof showNotice === 'function') showNotice('设置已保存', 'ok');
  } catch (e) {
    if (typeof showNotice === 'function') showNotice('保存失败：' + e.message, 'error');
  }
}

// ---------- 连接管理 ----------
function mkBtn(text, fn) { var b = document.createElement('button'); b.textContent = text; b.onclick = fn; return b; }

function noticeNode(text) { var n = document.createElement('div'); n.className = 'notice'; n.textContent = text; return n; }

async function openConnections() { u2('connections-drawer').hidden = false; await renderConnections(); }
function closeConnections() { u2('connections-drawer').hidden = true; }

async function renderConnections() {
  var body = u2('connections-body');
  if (!body) return;
  body.innerHTML = '';
  var list = [];
  try {
    var out = await api(API.connections);
    list = (out && out.items) || [];
  } catch (e) {
    body.appendChild(noticeNode('读取连接失败：' + e.message));
    return;
  }
  if (!list.length) {
    var empty = document.createElement('div');
    empty.className = 'empty-state';
    empty.innerHTML = '<strong>还没有连接</strong><p>新建一个连接，就能浏览表、跑 SQL、看结构。</p>';
    body.appendChild(empty);
    return;
  }
  for (var i = 0; i < list.length; i++) body.appendChild(connCard(list[i]));
}

function connCard(c) {
  var cur = state.current && state.current.id === c.id;
  var d = document.createElement('div');
  d.className = 'conn-card' + (cur ? ' current' : '');
  d.innerHTML = '<div class="conn-card-main"><strong>' + esc(c.name || c.id) + '</strong>'
    + '<span class="badge">' + esc(c.engine) + '</span>'
    + '<span class="badge env-' + esc(c.environment) + '">' + esc(c.environment) + '</span>'
    + '<span class="badge">' + esc(c.access_mode) + '</span></div>';
  var ops = document.createElement('div');
  ops.className = 'conn-card-ops';
  var use = mkBtn('切换', function () { switchConn(c.id); });
  var test = mkBtn('测试', function () { testConn(c.id, d); });
  var del = mkBtn('删除', function () { delConn(c.id); });
  del.className = 'danger';
  ops.appendChild(use); ops.appendChild(test); ops.appendChild(del);
  d.appendChild(ops);
  return d;
}

function switchConn(id) {
  var sel = u2('conn');
  if (sel) sel.value = id;
  if (typeof selectConnection === 'function') selectConnection(id);
  closeConnections();
}

async function testConn(id, card) {
  var slot = card.querySelector('.conn-test');
  if (!slot) { slot = document.createElement('div'); slot.className = 'conn-test'; card.appendChild(slot); }
  slot.textContent = '测试中…';
  try {
    await api(path(API.health, { id: id }));
    slot.textContent = '连接正常'; slot.className = 'conn-test ok';
  } catch (e) {
    slot.textContent = '不可用：' + e.message; slot.className = 'conn-test err';
  }
}

async function delConn(id) {
  var ok = false;
  if (typeof dialog === 'function') ok = await dialog('删除连接', '<p>确认关闭并移除这个连接？正在跑的查询会中断。</p>', '删除');
  if (!ok) return;
  try {
    await api(path(API.conn, { id: id }), { method: 'DELETE' });
    if (typeof loadConnections === 'function') await loadConnections();
    await renderConnections();
  } catch (e) {
    if (typeof showNotice === 'function') showNotice('删除失败：' + e.message, 'error');
  }
}

// ---------- 多标签编辑器 ----------
// 每个标签只存 SQL 文本；结果区属于「最近一次执行」，切标签时清空并提示。
// 刻意不快照结果 DOM：网格里的编辑/审批按钮是带事件绑定的，innerHTML 回放会变成死按钮。
function tabLabel(t) { return t.name || ('查询 ' + t.id); }

function renderTabs() {
  var bar = u2('tab-bar');
  if (!bar) return;
  bar.innerHTML = '';
  for (var i = 0; i < ui2.tabs.length; i++) {
    var t = ui2.tabs[i];
    var b = document.createElement('button');
    b.className = 'sql-tab' + (i === ui2.active ? ' active' : '');
    b.textContent = tabLabel(t);
    (function (idx) { b.onclick = function () { switchTab(idx); }; })(i);
    bar.appendChild(b);
    var x = document.createElement('button');
    x.className = 'tab-x'; x.textContent = '×'; x.title = '关闭标签';
    (function (idx) { x.onclick = function (e) { e.stopPropagation(); closeTab(idx); }; })(i);
    bar.appendChild(x);
  }
  var add = mkBtn('+', addTab); add.className = 'tab-add'; add.title = '新建标签';
  bar.appendChild(add);
  var spacer = document.createElement('span'); spacer.className = 'grow'; bar.appendChild(spacer);
  var clear = mkBtn('清空结果', function () { u2('results').replaceChildren(); });
  clear.className = 'tab-clear'; clear.title = '清空当前结果区';
  bar.appendChild(clear);
}

function currentSQL() { var el = u2('sql'); return el ? el.value : ''; }
function syncActiveSQL() { if (ui2.tabs[ui2.active]) ui2.tabs[ui2.active].sql = currentSQL(); }

function resultHint(text) {
  var r = u2('results');
  if (!r) return;
  r.replaceChildren();
  r.appendChild(noticeNode(text));
}

function switchTab(i) {
  if (i === ui2.active || i < 0 || i >= ui2.tabs.length) return;
  syncActiveSQL();
  ui2.active = i;
  var el = u2('sql'); if (el) el.value = ui2.tabs[i].sql || '';
  resultHint('已切到「' + tabLabel(ui2.tabs[i]) + '」。执行以查看结果。');
  renderTabs(); updateCursor();
}

function addTab() {
  syncActiveSQL();
  var n = ui2.seq++;
  ui2.tabs.push({ id: n, name: '查询 ' + n, sql: '' });
  ui2.active = ui2.tabs.length - 1;
  var el = u2('sql'); if (el) { el.value = ''; el.focus(); }
  resultHint('新标签。写一条 SQL，按 Ctrl+Enter 执行。');
  renderTabs(); updateCursor();
}

function closeTab(i) {
  if (ui2.tabs.length <= 1) return;
  if (i === ui2.active) syncActiveSQL();
  ui2.tabs.splice(i, 1);
  if (ui2.active > i) ui2.active--;
  else if (ui2.active >= ui2.tabs.length) ui2.active = ui2.tabs.length - 1;
  var el = u2('sql'); if (el) el.value = ui2.tabs[ui2.active].sql || '';
  resultHint('已切换标签。执行以查看结果。');
  renderTabs(); updateCursor();
}

function updateCursor() {
  var el = u2('sql'), out = u2('cursor-pos');
  if (!el || !out) return;
  var lines = el.value.slice(0, el.selectionStart).split(String.fromCharCode(10));
  out.textContent = lines.length + ':' + (lines[lines.length - 1].length + 1);
}

// ---------- 接线 ----------
function wireUI2() {
  detectMode();
  var b;
  b = u2('btn-settings'); if (b) b.onclick = openSettings;
  b = u2('btn-connections'); if (b) b.onclick = openConnections;
  b = u2('settings-close'); if (b) b.onclick = closeSettings;
  b = u2('settings-save'); if (b) b.onclick = saveSettings;
  b = u2('connections-close'); if (b) b.onclick = closeConnections;
  b = u2('connections-new'); if (b) b.onclick = function () { closeConnections(); if (typeof newConnection === 'function') newConnection(); };

  var el = u2('sql');
  if (el) {
    ui2.tabs = [{ id: 1, name: '查询 1', sql: el.value }];
    ui2.active = 0; ui2.seq = 2;
    el.addEventListener('input', function () { syncActiveSQL(); updateCursor(); });
    el.addEventListener('click', updateCursor);
    el.addEventListener('keyup', updateCursor);
    renderTabs(); updateCursor();
  }

  // 令牌门「进入」之后补拉一次设置（首次加载可能还没有令牌）。
  var gate = u2('btn-token');
  if (gate && gate.onclick) {
    var prev = gate.onclick;
    gate.onclick = function (e) { prev(e); loadSettings(); detectMode(); };
  }
  // Ctrl+S 在设置面板打开时保存，不抢编辑器里 Ctrl+Enter 的链路。
  document.addEventListener('keydown', function (e) {
    var d = u2('settings-drawer');
    if ((e.ctrlKey || e.metaKey) && e.key === 's' && d && !d.hidden) {
      e.preventDefault(); saveSettings();
    }
  });
  loadSettings();
}

wireUI2();
})();
