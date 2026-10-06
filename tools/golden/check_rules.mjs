// 用 tools/extract 的权威 JS 文本（engine_js.json，来自线上 engine.py）逐条套用
// golden_generate.py 的 RULES 顺序，打印「哪条规则认领了这段 JS」。
// 认领不到 = 该分支不可能被驱动；认领错 = 场景值喂给了另一个函数。两种都是静默错误。
import fs from 'node:fs';

const py = fs.readFileSync('tools/golden/golden_generate.py', 'utf8');
const rulesBlock = py.slice(py.indexOf('RULES = ['), py.indexOf(']\n\nDEFAULTS'));
const marks = [];
for (const m of rulesBlock.matchAll(/\("((?:[^"\\]|\\.)*)",\s*"(\w+)"\)/g)) {
  marks.push([JSON.parse('"' + m[1] + '"'), m[2]]);
}
console.log('规则数', marks.length);

// js.go 里每个常量上面有 // engine.py:NNN，用它把常量名和权威文本对上
const go = fs.readFileSync('internal/engine/js.go', 'utf8').split(/\r?\n/);
const byLine = {};
for (let i = 0; i < go.length; i++) {
  const c = go[i].match(/\/\/ engine\.py:(\d+)/);
  if (!c) continue;
  for (let j = i + 1; j < go.length; j++) {
    if (!go[j].trim()) continue;
    const n = go[j].match(/^\t*(\w+)\s*=\s*/);
    if (n) { byLine[c[1]] = n[1]; break; }
  }
}

const cat = JSON.parse(fs.readFileSync('internal/engine/testdata/engine_js.json', 'utf8'));
const entries = [...cat.consts, ...cat.concats, ...cat.named];
let unmapped = 0;
for (const e of entries) {
  if (!e.b64) continue;
  const txt = Buffer.from(e.b64, 'base64').toString('utf8');
  const hitIdx = marks.findIndex(([m]) => txt.includes(m));
  const name = byLine[e.line] || '(line ' + e.line + ')';
  if (hitIdx < 0) { unmapped++; console.log('*** 无规则'.padEnd(12), name.padEnd(24), txt.slice(0, 52).replace(/\n/g, ' ')); continue; }
  console.log(marks[hitIdx][1].padEnd(12), name.padEnd(24), '第' + (hitIdx + 1) + '条', txt.slice(0, 40).replace(/\n/g, ' '));
}
console.log('未认领条数', unmapped, '/', entries.length);
