// 把 ssh_run 抓回来的 stdout（@@JSON@@ + base64）解成 testdata 下的金样本文件。
// 用法: node tools/golden/decode_golden.mjs <raw.txt> <out.json>
import fs from 'node:fs';

const [rawPath, outPath] = process.argv.slice(2);
const raw = fs.readFileSync(rawPath, 'utf8');
const i = raw.indexOf('@@JSON@@');
if (i < 0) {
  console.error('输出里没有 @@JSON@@，金样本脚本大概率报错了：\n' + raw.slice(-3000));
  process.exit(1);
}
const rest = raw.slice(i + '@@JSON@@'.length).replace(/^[\r\n]+/, '');
const nl = rest.search(/[\r\n]/);
const b64 = nl < 0 ? rest : rest.slice(0, nl);
const json = Buffer.from(b64, 'base64').toString('utf8');
const obj = JSON.parse(json);
fs.writeFileSync(outPath, JSON.stringify(obj, null, 2) + '\n');

const S = obj.sections;
console.log('bytes', json.length, 'sections', Object.keys(S).length, 'exprs', obj.exprs.length);
console.log('FALLBACKS', JSON.stringify(S.__fallbacks__));
for (const k of Object.keys(S)) {
  if (k === '__fallbacks__') continue;
  const s = S[k];
  console.log(' ', k.padEnd(26), 'log=' + ((s.log || s.head_log || []).length + '').padEnd(4),
    JSON.stringify(s).slice(0, 96));
}
