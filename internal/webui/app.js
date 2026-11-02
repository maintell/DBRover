'use strict';

const state = {
  token: '',
  conns: [],
  current: null,
  tableNext: null,
  tableName: '',
};

const $ = (id) => document.getElementById(id);

// 所有接口路径集中在这里，不再散落成字符串拼接。
// 改动端点时只需改这一处；TestWebuiUsesKnownPaths 会校验每条路径都真实存在。
const API = {
  connections: '/api/v1/connections',
  health: '/api/v1/connections/:id/health',
  tables: '/api/v1/connections/:id/tables',
  rows: '/api/v1/connections/:id/tables/:name/rows',
  exec: '/api/v1/connections/:id/exec',
};

// path 把 :param 占位符替换为实际值。
function path(tpl, params) {
  return tpl.replace(/:([a-z]+)/g, (_, key) => encodeURIComponent(String(params[key])));
}

// ---------- HTTP ----------

// 以 /api/ 开头的模板直接使用，其余视为相对 /api/v1。
async function api(p, opts = {}) {
  const headers = Object.assign(
    { 'Content-Type': 'application/json' },
    opts.headers || {},
  );
  if (state.token) headers['X-DBRover-Token'] = state.token;

  const res = await fetch(p, Object.assign({}, opts, { headers }));
  if (res.status === 204) return null;

  const text = await res.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch { body = null; }
  }
  if (!res.ok) {
    const e = body && body.error ? body.error : {};
    const err = new Error(e.message || res.statusText);
    err.code = e.code || 'HTTP_' + res.status;
    err.hint = e.hint || '';
    err.status = res.status;
    err.detail = e; // 保留完整错误体：需要审批时里面带 approval.token
    throw err;
  }
  return body;
}

// ---------- 令牌 ----------

function readHashToken() {
  const m = location.hash.match(/[#&]t=([^&]+)/);
  if (!m) return '';
  const t = decodeURIComponent(m[1]);
  // 立刻从地址栏与历史里抹掉，避免长期留存
  history.replaceState(null, '', location.pathname);
  return t;
}

function saveToken(t) {
  state.token = t;
  try { sessionStorage.setItem('dbrover.token', t); } catch { /* 隐私模式忽略 */ }
}

function forgetToken() {
  state.token = '';
  try { sessionStorage.removeItem('dbrover.token'); } catch { /* ignore */ }
}

// ---------- 界面工具 ----------

function setStatus(text) { $('status').textContent = text; }

function showNotice(text, kind) {
  const div = document.createElement('div');
  div.className = 'notice' + (kind ? ' ' + kind : '');
  div.textContent = text;
  $('results').replaceChildren(div);
}

function envBadge(env) {
  const el = $('env');
  el.textContent = env || 'unknown';
  el.className = 'badge env-' + (env || 'unknown');
}

function showRisk(risk, friction) {
  const r = $('risk');
  r.hidden = false;
  r.textContent = '风险 ' + risk;
  r.className = 'badge ' + String(risk).toLowerCase();

  const f = $('friction');
  f.hidden = false;
  f.textContent = '打扰 ' + friction;
  f.className = 'badge';
}

// ---------- 对话框 ----------

function dialog(title, bodyHTML, okText) {
  return new Promise((resolve) => {
    $('dlg-title').textContent = title;
    $('dlg-body').innerHTML = bodyHTML;
    $('dlg-ok').textContent = okText || '确定';
    $('dialog').hidden = false;

    const done = (v) => {
      $('dialog').hidden = true;
      $('dlg-ok').onclick = null;
      $('dlg-cancel').onclick = null;
      resolve(v);
    };
    $('dlg-ok').onclick = () => done(true);
    $('dlg-cancel').onclick = () => done(false);
  });
}

async function askApproval(sql, err) {
  // 注意：不展示 err.hint —— 那是给 API 调用方的（例如 approve=true），
  // 界面上的用户点「确认执行」即可，不需要知道协议细节。
  const html =
    '<p>' + escapeHTML(err.message) + '</p>' +
    '<p>确认要执行以下语句吗？</p><pre>' + escapeHTML(sql) + '</pre>' +
    '<p>确认后立即执行；取消则不会执行，也不会有任何改动。</p>';
  return dialog('需要确认', html, '确认执行');
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

// ---------- 连接 ----------

async function loadConnections(preferId) {
  const out = await api(API.connections);
  state.conns = (out && out.items) || [];
  const sel = $('conn');
  sel.replaceChildren();
  for (const c of state.conns) {
    const opt = document.createElement('option');
    opt.value = c.id;
    opt.textContent = c.name || c.id;
    sel.appendChild(opt);
  }
  if (!state.conns.length) {
    envBadge('unknown');
    $('profile').textContent = '无连接';
    $('tables').replaceChildren();

    const box = document.createElement('div');
    box.className = 'notice';
    box.textContent = '还没有连接。先建一个 SQLite 连接：';
    const btn = document.createElement('button');
    btn.className = 'primary';
    btn.textContent = '新建连接';
    btn.style.marginLeft = '8px';
    btn.onclick = newConnection;
    box.appendChild(btn);
    $('results').replaceChildren(box);
    return;
  }
  // 优先选中刚创建/指定的连接，否则回落到第一个。
  const pick = (preferId && state.conns.some((c) => c.id === preferId)) ? preferId : state.conns[0].id;
  sel.value = pick;
  await selectConnection(pick);
}

async function selectConnection(id) {
  const c = state.conns.find((x) => x.id === id);
  if (!c) return;
  state.current = c;
  envBadge(c.environment);
  $('profile').textContent = c.engine + ' · ' + c.access_mode;
  // 切连接后必须清掉上一个连接留下的结果与提示。
  state.tableNext = null;
  state.tableName = '';
  const hint = document.createElement('div');
  hint.className = 'notice';
  hint.textContent = '点左侧对象查看数据，或直接在上方执行 SQL。';
  $('results').replaceChildren(hint);
  setStatus('');
  await loadIdentity(c.id);
  await loadTables();
}

// loadIdentity 展示真实服务器身份。
// 这是「环境标错了也看得出来」的兜底：连接别名可以随手起，指纹不会。
async function loadIdentity(id) {
  const el = $('identity');
  try {
    const h = await api(path(API.health, { id }));
    const ident = h.identity || {};
    const host = ident.hostname || '?';
    const fp = (ident.fingerprint || '').slice(0, 8);
    el.textContent = host + (fp ? ' · ' + fp : '');
    el.title = '服务器身份：' + host + '\n版本：' + (h.version || '?') +
      '\n指纹：' + (ident.fingerprint || '?');
    el.hidden = false;
    if (h.warnings && h.warnings.length) {
      setStatus(h.warnings.join('；'));
    }
  } catch (e) {
    el.textContent = '身份未知';
    el.title = '读取服务器身份失败：' + e.message;
  }
}

function newConnection() {
  const html =
    '<label class="field">名称</label><input id="f-name" style="width:100%" value="local">' +
    '<label class="field">数据库文件路径</label><input id="f-host" style="width:100%" placeholder="demo.db">' +
    '<label class="field">环境</label><select id="f-env" style="width:100%">' +
    '<option value="local">local</option><option value="test">test</option>' +
    '<option value="staging">staging</option><option value="production">production</option>' +
    '</select>' +
    '<label class="field">访问模式</label><select id="f-mode" style="width:100%">' +
    '<option value="read-write">read-write</option><option value="read-only">read-only</option>' +
    '</select>';
  dialog('新建 SQLite 连接', html, '创建').then(async (ok) => {
    if (!ok) return;
    const body = {
      name: $('f-name').value.trim() || 'local',
      engine: 'sqlite',
      host: $('f-host').value.trim(),
      environment: $('f-env').value,
      access_mode: $('f-mode').value,
    };
    try {
      const created = await api(API.connections, { method: 'POST', body: JSON.stringify(body) });
      await loadConnections(created && created.id);
    } catch (e) {
      showNotice('创建失败：' + e.message, 'error');
    }
  });
}

// ---------- 表 ----------

async function loadTables() {
  if (!state.current) return;
  $('tables').replaceChildren();
  try {
    const out = await api(path(API.tables, { id: state.current.id }));
    const items = (out && out.items) || [];
    $('table-count').textContent = items.length + ' 个';
    if (!items.length) {
      const d = document.createElement('div');
      d.className = 'notice';
      d.textContent = '这个库里还没有表。';
      $('tables').appendChild(d);
      return;
    }
    for (const t of items) {
      const b = document.createElement('button');
      b.textContent = t.name;
      b.onclick = () => browseTable(t.name);
      $('tables').appendChild(b);
    }
  } catch (e) {
    showNotice('加载表清单失败：' + e.message, 'error');
  }
}

async function browseTable(name) {
  state.tableName = name;
  state.tableNext = null;
  $('sql').value = 'SELECT * FROM ' + name + ' LIMIT 100';
  await loadTablePage(true);
}

async function loadTablePage(first) {
  if (!state.current) return;
  try {
    let url = path(API.rows, { id: state.current.id, name: state.tableName }) + '?limit=100';
    if (!first && state.tableNext) {
      url += '&after=' + encodeURIComponent(JSON.stringify(state.tableNext));
    }
    const out = await api(url);
    if (first) {
      $('results').replaceChildren();
      setStatus(state.tableName + ' · ' + out.rows.length + ' 行');
    }
    renderTable($('results'), out.columns, out.rows, !first);
    state.tableNext = out.has_more ? out.next : null;
    if (out.has_more) {
      const bar = document.createElement('div');
      bar.className = 'notice';
      const btn = document.createElement('button');
      btn.textContent = '加载更多';
      btn.onclick = () => { bar.remove(); loadTablePage(false); };
      bar.appendChild(btn);
      $('results').appendChild(bar);
    }
    showRisk('R0', 'F0');
  } catch (e) {
    showNotice(e.message + (e.hint ? '（' + e.hint + '）' : ''), 'error');
  }
}

// ---------- 渲染 ----------

function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

function renderTable(container, columns, rows, append) {
  let table = container.querySelector('table');
  if (!append || !table) {
    table = document.createElement('table');
    const thead = document.createElement('thead');
    const tr = document.createElement('tr');
    for (const c of columns) {
      const th = document.createElement('th');
      th.textContent = c.name + (c.type && c.type.raw ? '  ' + c.type.raw : '');
      tr.appendChild(th);
    }
    thead.appendChild(tr);
    table.appendChild(thead);
    table.appendChild(document.createElement('tbody'));
    if (!append) container.replaceChildren(table);
    else container.appendChild(table);
  }

  const tbody = table.querySelector('tbody');
  for (const r of rows) {
    const tr = document.createElement('tr');
    for (let i = 0; i < columns.length; i++) {
      const v = r[i];
      if (v === null || v === undefined) tr.appendChild(cell('NULL', 'null'));
      else if (v === '') tr.appendChild(cell("''", 'empty'));
      else tr.appendChild(cell(String(v)));
    }
    tbody.appendChild(tr);
  }
}

function renderExec(out) {
  $('results').replaceChildren();
  for (const r of out.results || []) {
    if (r.err) {
      showNotice('第 ' + r.index + ' 条失败：' + r.err, 'error');
      return;
    }
    if (r.columns && r.columns.length) {
      renderTable($('results'), r.columns, r.rows || [], false);
    } else {
      const d = document.createElement('div');
      d.className = 'notice';
      d.textContent = '影响 ' + r.affected + ' 行';
      $('results').appendChild(d);
    }
  }
}

// ---------- 执行 ----------

async function runSQL(approvalToken) {
  if (!state.current) { showNotice('请先建立连接。', 'warn'); return; }
  const sql = $('sql').value.trim();
  if (!sql) return;

  setStatus('执行中…');
  $('btn-run').disabled = true;
  try {
    const body = { sql, max_rows: 200 };
    if (approvalToken) body.approval_token = approvalToken;
    const out = await api(path(API.exec, { id: state.current.id }), {
      method: 'POST',
      body: JSON.stringify(body),
    });
    showRisk(out.risk, out.friction);
    renderExec(out);
    setStatus(out.duration_ms + ' ms');
  } catch (e) {
    if (e.code === 'POLICY_APPROVAL_REQUIRED') {
      const token = e.detail && e.detail.approval && e.detail.approval.token;
      const ok = await askApproval(sql, e);
      if (ok && token) {
        $('btn-run').disabled = false;
        // 必须用刚签发的凭证重放「同一段 SQL」：服务端会比对语句指纹，
        // 期间若编辑了文本框，凭证失效并返回 409。
        return runSQL(token);
      }
      if (ok && !token) {
        showNotice('服务端未返回审批凭证，无法执行。', 'error');
      }
      setStatus('已取消');
    } else if (e.code === 'POLICY_READ_ONLY') {
      showNotice('该连接是只读的：' + e.message, 'warn');
      setStatus('已拒绝');
    } else if (e.code === 'POLICY_APPROVAL_MISMATCH') {
      showNotice('审批凭证与当前语句不匹配（可能你改了 SQL）。请重新执行以获取新凭证。', 'warn');
      setStatus('已拒绝');
    } else {
      showNotice(e.message, 'error');
      setStatus('失败');
    }
  } finally {
    $('btn-run').disabled = false;
  }
}

// ---------- 启动 ----------

async function boot() {
  if (!state.token) {
    $('gate').hidden = false;
    $('token').focus();
    return;
  }
  $('gate').hidden = true;
  try {
    await loadConnections();
  } catch (e) {
    if (e.status === 401) {
      forgetToken();
      $('gate').hidden = false;
      return;
    }
    showNotice('初始化失败：' + e.message, 'error');
  }
}

$('btn-token').onclick = () => {
  const t = $('token').value.trim();
  if (!t) return;
  saveToken(t);
  boot();
};

$('token').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') $('btn-token').click();
});

$('conn').onchange = (e) => selectConnection(e.target.value);
$('btn-reload').onclick = () => loadTables();
$('btn-forget').onclick = () => {
  forgetToken();
  location.reload();
};
$('btn-run').onclick = () => runSQL(false);

$('sql').addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault();
    runSQL(false);
  }
});

const initial = readHashToken() || sessionStorage.getItem('dbrover.token') || '';
if (initial) saveToken(initial);
boot();
