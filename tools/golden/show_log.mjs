// 把金样本某一段的调用序列按「规则名」打印出来，方便肉眼看 Python 到底轮询了什么。
// 用法: node .remote/show_log.mjs <section>
import fs from 'node:fs';

const py = fs.readFileSync('tools/golden/golden_generate.py', 'utf8');
const rulesBlock = py.slice(py.indexOf('RULES = ['), py.indexOf(']\n\nDEFAULTS'));
const marks = [];
for (const m of rulesBlock.matchAll(/\("((?:[^"\\]|\\.)*)",\s*"(\w+)"\)/g)) {
  marks.push([JSON.parse('"' + m[1] + '"'), m[2]]);
}
const g = JSON.parse(fs.readFileSync('internal/engine/testdata/generate_golden.json', 'utf8'));
const sec = g.sections[process.argv[2]];
if (!sec) { console.error('没有这一段：' + process.argv[2]); process.exit(1); }
const log = sec.log || sec.head_log || [];
const from = Number(process.argv[3] || 0);
log.forEach((e, i) => {
  if (i < from) return;
  if (e[0] === 'send') { console.log(String(i).padStart(3), 'send', e[1], JSON.stringify(e[2]).slice(0, 60)); return; }
  const expr = g.exprs[e[1]] || '';
  const hit = marks.find(([m]) => expr.includes(m));
  console.log(String(i).padStart(3), 'js  ', String(hit ? hit[1] : '???').padEnd(11),
    (e[4] ? 'RAISE ' : '') + String(JSON.stringify(e[3])).slice(0, 70));
});
console.log('总条数', log.length, 'timeout', sec.timeout);
