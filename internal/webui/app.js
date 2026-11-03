'use strict';

const state = {
  token: '',
  conns: [],
  current: null,
  tableNext: null,
  tableName: '',
  // 视图：当前列的元数据、筛选条件、排序、页大小
  columns: [],
  filter: [],
  order: null,
  limit: 100,
  // 行级编辑：是否允许、主键列、待提交的改动
  editable: false,
  pkCols: [],
  pending: [],
  // 刚应用成功的变更对应的撤销预案（10 分钟窗口，服务端保管）
  undo: null,
  // 分批执行中的进度与停止入口；null 表示没在跑
  bulkRun: null,
  // 视图：data = 行数据，schema = 表结构
  view: 'data',
  schema: null,
  // 导出格式要跨视图刷新保留，否则改个筛选就被重置回 CSV
  exportFormat: 'csv',
};

const $ = (id) => document.getElementById(id);

// 所有接口路径集中在这里，不再散落成字符串拼接。
// 改动端点时只需改这一处；TestWebuiUsesKnownPaths 会校验每条路径都真实存在。
const API = {
  connections: '/api/v1/connections',
  health: '/api/v1/connections/:id/health',
  tables: '/api/v1/connections/:id/tables',
  rows: '/api/v1/connections/:id/tables/:name/rows',
  schema: '/api/v1/connections/:id/tables/:name/schema',
  schemaPlan: '/api/v1/connections/:id/schema/plan',
  export: '/api/v1/connections/:id/export',
  exec: '/api/v1/connections/:id/exec',
  changePlan: '/api/v1/connections/:id/changes/plan',
  bulkPlan: '/api/v1/connections/:id/bulk/plan',
  compare: '/api/v1/compare',
  changeApply: '/api/v1/changes/:pid/apply',
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
  if (!res.ok) throw httpError(res.status, res.statusText, body);
  return body;
}

// httpError 把服务端的 errorBody 转成统一的异常形状。
// 保留完整错误体：需要审批时里面带 approval.token。
function httpError(status, statusText, body) {
  const e = body && body.error ? body.error : {};
  const err = new Error(e.message || statusText);
  err.code = e.code || 'HTTP_' + status;
  err.hint = e.hint || '';
  err.status = status;
  err.detail = e;
  return err;
}

// readNDJSON 边到边解析：进度流的意义就是「先到先显示」，不能等整包下来。
async function readNDJSON(res, onEvent) {
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  const drain = (flush) => {
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (line) onEvent(JSON.parse(line));
    }
    if (flush && buf.trim()) {
      onEvent(JSON.parse(buf.trim()));
      buf = '';
    }
  };
  for (;;) {
    const { value, done } = await reader.read();
    if (value) buf += dec.decode(value, { stream: true });
    drain(false);
    if (done) break;
  }
  drain(true);
}

// applyStream 执行预案并接收逐批进度。
//
// 服务端只在真的有进度可报时才用 NDJSON：被审批拦下、参数错误这些
// 都还没开始执行，仍然是普通 JSON + 正确的状态码，所以这里两种形态都要认。
async function applyStream(url, body, ctrl, onProgress) {
  const headers = { 'Content-Type': 'application/json' };
  if (state.token) headers['X-DBRover-Token'] = state.token;

  const res = await fetch(url, {
    method: 'POST',
    headers,
    body: JSON.stringify(body),
    signal: ctrl.signal,
  });

  const ctype = res.headers.get('Content-Type') || '';
  if (!ctype.includes('ndjson')) {
    const text = await res.text();
    let parsed = null;
    if (text) {
      try { parsed = JSON.parse(text); } catch { parsed = null; }
    }
    if (!res.ok) throw httpError(res.status, res.statusText, parsed);
    return parsed;
  }

  let result = null;
  let failure = null;
  await readNDJSON(res, (ev) => {
    if (ev.type === 'progress') {
      onProgress(ev.done, ev.total);
      return;
    }
    if (ev.type === 'result') {
      result = ev.body;
      return;
    }
    if (ev.type === 'error') {
      failure = httpError(ev.status || 409, '', { error: ev.error });
    }
  });
  if (failure) throw failure;
  if (!result) throw httpError(500, '', { error: { message: '进度流没有给出结果' } });
  return result;
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
    box.textContent = '还没有连接。先建一个连接：';
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
  state.columns = [];
  state.filter = [];
  state.order = null;
  state.undo = null;
  $('table-toolbar').hidden = true;
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

const ENGINES = [
  ['sqlite', 'SQLite'],
  ['mysql', 'MySQL'],
  ['pg', 'PostgreSQL'],
];

// applyEngineLayout 按引擎切换字段：SQLite 只有一个文件路径，
// 网络引擎才需要主机/端口/账号/库名。
function applyEngineLayout() {
  const engine = $('f-engine').value;
  const net = engine !== 'sqlite';
  $('f-net').hidden = !net;
  $('f-host-label').textContent = net ? '主机' : '数据库文件路径';
  $('f-host').placeholder = net ? '192.168.2.88' : 'demo.db';
  $('f-port').value = engine === 'mysql' ? '3306' : engine === 'pg' ? '5432' : '';
  if (net) $('f-catalog').value = $('f-catalog').value || 'postgres';
}

function newConnection() {
  const html =
    '<label class="field">名称</label><input id="f-name" style="width:100%" value="local">' +
    '<label class="field">引擎</label><select id="f-engine" style="width:100%">' +
    ENGINES.map(([v, t]) => '<option value="' + v + '">' + t + '</option>').join('') +
    '</select>' +
    '<label class="field" id="f-host-label">数据库文件路径</label>' +
    '<input id="f-host" style="width:100%" placeholder="demo.db" autocomplete="off" spellcheck="false">' +
    '<div id="f-net" hidden>' +
    '<label class="field">端口</label><input id="f-port" style="width:100%" autocomplete="off">' +
    '<label class="field">用户</label><input id="f-user" style="width:100%" autocomplete="off">' +
    '<label class="field">密码（只在本次请求里，不落盘、不回显）</label>' +
    '<input id="f-password" type="password" style="width:100%" autocomplete="off">' +
    '<label class="field">库名</label><input id="f-catalog" style="width:100%" autocomplete="off">' +
    '<label class="field">schema（可选，PostgreSQL 用，默认 public）</label>' +
    '<input id="f-schema" style="width:100%" autocomplete="off" placeholder="public">' +
    '</div>' +
    '<label class="field">环境</label><select id="f-env" style="width:100%">' +
    '<option value="local">local</option><option value="test">test</option>' +
    '<option value="staging">staging</option><option value="production">production</option>' +
    '</select>' +
    '<label class="field">访问模式</label><select id="f-mode" style="width:100%">' +
    '<option value="read-write">read-write</option><option value="read-only">read-only</option>' +
    '</select>';
  dialog('新建连接', html, '创建').then(async (ok) => {
    if (!ok) return;
    const engine = $('f-engine').value;
    const body = {
      name: $('f-name').value.trim() || 'local',
      engine,
      host: $('f-host').value.trim(),
      environment: $('f-env').value,
      access_mode: $('f-mode').value,
    };
    if (engine !== 'sqlite') {
      body.port = Number($('f-port').value) || 0;
      body.user = $('f-user').value.trim();
      body.password = $('f-password').value;
      body.catalog = $('f-catalog').value.trim();
      const schema = $('f-schema').value.trim();
      if (schema) body.schema = schema;
      // 连接前先自查必填项，别把「没填主机」变成一次服务端往返。
      const missing = [
        ['主机', body.host], ['端口', body.port], ['用户', body.user], ['库名', body.catalog],
      ].filter(([, v]) => !v).map(([k]) => k);
      if (missing.length) {
        showNotice('还差：' + missing.join('、'), 'warn');
        return;
      }
    }
    try {
      const created = await api(API.connections, { method: 'POST', body: JSON.stringify(body) });
      $('f-password').value = '';
      await loadConnections(created && created.id);
    } catch (e) {
      showNotice('创建失败：' + e.message, 'error');
    }
  });
  // 表单已经插入 DOM，绑定引擎切换。
  $('f-engine').onchange = applyEngineLayout;
  applyEngineLayout();
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
  state.columns = [];
  state.filter = [];
  state.order = null;
  state.view = 'data';
  // 换表就不该再提示上一张表的撤销入口（预案本身仍在服务端有效期内）。
  state.undo = null;
  $('sql').value = 'SELECT * FROM ' + name + ' LIMIT 100';
  await loadTablePage(true);
}

// ---------- 视图工具条：筛选与排序 ----------

const OPS = [
  ['eq', '='],
  ['ne', '≠'],
  ['gt', '>'],
  ['ge', '≥'],
  ['lt', '<'],
  ['le', '≤'],
  ['like', '匹配'],
  ['is_null', '为空'],
  ['is_not_null', '不为空'],
];

function opLabel(op) {
  const hit = OPS.find((x) => x[0] === op);
  return hit ? hit[1] : op;
}

// renderToolbar 重建工具条。列清单来自服务端返回的 columns，
// 因此「能筛什么列」永远等于「这张表有哪些列」，不在前端猜。
function renderToolbar() {
  const bar = $('table-toolbar');
  bar.replaceChildren();
  if (!state.tableName || !state.columns.length) {
    bar.hidden = true;
    return;
  }
  bar.hidden = false;

  const fill = (sel, items, pick) => {
    for (const it of items) {
      const o = document.createElement('option');
      o.value = it[0];
      o.textContent = it[1];
      sel.appendChild(o);
    }
    if (pick !== undefined) sel.value = pick;
  };
  const colPairs = state.columns.map((c) => [c.name, c.name]);

  const sortSel = document.createElement('select');
  sortSel.id = 'sort-col';
  fill(sortSel, [['', '默认顺序']].concat(colPairs), state.order ? state.order.column : '');
  sortSel.title = '排序依据';
  sortSel.onchange = () => {
    state.order = sortSel.value
      ? { column: sortSel.value, desc: state.order ? state.order.desc : false }
      : null;
    loadTablePage(true);
  };

  const dirBtn = document.createElement('button');
  dirBtn.id = 'sort-dir';
  dirBtn.textContent = state.order && state.order.desc ? '降序' : '升序';
  dirBtn.disabled = !state.order;
  dirBtn.onclick = () => {
    if (!state.order) return;
    state.order.desc = !state.order.desc;
    loadTablePage(true);
  };

  const fCol = document.createElement('select');
  fCol.id = 'filter-col';
  fill(fCol, colPairs);
  const fOp = document.createElement('select');
  fOp.id = 'filter-op';
  fill(fOp, OPS, 'eq');
  const fVal = document.createElement('input');
  fVal.id = 'filter-val';
  fVal.placeholder = '值';
  fVal.className = 'narrow';
  fOp.onchange = () => {
    fVal.hidden = fOp.value === 'is_null' || fOp.value === 'is_not_null';
  };
  const addBtn = document.createElement('button');
  addBtn.id = 'filter-add';
  addBtn.textContent = '添加条件';
  addBtn.onclick = () => {
    const op = fOp.value;
    const needVal = op !== 'is_null' && op !== 'is_not_null';
    state.filter.push({ column: fCol.value, op: op, value: needVal ? fVal.value : '' });
    loadTablePage(true);
  };

  const sizeSel = document.createElement('select');
  sizeSel.id = 'page-size';
  fill(sizeSel, [['100', '100 行'], ['200', '200 行'], ['500', '500 行'], ['1000', '1000 行']], String(state.limit));
  sizeSel.title = '每页行数';
  sizeSel.onchange = () => {
    state.limit = Number(sizeSel.value) || 100;
    loadTablePage(true);
  };

  bar.append(sortSel, dirBtn, fCol, fOp, fVal, addBtn, sizeSel);

  const schemaBtn = document.createElement('button');
  schemaBtn.id = 'btn-schema';
  schemaBtn.textContent = '结构';
  schemaBtn.title = '查看表结构并做结构变更';
  schemaBtn.onclick = () => showSchema();
  bar.appendChild(schemaBtn);

  const exportSel = document.createElement('select');
  exportSel.id = 'export-format';
  exportSel.title = '导出格式';
  fill(exportSel, [['csv', 'CSV'], ['ndjson', 'NDJSON']], state.exportFormat);
  exportSel.onchange = () => { state.exportFormat = exportSel.value; };
  const exportBtn = document.createElement('button');
  exportBtn.id = 'btn-export';
  exportBtn.textContent = '导出';
  exportBtn.title = '按当前筛选与排序导出整表（不受分页限制）';
  exportBtn.onclick = () => exportTable(exportSel.value);
  bar.append(exportSel, exportBtn);

  const bulkBtn = document.createElement('button');
  bulkBtn.id = 'btn-bulk';
  bulkBtn.textContent = '批量修改';
  bulkBtn.title = '按当前筛选条件一次改多行（不加载行）';
  bulkBtn.onclick = () => bulkEdit();
  bar.appendChild(bulkBtn);

  state.filter.forEach((f, i) => {
    const chip = document.createElement('span');
    chip.className = 'chip';
    chip.textContent = f.column + ' ' + opLabel(f.op) + (f.value ? ' ' + f.value : '');
    const x = document.createElement('button');
    x.textContent = '×';
    x.title = '移除该条件';
    x.onclick = () => {
      state.filter.splice(i, 1);
      loadTablePage(true);
    };
    chip.appendChild(x);
    bar.appendChild(chip);
  });
  if (state.filter.length) {
    const clear = document.createElement('button');
    clear.id = 'filter-clear';
    clear.textContent = '清空条件';
    clear.onclick = () => {
      state.filter = [];
      loadTablePage(true);
    };
    bar.appendChild(clear);
  }
}

// exportTable 走 fetch 而不是 <a href>：接口要 X-DBRover-Token 请求头，
// 普通链接带不上。代价是文件先落在浏览器内存里，
// 千万行级别的导出请用 CLI / curl 走流式。
async function exportTable(format) {
  if (!state.current || !state.tableName) return;
  let url = path(API.export, { id: state.current.id }) +
    '?table=' + encodeURIComponent(state.tableName) + '&format=' + encodeURIComponent(format);
  if (state.filter.length) {
    url += '&where=' + encodeURIComponent(JSON.stringify(state.filter));
  }
  if (state.order) {
    url += '&order=' + encodeURIComponent(JSON.stringify(state.order));
  }

  setStatus('导出中…');
  try {
    const headers = {};
    if (state.token) headers['X-DBRover-Token'] = state.token;
    const res = await fetch(url, { headers });
    if (!res.ok) {
      const text = await res.text();
      let msg = res.statusText;
      try {
        const e = JSON.parse(text);
        if (e && e.error && e.error.message) msg = e.error.message;
      } catch { /* 非 JSON 就用状态文本 */ }
      throw new Error(msg);
    }
    const blob = await res.blob();
    const text = await blob.text();
    const name = state.tableName + '.' + (format === 'ndjson' ? 'ndjson' : 'csv');

    // 先把内容确认下来再交给浏览器保存：万一宿主环境拦了下载，
    // 状态栏至少能证明「数据确实取回来了、多少字节、长什么样」。
    const head = (text.split('\n')[0] || '').slice(0, 80);

    const href = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = href;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(href), 10000);

    setStatus('已取回 ' + name + ' · ' + fmtSize(blob.size) + ' · 首行 ' + head);
  } catch (e) {
    setStatus('');
    showNotice('导出失败：' + e.message, 'error');
  }
}

function fmtSize(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}

async function loadTablePage(first) {
  if (!state.current) return;

  // 换视图会丢改动，先问一句。截断在提交前丢改动比让人以为还在更糟。
  if (first && state.pending.length) {
    const ok = await dialog('有未提交的修改',
      '<p>继续会丢弃这 ' + state.pending.length + ' 处修改。</p>', '丢弃并继续');
    if (!ok) return;
    state.pending = [];
    updatePendingBar();
  }

  try {
    let url = path(API.rows, { id: state.current.id, name: state.tableName }) + '?limit=' + state.limit;
    if (!first && state.tableNext) {
      url += '&after=' + encodeURIComponent(JSON.stringify(state.tableNext));
    }
    if (state.filter.length) {
      url += '&where=' + encodeURIComponent(JSON.stringify(state.filter));
    }
    if (state.order) {
      url += '&order=' + encodeURIComponent(JSON.stringify(state.order));
    }
    const out = await api(url);
    if (first) {
      $('results').replaceChildren();
      state.pending = [];
      state.editable = !!out.editable;
      state.pkCols = out.primary_key || [];
      state.columns = out.columns || [];
      updatePendingBar();
      renderToolbar();
      setStatus(state.tableName + ' · ' + out.rows.length + ' 行' +
        (state.editable ? '' : '（无主键，只读）'));
    }
    renderTable($('results'), out.columns, out.rows, !first, state.editable);
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

// ---------- 行级编辑 ----------

// coerce 把输入框里的字符串还原成列类型对应的值。
// 不还原的话，整数列会拿字符串去比较，定位条件永远匹配不到。
function coerce(kind, text) {
  if (text === '') return '';
  switch (kind) {
    case 'integer': {
      const n = Number(text);
      // BIGINT 超过 2^53 转成 Number 就丢低位了（9223372036854775807 → ...5808）。
      // 这种情况保持字符串，交给服务端按列类型还原。
      return Number.isSafeInteger(n) ? n : text;
    }
    case 'float': {
      const f = Number(text);
      return Number.isFinite(f) ? f : text;
    }
    default:
      return text; // decimal 保持字符串以免丢精度
  }
}

function cellKind(column) {
  const t = column && column.type ? column.type : {};
  return t.kind || '';
}

function updatePendingBar() {
  const bar = $('pending-bar');
  bar.replaceChildren();

  // 分批执行中，这条位置只讲一件事：进度到哪、怎么停。
  if (state.bulkRun) {
    bar.hidden = false;
    const run = state.bulkRun;
    const pct = run.total > 0 ? Math.floor((run.done / run.total) * 100) : 0;
    const text = document.createElement('span');
    text.textContent = run.total > 0
      ? '正在分批执行 · 已提交 ' + run.done + ' / ' + run.total + ' 行（' + pct + '%）'
      : '正在分批执行 · 已提交 ' + run.done + ' 行';
    const stop = document.createElement('button');
    stop.id = 'btn-stop-bulk';
    stop.textContent = '停止';
    stop.onclick = () => run.abort();
    bar.append(text, stop);
    return;
  }

  if (!state.pending.length) {
    // 没有待提交改动时，这条位置留给「刚才那次变更还能撤销」。
    // 撤销窗口 10 分钟，由服务端持有反向预案，这里只拿 id。
    if (state.undo) {
      bar.hidden = false;
      const text = document.createElement('span');
      text.textContent = '变更已应用 · 10 分钟内可撤销';
      const undoBtn = document.createElement('button');
      undoBtn.id = 'btn-undo';
      undoBtn.className = 'primary';
      undoBtn.textContent = '撤销';
      undoBtn.onclick = () => {
        const u = state.undo;
        state.undo = null;
        updatePendingBar();
        applyPlan(u.id, undefined, u.after);
      };
      const close = document.createElement('button');
      close.textContent = '知道了';
      close.onclick = () => {
        state.undo = null;
        updatePendingBar();
      };
      bar.append(text, undoBtn, close);
      return;
    }
    bar.hidden = true;
    return;
  }
  bar.hidden = false;
  const text = document.createElement('span');
  text.textContent = '待提交 ' + state.pending.length + ' 处修改';
  const submit = document.createElement('button');
  submit.className = 'primary';
  submit.textContent = '提交变更';
  submit.onclick = () => submitChanges();
  const discard = document.createElement('button');
  discard.textContent = '丢弃';
  discard.onclick = () => { state.pending = []; loadTablePage(true); };
  bar.appendChild(text);
  bar.appendChild(submit);
  bar.appendChild(discard);
}

async function submitChanges() {
  if (!state.pending.length || !state.current) return;

  // 同一行可能改了多列：按主键聚合成一条 update。
  const byRow = new Map();
  for (const ch of state.pending) {
    const k = JSON.stringify(ch.key);
    if (!byRow.has(k)) {
      byRow.set(k, { op: 'update', key: ch.key, before: { ...ch.key }, after: {} });
    }
    const row = byRow.get(k);
    row.before[ch.col] = ch.oldValue;
    row.after[ch.col] = ch.newValue;
  }

  let plan;
  try {
    plan = await api(path(API.changePlan, { id: state.current.id }), {
      method: 'POST',
      body: JSON.stringify({
        table: { kind: 'table', name: state.tableName },
        changes: [...byRow.values()],
      }),
    });
  } catch (e) {
    if (e.code === 'CAPABILITY_UNSUPPORTED') {
      showNotice('该表没有主键，无法安全定位行：' + e.message, 'warn');
    } else {
      showNotice('生成变更预案失败：' + e.message, 'error');
    }
    return;
  }

  const ok = await confirmPlan(plan);
  if (!ok) return;

  await applyPlan(plan.id);
}

// confirmPlan 把预案讲清楚再让人点执行。
// 三件事必须出现在弹窗里：会执行什么、能不能回来、影响多少行。
async function confirmPlan(plan) {
  const list = plan.statements || [];
  const html =
    '<div class="meta">' +
    '<span class="badge ' + String(plan.risk).toLowerCase() + '">风险 ' + plan.risk + '</span>' +
    '<span class="badge">打扰 ' + plan.friction + '</span>' +
    (plan.ddl ? '<span class="badge">结构变更</span>' : '') +
    (plan.irreversible ? '<span class="badge r4">不可逆</span>' : '') +
    '</div>' +
    '<p>将执行 ' + list.length + ' 条语句：</p>' +
    '<pre>' + escapeHTML(list.map((s) => s.sql).join(';\n')) + '</pre>' +
    (plan.affected_rows > 0 ? '<p>影响 ' + plan.affected_rows + ' 行。</p>' : '') +
    rollbackNote(plan);
  return dialog('确认变更', html, '执行');
}

// rollbackNote 说明「这次改动能不能回来」。
// 回滚语句存在 ≠ 数据能回来，这两件事必须分开讲。
function rollbackNote(plan) {
  const lost = plan.ddl && plan.irreversible ? '，且列里的数据会丢失' : '';
  if (plan.rolls_back_as_compensation) {
    return '<p class="warn">该引擎的 DDL 隐式提交，回滚语句只是补偿：已经发生的结构变化撤不回来' +
      lost + '。</p>';
  }
  if (plan.irreversible) {
    return '<p class="warn">该变更不可逆：回滚语句只能恢复结构，数据不会回来' + lost + '。</p>';
  }
  return '<p>已生成回滚语句，可在需要时手动执行。</p>';
}

async function applyPlan(pid, token, after, batched) {
  // after 决定应用完回到哪个视图：数据变更回数据页，结构变更回结构页。
  const refresh = after || (() => loadTablePage(true));
  const body = {};
  if (token) body.approval_token = token;
  const url = path(API.changeApply, { pid }) + (batched ? '?stream=1' : '');

  // 分批执行可能跑几十秒，必须能看见进度、也能停下来。
  const ctrl = batched ? new AbortController() : null;
  if (ctrl) {
    state.bulkRun = {
      done: 0,
      total: 0,
      abort: () => ctrl.abort(),
    };
    updatePendingBar();
  }

  let out;
  try {
    out = ctrl
      ? await applyStream(url, body, ctrl, (done, total) => {
        state.bulkRun = { done, total, abort: () => ctrl.abort() };
        updatePendingBar();
      })
      : await api(url, { method: 'POST', body: JSON.stringify(body) });
  } catch (e) {
    if (ctrl && e.name === 'AbortError') {
      // 中止不是失败：服务端在批与批之间停下，已提交的批撤不回来。
      showNotice('已停止。已提交的批不会回滚，预案仍然有效——再执行一次会把剩下的行改完。', 'warn');
      state.pending = [];
      state.bulkRun = null;
      updatePendingBar();
      await refresh();
      return;
    }
    state.bulkRun = null;
    if (e.code === 'POLICY_APPROVAL_REQUIRED') {
      updatePendingBar();
      const t = e.detail && e.detail.approval && e.detail.approval.token;
      if (!t) { showNotice('服务端未返回审批凭证', 'error'); return; }
      return applyPlan(pid, t, after, batched);
    }
    if (e.code === 'CHANGE_APPLY_ABORTED') {
      showNotice(e.message + (e.hint ? ' ' + e.hint : ''), 'warn');
      state.pending = [];
      updatePendingBar();
      await refresh();
      return;
    }
    if (e.code === 'SCHEMA_CHANGED') {
      showNotice('表结构已被改动，本次未执行。已重新读取结构，请再生成一次预案。', 'warn');
      await refresh();
      return;
    }
    if (e.code === 'CHANGE_ROW_CONFLICT') {
      showNotice('有行已被其他会话修改，该批未生效。已刷新数据，请重试。', 'warn');
      state.pending = [];
      updatePendingBar();
      await loadTablePage(true);
      return;
    }
    showNotice('应用变更失败：' + e.message, 'error');
    return;
  }

  state.bulkRun = null;
  showRisk(out.risk, out.friction);
  state.pending = [];
  // 服务端在应用成功后返回撤销预案；不可逆变更时是 null。
  state.undo = out.undo && out.undo.id
    ? { id: out.undo.id, expiresAt: out.undo.expires_at, after: refresh }
    : null;
  updatePendingBar();
  setStatus('变更已应用 · ' + out.duration_ms + ' ms' +
    (out.rows_affected > 0 ? ' · ' + out.rows_affected + ' 行' : ''));
  await refresh();
}

// ---------- 结构比对 ----------

// showCompare 比对两个已打开连接的结构。只读：不生成预案、不改任何一边。
function showCompare() {
  if (!state.conns.length) {
    showNotice('至少要有两个连接才能比对。', 'warn');
    return;
  }
  const opts = (pick) => state.conns
    .map((c) => '<option value="' + c.id + '"' + (c.id === pick ? ' selected' : '') + '>' +
      escapeHTML(c.name || c.id) + ' · ' + escapeHTML(c.engine) + '</option>')
    .join('');
  const html =
    '<label class="field">左侧（被改的一方）</label><select id="cmp-left" style="width:100%">' +
    opts(state.current && state.current.id) + '</select>' +
    '<label class="field">右侧（目标结构）</label><select id="cmp-right" style="width:100%">' +
    opts(state.conns.length > 1 ? state.conns[1].id : undefined) + '</select>';

  dialog('结构比对', html, '比对').then(async (ok) => {
    if (!ok) return;
    await runCompare($('cmp-left').value, $('cmp-right').value);
  });
}

async function runCompare(left, right) {
  try {
    const res = await api(API.compare, {
      method: 'POST',
      body: JSON.stringify({ left, right }),
    });
    renderCompare(res);
  } catch (e) {
    showNotice('比对失败：' + e.message, 'error');
  }
}

function renderCompare(res) {
  const wrap = document.createElement('div');
  wrap.className = 'schema';

  const summary = document.createElement('div');
  const tables = res.tables || [];
  summary.className = 'notice';
  summary.textContent = tables.length
    ? '有差异的表：' + tables.length + ' 张（左侧 ' + res.left_engine + ' / 右侧 ' + res.right_engine + '）'
    : '两侧结构一致（左侧 ' + res.left_engine + ' / 右侧 ' + res.right_engine + '）';
  wrap.appendChild(summary);

  if (tables.length) {
    const t = document.createElement('table');
    const thead = document.createElement('thead');
    const htr = document.createElement('tr');
    for (const label of ['表', '状态', '差异']) {
      const th = document.createElement('th');
      th.textContent = label;
      htr.appendChild(th);
    }
    thead.appendChild(htr);
    t.appendChild(thead);
    const tb = document.createElement('tbody');
    for (const td of tables) {
      const tr = document.createElement('tr');
      tr.appendChild(cell(td.name));
      tr.appendChild(cell(td.status));
      // 仅在单侧的列没有「原因」，写清楚它是缺了还是多了，别留一个空冒号。
      // 索引与列分开列：一个是数据形状，一个是性能设施，混在一起看不出该改哪边。
      const colLines = (td.columns || []).map((c) => {
        if (c.status === 'only_left') return c.name + '：仅在左侧';
        if (c.status === 'only_right') return c.name + '：仅在右侧';
        return c.name + '：' + (c.reason || []).join('；');
      });
      const idxLines = (td.indexes || []).map((x) => {
        const label = x.right || x.left;
        const name = label ? label.name : x.key;
        if (x.status === 'only_left') return '索引 ' + name + '：仅在左侧';
        if (x.status === 'only_right') return '索引 ' + name + '：仅在右侧';
        return '索引 ' + name + '：' + (x.reason || []).join('；');
      });
      tr.appendChild(cell(colLines.concat(idxLines).join('\n')));
      tb.appendChild(tr);
    }
    t.appendChild(tb);
    wrap.appendChild(t);
  }

  const addPre = (title, lines) => {
    if (!lines || !lines.length) return;
    const h = document.createElement('div');
    h.className = 'head';
    h.textContent = title;
    wrap.appendChild(h);
    const pre = document.createElement('pre');
    pre.className = 'preview';
    pre.textContent = lines.join('\n');
    wrap.appendChild(pre);
  };
  addPre('升级脚本（左侧 → 右侧）', res.upgrade);
  addPre('回退脚本（右侧 → 左侧）', res.downgrade);

  if ((res.need_review || []).length) {
    const h = document.createElement('div');
    h.className = 'head';
    h.textContent = '需要人工决策';
    wrap.appendChild(h);
    const ul = document.createElement('ul');
    for (const n of res.need_review) {
      const li = document.createElement('li');
      li.textContent = n;
      ul.appendChild(li);
    }
    wrap.appendChild(ul);
  }

  $('results').replaceChildren(wrap);
  setStatus('比对完成');
}

// ---------- 条件批量修改 ----------

// bulkEdit 是「按条件改多行」的入口。它与行内编辑的区别必须让用户看见：
// 行内编辑改的是「我看着的这几行」，批量修改改的是「符合条件的全部行」。
function bulkEdit() {
  if (!state.current || !state.tableName) return;
  const pkSet = new Set((state.pkCols || []).map((c) => c.toLowerCase()));
  const cols = (state.columns || []).filter((c) => !pkSet.has(c.name.toLowerCase()));
  if (!cols.length) {
    showNotice('这张表没有可批量修改的非主键列。', 'warn');
    return;
  }

  const noFilter = state.filter.length === 0;
  const html =
    '<label class="field">列</label><select id="bk-col" style="width:100%">' +
    cols.map((c) => '<option value="' + c.name + '">' + c.name + '</option>').join('') +
    '</select>' +
    '<label class="field">新值</label><input id="bk-val" style="width:100%" autocomplete="off">' +
    (noFilter
      ? '<p class="warn">当前没有筛选条件：这会命中<strong>全表</strong>。要确认请回填表名。</p>' +
        '<label class="field">回填表名</label><input id="bk-confirm" style="width:100%" autocomplete="off">'
      : '<p>只影响当前筛选命中的行（' + state.filter.length + ' 个条件）。</p>');

  dialog('批量修改', html, '生成预案').then(async (ok) => {
    if (!ok) return;
    const body = {
      table: { kind: 'table', name: state.tableName },
      set: {},
      filter: state.filter,
    };
    const col = $('bk-col').value;
    const raw = $('bk-val').value;
    body.set[col] = coerce(cellKind(cols.find((c) => c.name === col)), raw);
    if (noFilter) {
      const typed = $('bk-confirm').value.trim();
      if (typed !== state.tableName) {
        showNotice('表名不一致，已取消。', 'warn');
        return;
      }
      body.confirm_table = typed;
    }
    await submitBulk(body);
  });
}

async function submitBulk(body) {
  let res;
  try {
    res = await api(path(API.bulkPlan, { id: state.current.id }), {
      method: 'POST',
      body: JSON.stringify(body),
    });
  } catch (e) {
    if (e.code === 'BULK_NO_ROLLBACK_ACK') {
      // 影响行数过大时不生成回滚，必须再确认一次。
      const ok = await dialog('这批不生成回滚',
        '<p>' + escapeHTML(e.message) + '</p><p>确认已备份这些行，继续吗？</p>', '继续');
      if (!ok) return;
      body.accept_no_rollback = true;
      return submitBulk(body);
    }
    showNotice('生成批量预案失败：' + e.message, 'error');
    return;
  }

  const plan = res.plan || {};
  const sample = (res.samples || []).slice(0, 5);
  const stmts = (plan.statements || []).map((s) => s.sql).join(';\n');

  const sampleHTML = sample.length
    ? '<p>前后对比（最多 5 行）：</p><pre>' + escapeHTML(sample.map((s) =>
        JSON.stringify(s.key) + '  ' + JSON.stringify(s.before) + ' → ' + JSON.stringify(s.after),
      ).join('\n')) + '</pre>'
    : '';
  const noteHTML = res.note ? '<p class="warn">' + escapeHTML(res.note) + '</p>' : '';

  const ok = await dialog('确认批量修改',
    '<div class="meta">' +
      '<span class="badge ' + String(plan.risk).toLowerCase() + '">风险 ' + plan.risk + '</span>' +
      '<span class="badge">打扰 ' + plan.friction + '</span>' +
      '<span class="badge">影响 ' + res.affected + ' 行</span>' +
      (res.batched ? '<span class="badge">分 ' + res.batches + ' 批</span>' : '') +
      (plan.irreversible ? '<span class="badge r4">不可逆</span>' : '') +
    '</div>' +
    '<p>将执行 ' + (plan.statements || []).length + ' 条语句：</p>' +
    '<pre>' + escapeHTML(stmts) + '</pre>' +
    sampleHTML + noteHTML, '执行');
  if (!ok) return;

  await applyPlan(plan.id, undefined, undefined, !!res.batched);
}

// ---------- 建表 ----------

const CREATE_ROW = () =>
  '<div class="col-row">' +
  '<input class="col-name" placeholder="列名" autocomplete="off" spellcheck="false">' +
  '<input class="col-type" placeholder="类型" list="sch-types" autocomplete="off" spellcheck="false">' +
  '<label class="inline"><input type="checkbox" class="col-nullable" checked> 可空</label>' +
  '<input class="col-default" placeholder="默认值" autocomplete="off" spellcheck="false">' +
  '<button class="col-del" title="移除这一列">×</button>' +
  '</div>';

function showCreateTable() {
  if (!state.current) {
    showNotice('请先建立连接。', 'warn');
    return;
  }
  state.view = 'create';
  state.tableName = '';
  state.columns = [];
  state.filter = [];
  state.order = null;
  state.undo = null;
  $('table-toolbar').hidden = true;
  updatePendingBar();

  const wrap = document.createElement('div');
  wrap.className = 'schema';
  wrap.innerHTML =
    '<div class="toolbar">' +
    '<input id="ct-name" class="narrow" placeholder="表名" autocomplete="off" spellcheck="false">' +
    '<input id="ct-pk" class="narrow" placeholder="主键（逗号分隔）" autocomplete="off" spellcheck="false">' +
    '<button id="ct-addcol">添加列</button>' +
    '<button id="ct-submit" class="primary">建表</button>' +
    '</div>' +
    '<datalist id="sch-types">' + TYPES.map((t) => '<option value="' + t + '"></option>').join('') + '</datalist>' +
    '<div id="ct-cols"></div>' +
    '<pre id="ct-preview" class="preview">填表名、列与主键后显示将执行的语句。</pre>';

  $('results').replaceChildren(wrap);
  addCreateRow();
  addCreateRow();

  $('ct-addcol').onclick = () => addCreateRow();
  $('ct-submit').onclick = () => submitCreateTable();
  for (const id of ['ct-name', 'ct-pk']) {
    $(id).oninput = () => scheduleCreatePreview();
  }
  setStatus('新建表 · 先填列，再指定主键');
}

function addCreateRow() {
  const box = $('ct-cols');
  if (!box) return;
  const holder = document.createElement('div');
  holder.innerHTML = CREATE_ROW();
  const row = holder.firstElementChild;
  row.querySelector('.col-del').onclick = () => {
    row.remove();
    scheduleCreatePreview();
  };
  for (const sel of ['.col-name', '.col-type', '.col-default', '.col-nullable']) {
    row.querySelector(sel).oninput = () => scheduleCreatePreview();
  }
  box.appendChild(row);
  scheduleCreatePreview();
}

let createTimer = null;

function scheduleCreatePreview() {
  clearTimeout(createTimer);
  createTimer = setTimeout(previewCreateTable, 250);
}

// collectCreate 把表单读成请求体。DOM 顺序即列顺序。
function collectCreate() {
  const body = {
    table: { kind: 'table', name: $('ct-name').value.trim() },
    action: 'create_table',
    columns: [],
    primary_key: $('ct-pk').value.split(/[,，\s]+/).filter(Boolean),
  };
  const rows = $('ct-cols').querySelectorAll('.col-row');
  for (const row of rows) {
    const name = row.querySelector('.col-name').value.trim();
    const type = row.querySelector('.col-type').value.trim();
    if (!name && !type) continue; // 空行直接忽略，别逼用户删
    const col = { name, type, nullable: row.querySelector('.col-nullable').checked };
    const def = row.querySelector('.col-default').value.trim();
    if (def) col.default = def;
    body.columns.push(col);
  }
  return body;
}

async function previewCreateTable() {
  const box = $('ct-preview');
  if (!box) return;
  const body = collectCreate();
  if (!body.table.name || !body.columns.length) {
    box.textContent = '填表名、列与主键后显示将执行的语句。';
    return;
  }
  try {
    const plan = await api(path(API.schemaPlan, { id: state.current.id }) + '?preview=1', {
      method: 'POST',
      body: JSON.stringify(body),
    });
    box.textContent = (plan.statements || []).map((s) => s.sql).join(';\n');
  } catch (e) {
    box.textContent = '参数不合法：' + e.message;
  }
}

async function submitCreateTable() {
  const body = collectCreate();
  if (!body.table.name) { showNotice('表名不能为空。', 'warn'); return; }
  if (!body.columns.length) { showNotice('至少要有一列。', 'warn'); return; }
  if (!body.primary_key.length) {
    showNotice('必须指定主键：无主键的新表在本工具里既不能浏览也不能编辑。', 'warn');
    return;
  }

  let plan;
  try {
    plan = await api(path(API.schemaPlan, { id: state.current.id }), {
      method: 'POST',
      body: JSON.stringify(body),
    });
  } catch (e) {
    showNotice('生成建表预案失败：' + e.message, 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;

  const created = body.table.name;
  await applyPlan(plan.id, undefined, async () => {
    await loadTables();
    state.tableName = created;
    state.view = 'schema';
    await refreshSchema();
  });
}

// ---------- 结构 ----------

// TYPES 是常见类型的快捷项。类型仍可手填：白名单在服务端，
// 前端给列表只是省打字，不是权威。
const TYPES = [
  'TEXT', 'INTEGER', 'BIGINT', 'BOOLEAN', 'NUMERIC(10,2)',
  'VARCHAR(64)', 'TIMESTAMP', 'DATE', 'JSON',
];

async function showSchema() {
  if (!state.current || !state.tableName) return;
  state.view = 'schema';
  await refreshSchema();
}

async function refreshSchema() {
  try {
    const out = await api(path(API.schema, { id: state.current.id, name: state.tableName }));
    state.schema = out;
    renderSchema(out);
  } catch (e) {
    if (e.code === 'OBJECT_NOT_FOUND') {
      // 表刚被删掉（例如撤销了一次建表）：回清单，不要对着消失的表报错。
      state.tableName = '';
      $('table-toolbar').hidden = true;
      const hint = document.createElement('div');
      hint.className = 'notice';
      hint.textContent = '这张表已经不存在了，已回到对象清单。';
      $('results').replaceChildren(hint);
      setStatus('');
      await loadTables();
      return;
    }
    showNotice('读取结构失败：' + e.message, 'error');
  }
}

function renderSchema(out) {
  const cols = out.columns || [];
  const pk = out.primary_key || [];

  const wrap = document.createElement('div');
  wrap.className = 'schema';

  // 加列表单：改动即预览，避免「填完点保存才发现要执行什么」。
  const form = document.createElement('div');
  form.className = 'toolbar';
  form.innerHTML =
    '<input id="sch-name" class="narrow" placeholder="列名" autocomplete="off" spellcheck="false">' +
    '<input id="sch-type" class="narrow" placeholder="类型" list="sch-types" autocomplete="off" spellcheck="false">' +
    '<datalist id="sch-types">' + TYPES.map((t) => '<option value="' + t + '"></option>').join('') + '</datalist>' +
    '<label class="inline"><input type="checkbox" id="sch-nullable" checked> 可空</label>' +
    '<input id="sch-default" class="narrow" placeholder="默认值（字面量）" autocomplete="off" spellcheck="false">' +
    '<button id="sch-add" class="primary">加列</button>';
  wrap.appendChild(form);

  const preview = document.createElement('pre');
  preview.id = 'ddl-preview';
  preview.className = 'preview';
  preview.textContent = '填列名与类型后显示将执行的语句。';
  wrap.appendChild(preview);

  const table = document.createElement('table');
  const thead = document.createElement('thead');
  const htr = document.createElement('tr');
  for (const label of ['列', '类型', '可空', '默认', '主键', '']) {
    const th = document.createElement('th');
    th.textContent = label;
    htr.appendChild(th);
  }
  thead.appendChild(htr);
  table.appendChild(thead);

  const tbody = document.createElement('tbody');
  for (const c of cols) {
    const tr = document.createElement('tr');
    tr.appendChild(cell(c.name));
    tr.appendChild(cell(c.type && c.type.raw ? c.type.raw : String(c.type.kind)));
    tr.appendChild(cell(c.nullable ? '是' : '否'));
    tr.appendChild(cell(c.default !== undefined && c.default !== null ? c.default : '—'));
    tr.appendChild(cell(c.is_primary ? '✓' : ''));
    const actions = document.createElement('td');
    if (!c.is_primary) {
      const drop = document.createElement('button');
      drop.textContent = '删列';
      drop.onclick = () => planDropColumn(c.name);
      actions.appendChild(drop);
    }
    // 改类型：主键列也允许（类型与「是不是主键」无关）。
    const retype = document.createElement('button');
    retype.textContent = '改类型';
    retype.onclick = () => startRetype(tr, c);
    actions.appendChild(retype);
    tr.appendChild(actions);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);

  // 索引区
  const ixHead = document.createElement('div');
  ixHead.className = 'head';
  ixHead.textContent = '索引';
  wrap.appendChild(ixHead);

  const ixForm = document.createElement('div');
  ixForm.className = 'toolbar';
  ixForm.innerHTML =
    '<input id="ix-name" class="narrow" placeholder="索引名" autocomplete="off" spellcheck="false">' +
    '<input id="ix-cols" class="narrow" placeholder="列（逗号分隔）" autocomplete="off" spellcheck="false">' +
    '<label class="inline"><input type="checkbox" id="ix-unique"> 唯一</label>' +
    '<button id="ix-add" class="primary">建索引</button>';
  wrap.appendChild(ixForm);

  const idxs = out.indexes || [];
  if (!idxs.length) {
    const none = document.createElement('div');
    none.className = 'notice';
    none.textContent = '这个表还没有索引（主键索引除外）。';
    wrap.appendChild(none);
  } else {
    const it = document.createElement('table');
    const ith = document.createElement('thead');
    const ihtr = document.createElement('tr');
    for (const label of ['名称', '列', '唯一', '主键', '']) {
      const th = document.createElement('th');
      th.textContent = label;
      ihtr.appendChild(th);
    }
    ith.appendChild(ihtr);
    it.appendChild(ith);
    const itb = document.createElement('tbody');
    for (const ix of idxs) {
      const tr = document.createElement('tr');
      tr.appendChild(cell(ix.name));
      tr.appendChild(cell((ix.columns || []).join(', ')));
      tr.appendChild(cell(ix.unique ? '是' : '否'));
      tr.appendChild(cell(ix.primary ? '✓' : ''));
      const act = document.createElement('td');
      if (!ix.primary) {
        const del = document.createElement('button');
        del.textContent = '删索引';
        del.onclick = () => planDropIndex(ix.name);
        act.appendChild(del);
      }
      tr.appendChild(act);
      itb.appendChild(tr);
    }
    it.appendChild(itb);
    wrap.appendChild(it);
  }

  if (out.ddl) {
    const ddl = document.createElement('pre');
    ddl.className = 'preview';
    ddl.textContent = out.ddl;
    wrap.appendChild(ddl);
  }

  $('results').replaceChildren(wrap);

  const onInput = () => schedulePreview();
  $('sch-name').oninput = onInput;
  $('sch-type').oninput = onInput;
  $('sch-default').oninput = onInput;
  $('sch-nullable').onchange = onInput;
  $('sch-add').onclick = () => planAddColumn();
  $('ix-add').onclick = () => planCreateIndex();
  setStatus(state.tableName + ' · ' + cols.length + ' 列' +
    (pk.length ? '（主键 ' + pk.join(', ') + '）' : '（无主键）'));
}

let previewTimer = null;

function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(previewAddColumn, 250);
}

function columnInput() {
  const column = {
    name: $('sch-name').value.trim(),
    type: $('sch-type').value.trim(),
    nullable: $('sch-nullable').checked,
  };
  const def = $('sch-default').value.trim();
  if (def) column.default = def;
  return column;
}

// postSchemaPlan 是结构预案的统一出口：不同动作带的字段不同
// （column / columns+primary_key / index），由调用方拼好再发。
async function postSchemaPlan(body, preview) {
  const url = path(API.schemaPlan, { id: state.current.id }) + (preview ? '?preview=1' : '');
  return api(url, {
    method: 'POST',
    body: JSON.stringify(Object.assign({ table: { kind: 'table', name: state.tableName } }, body)),
  });
}

async function fetchSchemaPlan(action, column, preview) {
  return postSchemaPlan({ action, column }, preview);
}

async function fetchIndexPlan(action, index, preview) {
  return postSchemaPlan({ action, index }, preview);
}

async function previewAddColumn() {
  const box = $('ddl-preview');
  if (!box) return;
  const column = columnInput();
  if (!column.name || !column.type) {
    box.textContent = '填列名与类型后显示将执行的语句。';
    return;
  }
  try {
    const plan = await fetchSchemaPlan('add_column', column, true);
    box.textContent = (plan.statements || []).map((s) => s.sql).join(';\n');
  } catch (e) {
    box.textContent = '参数不合法：' + e.message;
  }
}

async function planAddColumn() {
  const column = columnInput();
  if (!column.name || !column.type) {
    showNotice('列名与类型都要填。', 'warn');
    return;
  }
  let plan;
  try {
    plan = await fetchSchemaPlan('add_column', column, false);
  } catch (e) {
    showNotice('生成结构预案失败：' + e.message, 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;
  await applyPlan(plan.id, undefined, refreshSchema);
}

async function planDropColumn(name) {
  let plan;
  try {
    plan = await fetchSchemaPlan('drop_column', { name }, false);
  } catch (e) {
    showNotice('生成结构预案失败：' + e.message, 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;
  await applyPlan(plan.id, undefined, refreshSchema);
}

// ---------- 索引 ----------

async function planCreateIndex() {
  const name = $('ix-name').value.trim();
  const cols = $('ix-cols').value.split(/[,，\s]+/).filter(Boolean);
  if (!name) {
    showNotice('索引名不能为空。', 'warn');
    return;
  }
  if (!cols.length) {
    showNotice('索引至少要有一列。', 'warn');
    return;
  }
  let plan;
  try {
    plan = await fetchIndexPlan('create_index', {
      name, columns: cols, unique: $('ix-unique').checked,
    }, false);
  } catch (e) {
    // 唯一索引撞重复值时会带样本，原文展示比换成笼统提示有用。
    showNotice(e.message + (e.hint ? '（' + e.hint + '）' : ''), 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;
  await applyPlan(plan.id, undefined, refreshSchema);
}

async function planDropIndex(name) {
  let plan;
  try {
    plan = await fetchIndexPlan('drop_index', { name }, false);
  } catch (e) {
    showNotice('生成结构预案失败：' + e.message, 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;
  await applyPlan(plan.id, undefined, refreshSchema);
}

// ---------- 改列类型 ----------

// startRetype 把该行的类型单元格换成输入框，让用户就地改。
// 预检与风险分级都在服务端，这里只负责把意图表达清楚。
function startRetype(tr, col) {
  const cells = tr.children;
  const oldType = col.type && col.type.raw ? col.type.raw : String(col.type.kind);
  const typeCell = cells[1];

  const input = document.createElement('input');
  input.className = 'narrow';
  input.value = oldType;
  input.setAttribute('list', 'sch-types');
  typeCell.replaceChildren(input);
  input.focus();
  input.select();

  const actions = tr.lastElementChild;
  actions.replaceChildren();
  const ok = document.createElement('button');
  ok.className = 'primary';
  ok.textContent = '应用';
  const cancel = document.createElement('button');
  cancel.textContent = '取消';

  ok.onclick = () => planRetype(col.name, input.value.trim());
  cancel.onclick = () => refreshSchema();
  actions.append(ok, cancel);

  input.onkeydown = (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      planRetype(col.name, input.value.trim());
    }
    if (ev.key === 'Escape') refreshSchema();
  };
}

async function planRetype(name, newType) {
  if (!newType) {
    showNotice('新类型不能为空。', 'warn');
    return;
  }
  let plan;
  try {
    plan = await fetchSchemaPlan('alter_column_type', { name, type: newType }, false);
  } catch (e) {
    // 收窄预检不通过时服务端会带越界样本，原文展示，别换成笼统的话。
    showNotice(e.message + (e.hint ? '（' + e.hint + '）' : ''), 'error');
    return;
  }
  if (!(await confirmPlan(plan))) return;
  await applyPlan(plan.id, undefined, refreshSchema);
}

// ---------- 渲染 ----------

function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

// renderTable 渲染结果网格。
// allowEdit 必须由调用方显式给：表浏览可以编辑，而原始 SQL 的结果集不行——
// 后者的列可能来自别的表，拿它拼 UPDATE 会改错表。
function renderTable(container, columns, rows, append, allowEdit) {
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

    // 行定位键：只取主键列的值。没有主键就不可编辑。
    let key = null;
    const editing = !!(allowEdit && state.pkCols && state.pkCols.length);
    if (editing) {
      key = {};
      for (const pk of state.pkCols) {
        const idx = columns.findIndex((c) => c.name.toLowerCase() === pk.toLowerCase());
        if (idx >= 0) key[pk] = r[idx];
      }
      if (Object.keys(key).length !== state.pkCols.length) key = null;
      else tr.dataset.key = JSON.stringify(key);
    }

    for (let i = 0; i < columns.length; i++) {
      const v = r[i];
      if (v === null || v === undefined) {
        // NULL 与空串是两件事：NULL 显示为 NULL，空串显示为 ''。
        // NULL 单元格允许直接输入来取消 NULL，但「清空输入框」不会把它变成空串。
        const td = cell('NULL', 'null');
        attachEdit(td, key, columns[i], null, true);
        tr.appendChild(td);
      } else if (v === '') {
        const td = cell("''", 'empty');
        attachEdit(td, key, columns[i], '');
        tr.appendChild(td);
      } else {
        const td = cell(String(v));
        attachEdit(td, key, columns[i], v);
        tr.appendChild(td);
      }
    }
    tbody.appendChild(tr);
  }
}

// attachEdit 让单元格可编辑，并在失焦时把改动记进待提交列表。
// isNull 为 true 时额外记住「原值是 NULL」，用于生成 before=null。
function attachEdit(td, key, column, origValue, isNull) {
  if (!key) return;
  td.contentEditable = 'true';
  td.dataset.col = column.name;
  td.dataset.kind = cellKind(column);
  td.dataset.orig = isNull ? '' : String(origValue);
  if (isNull) td.dataset.orignull = '1';
  td.classList.add('editable');
}

// 事件委托：只挂一次，避免每格一个监听器。
document.addEventListener('focusout', (ev) => {
  const td = ev.target;
  if (!td || td.tagName !== 'TD' || !td.isContentEditable) return;

  const tr = td.closest('tr');
  if (!tr || !tr.dataset.key) return;

  // 右键菜单已经把改动记下了，别再按输入框内容重算一遍。
  if (td.dataset.skip === '1') {
    delete td.dataset.skip;
    return;
  }

  const col = td.dataset.col;
  const kind = td.dataset.kind || '';
  const origText = td.dataset.orig || '';
  const wasNull = td.dataset.orignull === '1';
  const newText = td.textContent;

  // 原值是 NULL：清空输入框或原样留 NULL 都视为「没改」，不会被误解成空串。
  if (wasNull && (newText === '' || newText === 'NULL')) {
    td.textContent = 'NULL';
    td.classList.remove('dirty');
    dropPending(col, tr.dataset.key);
    updatePendingBar();
    return;
  }
  if (!wasNull && newText === origText) {
    td.classList.remove('dirty');
    return;
  }

  const key = JSON.parse(tr.dataset.key);
  state.pending = state.pending.filter((c) => !(c.col === col && JSON.stringify(c.key) === JSON.stringify(key)));
  state.pending.push({
    key,
    col,
    oldValue: wasNull ? null : coerce(kind, origText),
    newValue: coerce(kind, newText),
  });
  td.classList.add('dirty');
  updatePendingBar();
});

// dropPending 丢掉某一格已有的待提交改动。
function dropPending(col, keyText) {
  state.pending = state.pending.filter((c) => !(c.col === col && JSON.stringify(c.key) === keyText));
}

// ---------- NULL 与空串的显式选择 ----------

// 右键菜单：把「设为 NULL」和「设为空串」做成两个动作。
// 靠「输入框留空」猜意图必然出错——这两件事在数据库里完全不同。
let cellMenu = null;

function closeCellMenu() {
  if (cellMenu) {
    cellMenu.remove();
    cellMenu = null;
  }
}

document.addEventListener('contextmenu', (ev) => {
  const td = ev.target;
  if (!td || td.tagName !== 'TD') return;
  const tr = td.closest('tr');
  if (!tr || !tr.dataset.key || !td.dataset.col) return;

  ev.preventDefault();
  closeCellMenu();

  const menu = document.createElement('div');
  menu.className = 'cell-menu';
  menu.style.left = ev.clientX + 'px';
  menu.style.top = ev.clientY + 'px';
  const actions = [
    ['设为 NULL', null],
    ["设为空串 ''", ''],
  ];
  for (const [label, value] of actions) {
    const b = document.createElement('button');
    b.textContent = label;
    b.onclick = () => {
      closeCellMenu();
      setCellExact(td, value);
    };
    menu.appendChild(b);
  }
  document.body.appendChild(menu);
  cellMenu = menu;
});

document.addEventListener('click', closeCellMenu);
document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape') closeCellMenu();
});

function sameValue(a, b) {
  if (a === null || b === null) return a === b;
  return String(a) === String(b);
}

// setCellExact 把单元格直接设为指定值（null 或空串），并入待提交列表。
function setCellExact(td, value) {
  const tr = td.closest('tr');
  if (!tr || !tr.dataset.key) return;

  const col = td.dataset.col;
  const kind = td.dataset.kind || '';
  const wasNull = td.dataset.orignull === '1';
  const oldValue = wasNull ? null : coerce(kind, td.dataset.orig || '');
  if (sameValue(oldValue, value)) return;

  td.textContent = value === null ? 'NULL' : "''";
  td.dataset.orignull = value === null ? '1' : '0';
  td.classList.add('dirty');
  // 这次改动的 before/after 已经算好，别让紧随其后的失焦再记一遍。
  td.dataset.skip = '1';

  const key = JSON.parse(tr.dataset.key);
  dropPending(col, tr.dataset.key);
  state.pending.push({ key, col, oldValue, newValue: value });
  updatePendingBar();
}

// 关闭页面前拦一下：浏览器只允许通用文案，不能自定义内容。
window.addEventListener('beforeunload', (ev) => {
  if (!state.pending.length) return;
  ev.preventDefault();
  ev.returnValue = '';
});

function renderExec(out) {
  $('results').replaceChildren();
  for (const r of out.results || []) {
    if (r.err) {
      showNotice('第 ' + r.index + ' 条失败：' + r.err, 'error');
      return;
    }
    if (r.columns && r.columns.length) {
      // 原始 SQL 的结果集不给编辑：列可能来自别的表，拼出来的 UPDATE 会打错表。
      renderTable($('results'), r.columns, r.rows || [], false, false);
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
$('btn-new-conn').onclick = () => newConnection();
$('btn-compare').onclick = () => showCompare();
$('btn-new-table').onclick = () => showCreateTable();
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
