// 把 tools/golden 下的某个录制脚本送进 NAS 上的 Python 容器里跑（真 Python 当 oracle），
// 再把 stdout 里 @@JSON@@ 后面那段 base64 落成 testdata 的金样本文件。
//
//   export NAS_HOST=… NAS_USER=… NAS_PW=…   # 三个都只进环境，不写文件
//   node tools/golden/run_on_nas.mjs golden_generate internal/engine/testdata/generate_golden.json
//
// 容器名默认 `muse2api`（线上那份 Python）。追上游新版本时不必动线上容器：
// 用同一个镜像 + `--network none` + 挂载新源码，起一个只做解释器的 oracle 容器，
// 再用 NAS_CONTAINER 指过去 —— 空账号池加无外网，结构性地保证录制不消耗任何额度。
//   docker run -d --name muse2api-oracle --init --network none \
//     -v /path/to/upstream/src:/app -v /path/to/oracle/data:/app/data \
//     -e MUSE2API_KEY=oracle-local --entrypoint sleep <上游镜像> infinity
//   NAS_CONTAINER=muse2api-oracle node tools/golden/run_on_nas.mjs …
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
// 主机与用户名同样只从环境读：这份脚本会随源码发布，不该把家里的内网地址写死在里面。
// 地址允许写成 `http://1.2.3.4/` 这种形式（ssh 只吃裸主机名，这里统一剥掉协议与尾部斜杠）。
function needEnv(key) {
  const v = (process.env[key] || '').trim();
  if (!v) {
    console.error(`缺 ${key} 环境变量（与 NAS_PW 一样只从环境读）`);
    process.exit(2);
  }
  return v;
}
const NAS_HOST = needEnv('NAS_HOST').replace(/^[a-zA-Z]+:\/\//, '').replace(/\/+$/, '');
const NAS_USER = needEnv('NAS_USER');
const NAS_PORT = Number(process.env.NAS_PORT || 22);
// 每一步都套 timeout：容器里卡住的 python 会变成宿主机上的孤儿进程，之前踩过。
const ORACLE = (process.env.NAS_CONTAINER || 'muse2api').trim();
const run1 = (cmd) => `timeout 300 ${cmd}`;

const { Client } = await loadSSH2();

const SRC = path.join('tools', 'golden', scriptName.endsWith('.py') ? scriptName : scriptName + '.py');
const b64 = fs.readFileSync(SRC).toString('base64');
const remote = [
  run1(`printf '%s' '${b64}' | base64 -d > /tmp/gg.py`),
  run1(`docker cp /tmp/gg.py ${ORACLE}:/tmp/gg.py`),
  run1('rm -f /tmp/gg.py'),
  // 先 AST 解析：语法错在容器里当场报，不用等整轮跑完才发现
  run1(`docker exec ${ORACLE} python3 -c "import ast;ast.parse(open('/tmp/gg.py',encoding='utf-8').read());print('AST_OK')"`),
  run1(`docker exec ${ORACLE} python3 /tmp/gg.py 2>&1 | tail -c 900000`),
  run1(`docker exec ${ORACLE} rm -f /tmp/gg.py`),
  run1(`docker exec ${ORACLE} rm -rf /tmp/genc`),
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
    .connect({ host: NAS_HOST, port: NAS_PORT, username: NAS_USER, password: PW, readyTimeout: 20000 });
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
