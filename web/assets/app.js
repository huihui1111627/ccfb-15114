'use strict';

const $ = (id) => document.getElementById(id);
let META = null;

const PRESET = [
  { name: '赛区', expr: 'zone', kind: 'int', dir: 'asc', nulls: 'first', pinyin: false },
  { name: '分速', expr: 'distance * 1000 / duration', kind: 'float', dir: 'desc', nulls: 'last', pinyin: false },
  { name: '归巢时刻', expr: 'arrive', kind: 'string', dir: 'asc', nulls: 'last', pinyin: false },
  { name: '鸽舍名', expr: 'loft', kind: 'string', dir: 'asc', nulls: 'last', pinyin: true },
];

function colRow(c, i) {
  const el = document.createElement('div');
  el.className = 'col';
  el.innerHTML = `
    <span class="idx">#${i + 1}</span>
    <input type="text" class="expr" value="${escapeAttr(c.expr)}" spellcheck="false" title="表达式">
    <select class="kind">
      <option>int</option><option>uint</option><option>float</option><option>string</option><option>bool</option><option>any</option>
    </select>
    <select class="dir"><option value="asc">升序 ASC</option><option value="desc">降序 DESC</option></select>
    <select class="nulls"><option value="first">NULL 在前</option><option value="last">NULL 在后</option></select>
    <label class="chk" title="按拼音/声调排序"><input type="checkbox" class="pinyin"> 拼音</label>
    <button class="del" type="button" title="删除">×</button>`;
  el.querySelector('.kind').value = c.kind;
  el.querySelector('.dir').value = c.dir;
  el.querySelector('.nulls').value = c.nulls;
  el.querySelector('.pinyin').checked = !!c.pinyin;
  el.querySelector('.del').onclick = () => { el.remove(); };
  return el;
}

function addColumn(c) { $('columns').appendChild(colRow(c, $('columns').children.length)); }

function readColumns() {
  return [...$('columns').children].map((el, i) => ({
    name: `c${i + 1}`,
    expr: el.querySelector('.expr').value,
    kind: el.querySelector('.kind').value,
    dir: el.querySelector('.dir').value,
    nulls: el.querySelector('.nulls').value,
    pinyin: el.querySelector('.pinyin').checked,
  }));
}

function escapeAttr(s) { return String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;'); }
function escapeHtml(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

async function loadMeta() {
  const res = await fetch('/api/meta');
  META = await res.json();
  $('meta').innerHTML = `${META.goVersion}<br>GOMAXPROCS ${META.gomaxprocs}`;
}

async function run() {
  const btn = $('run');
  btn.disabled = true;
  btn.textContent = '排序中…';
  $('error').hidden = true;
  const body = {
    columns: readColumns(),
    rows: +$('rows').value,
    seed: +$('seed').value,
    topK: +$('topK').value,
    workers: +$('workers').value,
    sample: 8,
    compare: $('doCompare').checked,
  };
  try {
    const res = await fetch('/api/sort', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    const data = await res.json();
    if (!data.ok) {
      $('error').hidden = false;
      $('error').textContent = data.error || '未知错误';
      return;
    }
    render(data);
  } catch (e) {
    $('error').hidden = false;
    $('error').textContent = String(e);
  } finally {
    btn.disabled = false;
    btn.textContent = '生成数据并排序';
  }
}

function ms(ns) {
  if (ns >= 1e9) return (ns / 1e9).toFixed(2) + ' s';
  if (ns >= 1e6) return (ns / 1e6).toFixed(1) + ' ms';
  return (ns / 1e3).toFixed(0) + ' µs';
}

function render(d) {
  renderSamples(d);
  renderPhases(d);
  renderCompare(d);
  renderColumns(d);
  renderOrdering(d);
}

// Colour a hex string: 00/ff runs are markers, the rest is value bytes.
function hexHtml(hex, kind) {
  if (!hex) return '<span class="empty">空</span>';
  const marks = new Set(['00', 'ff', '01', 'fe', '0000', 'ffff']);
  let out = '';
  for (let i = 0; i < hex.length; i += 2) {
    const b = hex.slice(i, i + 2);
    let cls = '';
    if (kind === 'string') cls = 'hex-str';
    if (marks.has(b) && kind !== 'string') cls = 'hex-null';
    if (kind === 'string' && (b === '00' || b === 'ff')) cls = 'hex-null';
    out += `<span class="${cls}">${b}</span> `;
  }
  return out;
}

function renderSamples(d) {
  const host = $('samples');
  host.innerHTML = '';
  $('keyHint').textContent = `平均键长 ${d.keyLen.toFixed(1)} 字节 · 共 ${fmtInt(d.stats.keyBytes)} 字节`;
  if (!d.samples || !d.samples.length) { host.innerHTML = '<div class="empty">没有样例</div>'; return; }
  d.samples.forEach((s) => {
    const el = document.createElement('div');
    el.className = 'sample';
    let cols = '';
    s.columns.forEach((c) => {
      const kind = d.columns.find((x) => x.name === c.name)?.kind || '';
      cols += `<div class="scol">
        <span class="name" title="${escapeAttr(c.name)}">${escapeHtml(c.name)}</span>
        <span class="bytes">${hexHtml(c.bytes, kind)}</span>
        <span class="note" title="${escapeAttr(c.note)}">${escapeHtml(c.note)}</span>
      </div>`;
    });
    el.innerHTML = `<div class="sample-head"><b>输入第 ${s.row + 1} 行</b>
      <span>${s.length} 字节</span>
      <span>整键 <code>${escapeHtml(s.key)}</code></span></div>
      <div class="sample-cols">${cols}</div>`;
    host.appendChild(el);
  });
}

const PHASES = [
  ['encodeNs', '表达式求值 & 编码', '#6ea8fe'],
  ['partitionNs', '首字节分桶（并行）', '#5ad1a0'],
  ['bucketSortNs', '各桶 MSD 基数排序', '#f0b45a'],
  ['heapNs', '堆维护 Top-K', '#c793ff'],
  ['concatNs', '按桶号拼接', '#8b96a8'],
];

function renderPhases(d) {
  const st = d.stats;
  const host = $('phases');
  host.innerHTML = '';
  const max = Math.max(st.encodeNs, st.partitionNs, st.bucketSortNs, st.heapNs, 1);
  PHASES.forEach(([key, label, color]) => {
    const v = st[key] || 0;
    const row = document.createElement('div');
    row.className = 'phase';
    row.innerHTML = `<span>${label}</span>
      <span class="bar"><i style="width:${Math.max(1, (v / max) * 100)}%;background:${color}"></i></span>
      <span class="num">${ms(v)}</span>`;
    host.appendChild(row);
  });
  $('totalHint').textContent = `总计 ${ms(st.totalNs)}`;
  const items = [
    ['行数', fmtInt(st.rows)],
    ['排序列', st.columns],
    ['执行模式', st.mode],
    ['并行', st.parallel ? `是 · ${st.workers} worker` : '否'],
    ['分桶数', fmtInt(st.buckets)],
    ['最大桶行数', fmtInt(st.maxBucketRows)],
    ['编码 worker', st.encodeWorkers],
    ['平均键长', d.keyLen.toFixed(1) + ' B'],
    ['键总字节', fmtSize(st.keyBytes)],
    ['基数排序轮次', fmtInt(st.radixPasses)],
    ['插入排序段数', fmtInt(st.insertionRuns)],
    ['Top-K', st.topK > 0 ? fmtInt(st.topK) : '关闭'],
  ];
  $('stats').innerHTML = items.map(([k, v]) => `<div><dt>${k}</dt><dd>${escapeHtml(String(v))}</dd></div>`).join('');
}

function renderCompare(d) {
  const host = $('comparePanel');
  if (!d.compare) { host.innerHTML = '<div class="empty">勾选“与逐列比较排序对比耗时”后运行</div>'; return; }
  const c = d.compare;
  const max = Math.max(c.engineNs, c.referenceNs);
  host.innerHTML = `
    <div class="cmprow"><span>编码键 + 基数排序</span>
      <span class="bar"><i style="width:${(c.engineNs / max) * 100}%;background:var(--accent)"></i></span>
      <span class="num">${ms(c.engineNs)}</span></div>
    <div class="cmprow"><span>sort.SliceStable 逐列比较</span>
      <span class="bar"><i style="width:${(c.referenceNs / max) * 100}%;background:#6ea8fe"></i></span>
      <span class="num">${ms(c.referenceNs)}</span></div>
    <div class="speedup">${c.speedup.toFixed(1)}× <small>加速比 · ${fmtInt(c.rows)} 行</small></div>
    <div>结果一致性 ${c.verifyOk ? '<span class="badge ok">完全一致</span>' : '<span class="badge bad">不一致</span>'}</div>`;
}

function renderColumns(d) {
  const rows = d.columns.map((c) => `<tr>
    <td class="rank">#${c.index + 1}</td>
    <td>${escapeHtml(c.name)}</td>
    <td>${escapeHtml(c.kind)}</td>
    <td>${escapeHtml(c.expr)}</td>
    <td class="dir">${escapeHtml(c.dir)}${c.pinyin ? ' · 拼音' : ''}</td>
    <td class="dir">${escapeHtml(c.nulls)}</td>
    <td class="rule">${escapeHtml(c.encoding)}</td>
  </tr>`).join('');
  $('colTable').innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>#</th><th>列名</th><th>类型</th><th>表达式</th><th>方向</th><th>空值</th><th>编码规则</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

function renderOrdering(d) {
  const host = $('ordering');
  const st = d.stats;
  $('orderHint').textContent = st.topK > 0 ? `Top-${st.topK} 中前 ${d.ordering.length} 行` : `共 ${fmtInt(st.rows)} 行，按排序键输出前 ${d.ordering.length} 行`;
  const head = (META ? META.fields : []).map((f) => `<th>${escapeHtml(f.name)}</th>`).join('');
  const body = d.ordering.map((r) => `<tr><td class="rank">${r.rank}</td>${r.values.map((v) => `<td title="${escapeAttr(v)}">${escapeHtml(v)}</td>`).join('')}</tr>`).join('');
  host.innerHTML = `<table><thead><tr><th>排名</th>${head}</tr></thead><tbody>${body}</tbody></table>`;
}

function fmtInt(n) { return (n ?? 0).toLocaleString('en-US'); }
function fmtSize(n) {
  if (n > 1e9) return (n / 1e9).toFixed(2) + ' GB';
  if (n > 1e6) return (n / 1e6).toFixed(1) + ' MB';
  if (n > 1e3) return (n / 1e3).toFixed(0) + ' KB';
  return n + ' B';
}

$('addCol').onclick = () => addColumn({ expr: 'zone', kind: 'int', dir: 'asc', nulls: 'first', pinyin: false });
$('preset').onclick = () => { $('columns').innerHTML = ''; PRESET.forEach(addColumn); };
$('run').onclick = run;

(async function init() {
  await loadMeta();
  PRESET.forEach(addColumn);
  // Run once so the page opens with a populated example.
  $('rows').value = 200000;
  await run();
})();
