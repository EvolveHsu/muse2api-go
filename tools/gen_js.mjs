// tools/gen_js.mjs —— 从真 Python 的 AST 抽取结果生成 internal/engine/js.go
//
// 为什么不手抄：engine.py 里 30 条注入 JS 共 ~16KB，手抄错一个字符就会让线上行为
// 静默变化（选择器、正则、短路条件都可能错而没人发现）。金样本由
// .remote/extract_js.py 在 NAS 容器里用 ast 从**部署中的** engine.py 抽出。
//
// 用法：node tools/gen_js.mjs   （读 .tmp/jsout.txt，写 js.go + testdata）
import fs from 'node:fs';
import path from 'node:path';

const OUT_DIR = 'internal/engine';
const TESTDATA = path.join(OUT_DIR, 'testdata', 'engine_js.json');
const GOSRC = path.join(OUT_DIR, 'js.go');
const PY = '.reference/muse2api-py/engine.py';
const RAW = '.tmp/jsout.txt';

const pyLines = fs.readFileSync(PY, 'utf8').split('\n');
const ind = s => (s.match(/^ */)[0].length);
const NUL = String.fromCharCode(0);
const varRe = new RegExp(NUL + 'VAR:([A-Za-z_][A-Za-z0-9_]*)' + NUL, 'g');

let section = null;
const consts = [], concats = [], named = [];
for (const line of fs.readFileSync(RAW, 'utf8').split('\n')) {
  const t = line.trim();
  if (t.startsWith('=== JS_CONSTS')) { section = 'c'; continue; }
  if (t.startsWith('=== CONCAT_TEMPLATES')) { section = 'k'; continue; }
  if (t.startsWith('=== NAMED')) { section = 'n'; continue; }
  if (!t.startsWith('{')) continue;
  let o;
  try { o = JSON.parse(t); } catch { continue; }
  (section === 'c' ? consts : section === 'k' ? concats : named).push(o);
}

const dec = o => Buffer.from(o.b64, 'base64').toString('utf8');
const strip = s => s.replace(varRe, '');

// 拼接模板（形如 "前段" + ATT_SEL + "后段"）的组成字面量也会被 ast.walk 单独抓到，
// 那些片段不算独立常量。判定必须同时要求行号相同，否则会误删
// 恰好被那段拼接包含下来的真正独立字面量（如 innerText.slice(-500)）。
function isFragment(text, line) {
  return concats.some(c => c.line === line && strip(dec(c)) !== text && strip(dec(c)).includes(text));
}

// Python 里具名的类常量给个可读的 Go 名。
// 键是 engine.py 里的行号 —— 上游升级后行号会平移，重新生成时按新的行号更新这里，
// 引用点用的语义名保持稳定（js_test.go 的 TestNamedConstantsMatchPython 兜底）。
const NAMED_MAP = {
  // 1.6.1（engine.py 行号）：
  366: 'jsWaitWsReady',
  390: 'jsResetThread',
  415: 'jsResetThreadReady',
  429: 'jsHasTextarea',
  462: 'jsBodyText',
  496: 'clickJS',
  520: 'jsQuotaMenuItem',
  540: 'jsQuotaDialog',
  586: 'attJS',
  632: 'sendStateJS',
  641: 'jsFocusInput',
  653: 'jsInputRect',
  669: 'setterJS',
  687: 'jsSendButton',
  713: 'jsInputCleared',
  731: 'jsScrollBottom',
  815: 'jsWaitAttachmentState',
  855: 'extractJS',
  878: 'agentTextJS',
  887: 'userCountJS',
  895: 'agentCountJS',
  903: 'pollChatJS',
  974: 'jsChatTail',
  1046: 'jsDownloadButton',
  1108: 'jsClearAttachments',
  1131: 'injectJS',
  1186: 'jsAttachConfirm',
};

// 拼接模板同样按行号钉住语义名，引用点不跟着行号漂。
const ASSEMBLED_MAP = {
  1292: 'assembleDebugDump',
};

function camel(s) {
  return s.replace(/^_+/, '').split(/_+/).filter(Boolean)
    .map(w => w[0].toUpperCase() + w.slice(1)).join('');
}

// 归属：向上找最近的、缩进严格小于本行的 def（同级 def 不可能是外层）。
function enclosingDef(line) {
  const base = ind(pyLines[line - 1] || '');
  for (let i = line - 2; i >= 0; i--) {
    const s = pyLines[i] || '';
    if (!s.trim() || ind(s) >= base) continue;
    const m = s.match(/^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)/);
    if (m) return m[1];
    if (/^\s*class\b/.test(s)) return null;
  }
  return null;
}

function goString(s) {
  if (!s.includes('`') && !s.includes('\\') && !s.includes(NUL)) return '`' + s + '`';
  return JSON.stringify(s).replace(/\u0000/g, '\\x00');
}

const seen = new Set();
const entries = [];
const dropped = [];
for (const c of consts) {
  const text = dec(c);
  if (seen.has(c.b64)) continue;
  seen.add(c.b64);
  if (isFragment(text, c.line)) { dropped.push({ line: c.line, head: text.slice(0, 40) }); continue; }
  const go = NAMED_MAP[c.line] || ('js' + (camel(enclosingDef(c.line) || 'At') ) + 'L' + c.line);
  entries.push({ go, line: c.line, text });
}

// 拼接模板：同一行只留最长的一条（ast.walk 会把嵌套 BinOp 也报一遍）。
const byLine = new Map();
for (const c of concats) {
  const cur = byLine.get(c.line);
  if (!cur || dec(c).length > dec(cur).length) byLine.set(c.line, c);
}
const asm = [];
for (const c of byLine.values()) {
  const text = dec(c);
  const fn = camel(enclosingDef(c.line) || 'Assemble');
  const goFn = ASSEMBLED_MAP[c.line] || ('assemble' + fn + 'L' + c.line);
  // 注意：split 用带捕获组的正则会连捕获内容一起返回，这里必须用不捕获的版本。
  const parts = text.split(new RegExp(NUL + 'VAR:[A-Za-z_][A-Za-z0-9_]*' + NUL, 'g'));
  const marks = [...text.matchAll(varRe)].map(m => m[1]);
  let expr = '';
  parts.forEach((p, i) => {
    if (p) expr += (expr ? ' +\n\t\t' : '') + goString(p);
    if (i < marks.length) {
      const varGo = { ATT_SEL: 'attSel' }[marks[i]] || marks[i];
      expr += (expr ? ' +\n\t\t' : '') + varGo;
    }
  });
  asm.push({ goFn, line: c.line, text, expr: expr || '""' });
}

// 具名常量（ATT_SEL 这类不含 JS 关键词、上面抓不到的）
const namedOut = [];
for (const n of named) {
  if (n.name === 'ATT_SEL') namedOut.push({ go: 'attSel', name: n.name, text: dec(n) });
}

fs.mkdirSync(path.dirname(TESTDATA), { recursive: true });
fs.writeFileSync(TESTDATA, JSON.stringify({ consts, concats: [...byLine.values()], named, dropped }, null, 1) + '\n');

let out = `// 本文件由 tools/gen_js.mjs 从真 Python 的 ast 抽取结果生成，请勿手改。\n` +
  `// 金样本：internal/engine/testdata/engine_js.json\n` +
  `// 校验：internal/engine/js_test.go 会逐条比对这里每个常量与拼接模板。\n` +
  `package engine\n\nconst (\n`;
for (const n of namedOut) {
  out += `\t// engine.py 模块级常量 ${n.name}\n\t${n.go} = ${goString(n.text)}\n\n`;
}
for (const e of entries) {
  out += `\t// engine.py:${e.line}\n\t${e.go} = ${goString(e.text)}\n\n`;
}
out += `)\n\n`;
for (const a of asm) {
  out += `// engine.py:${a.line} 处由字面量与变量拼接而成的模板\nfunc ${a.goFn}() string {\n\treturn ${a.expr}\n}\n\n`;
}
out += `// jsCatalog 让测试能按名字遍历所有独立常量。\nvar jsCatalog = map[string]string{\n`;
for (const e of entries) out += `\t"${e.go}": ${e.go},\n`;
out += `}\n\n// jsAssembled 是拼接模板的还原函数表，测试逐个与金样本比对。\nvar jsAssembled = map[string]func() string{\n`;
for (const a of asm) out += `\t"${a.goFn}": ${a.goFn},\n`;
out += `}\n`;

fs.writeFileSync(GOSRC, out);
console.log(`consts=${consts.length} 去重后=${entries.length} 片段剔除=${dropped.length} 拼接模板=${asm.length} 具名=${namedOut.length}`);
console.log('生成 ' + GOSRC + ' (' + out.length + ' bytes)');
