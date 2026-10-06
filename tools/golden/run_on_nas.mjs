// 把 tools/golden 下的某个录制脚本送进 NAS 上的 muse2api 容器里跑（真 Python 当 oracle），
// 再把 stdout 里 @@JSON@@ 后面那段 base64 落成 testdata 的金样本文件。
//
//   NAS_PW=*** node tools/golden/run_on_nas.mjs golden_generate internal/engine/testdata/generate_golden.json
//
// 密码只从环境变量读，绝不落盘。依赖 ssh2（一次性装在临时目录里，用完卸载）：
//   若 import 'ssh2' 失败，脚本会打印安装指引然后退出。
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

// ssh2 是临时装在 .remote/ 下的（那目录不入库），从仓库根按默认规则解析不到，
// 所以显式列几个候选 node_modules；全都没有就给出安装指引。
async function loadSSH2() {
  const req = createRequire(path.join(process.cwd(), 'noop.js'));
  try {
    return await import('ssh2');
  } catch { /* 换绝对路径找 */ }
  for (const base of ['.remote', '.', '..']) {
    const dir = path.resolve(base, 'node_modules', 'ssh2');
    if (fs.existsSync(dir)) return req(dir);
  }
  console.error([
    '找不到 ssh2 模块。一次性安装（用完记得卸载）：',
    '  mkdir -p .remote && cd .remote && npm i ssh2',
  ].join('\n'));
  process.exit(1);
}

const [scriptName, outPath] = process.argv.slice(2);
if (!scriptName || !outPath) {
  console.error('用法: run_on_nas.mjs <tools/golden 下的脚本名> <写出的 testdata 路径>');
  process.exit(2);
}
const PW = process.env.NAS_PW;
if (!PW) {
  console.error('缺 NAS_PW 环境变量（只从环境读，不写文件）');
  process.exit(2);
}

const { Client } = await loadSSH2();

const SRC = path.join('tools', 'golden', scriptName.endsWith('.py') ? scriptName : scriptName + '.py');
const b64 = fs.readFileSync(SRC).toString('base64');
const remote = [
  `printf '%s' '${b64}' | base64 -d > /tmp/gg.py`,
  'docker cp /tmp/gg.py muse2api:/tmp/gg.py',
  'rm -f /tmp/gg.py',
  // 先 AST 解析：语法错在容器里当场报，不用等整轮跑完才发现
  `docker exec muse2api python3 -c "import ast;ast.parse(open('/tmp/gg.py',encoding='utf-8').read());print('AST_OK')"`,
  'docker exec muse2api python3 /tmp/gg.py 2>&1 | tail -c 900000',
  'docker exec muse2api rm -f /tmp/gg.py',
  'docker exec muse2api rm -rf /tmp/genc',
].join(' && ');

const run = (cmd) => new Promise((res) => {
  const c = new Client();
  c.on('ready', () => {
    c.exec(cmd, (e, s) => {
      if (e) { c.end(); return res({ code: -1, out: 'ERR ' + e.message }); }
      let o = '';
      s.on('data', (d) => { o += d; });
      s.stderr.on('data', (d) => { o += '[err] ' + d; });
      s.on('close', (code) => { c.end(); res({ code, out: o }); });
    });
  }).on('error', (e) => res({ code: -1, out: 'CONNECT ' + e.message }))
    .connect({ host: '192.0.2.10', port: 22, username: 'nasuser', password: PW, readyTimeout: 20000 });
});

const r = await run(remote);
const marker = '@@JSON@@';
const i = r.out.indexOf(marker);
if (i < 0) {
  console.error('没有 @@JSON@@ 输出，脚本大概率报错了：\n' + r.out.slice(-4000));
  process.exit(1);
}
const rest = r.out.slice(i + marker.length).replace(/^[\r\n]+/, '');
const nl = rest.search(/[\r\n]/);
const json = Buffer.from(nl < 0 ? rest : rest.slice(0, nl), 'base64').toString('utf8');
const obj = JSON.parse(json);
fs.mkdirSync(path.dirname(outPath), { recursive: true });

// 附属产物（如导出的 openapi.json）：脚本用 __files__ 声明，一并落地后从主金样本里摘掉。
const files = obj.__files__ || {};
delete obj.__files__;
fs.writeFileSync(outPath, JSON.stringify(obj, null, 2) + '\n');
for (const [rel, b64] of Object.entries(files)) {
  if (!b64) { console.error('附属产物为空：' + rel); continue; }
  const buf = Buffer.from(b64, 'base64');
  fs.mkdirSync(path.dirname(rel), { recursive: true });
  fs.writeFileSync(rel, buf);
  console.log('附带写出', rel, buf.length, 'bytes');
}

const S = obj.sections || {};
console.log('写出', outPath, 'bytes', json.length, 'sections', Object.keys(S).length,
  'exprs', (obj.exprs || []).length);
if (S.__fallbacks__) console.log('FALLBACKS', JSON.stringify(S.__fallbacks__));
if (S.__fallbacks__ && Object.keys(S.__fallbacks__).some((k) => k.startsWith('MISSING'))) {
  console.error('有分支没被驱动（MISSING），先给场景补值再采信这份金样本');
  process.exit(1);
}
