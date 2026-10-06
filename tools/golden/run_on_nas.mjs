// 把 tools/golden 下的某个录制脚本送进 NAS 上的 muse2api 容器里跑（真 Python 当 oracle），
// 再把 stdout 里 @@JSON@@ 后面那段 base64 落成 testdata 的金样本文件。
//
//   NAS_PW=*** node tools/golden/run_on_nas.mjs golden_generate internal/engine/testdata/generate_golden.json
//
// 密码只从环境变量读，绝不落盘。依赖 ssh2（一次性装在临时目录里，用完卸载）：
//   若 import 'ssh2' 失败，脚本会打印安装指引然后退出。
import fs from 'node:fs';
import path from 'node:path';

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

let Client;
try {
  ({ Client } = await import('ssh2'));
} catch {
  console.error([
    '找不到 ssh2 模块。一次性安装（用完记得卸载）：',
    '  mkdir .tmp/sshrun && cd .tmp/sshrun && npm i ssh2',
    '  cd ../..  && NAS_PW=*** node --experimental-vm-modules ... ',
    '或直接：把本脚本拷进 .tmp/sshrun 里跑，那里能解析到 node_modules。',
  ].join('\n'));
  process.exit(1);
}

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
fs.writeFileSync(outPath, JSON.stringify(obj, null, 2) + '\n');

const S = obj.sections || {};
console.log('写出', outPath, 'bytes', json.length, 'sections', Object.keys(S).length,
  'exprs', (obj.exprs || []).length);
if (S.__fallbacks__) console.log('FALLBACKS', JSON.stringify(S.__fallbacks__));
if (S.__fallbacks__ && Object.keys(S.__fallbacks__).some((k) => k.startsWith('MISSING'))) {
  console.error('有分支没被驱动（MISSING），先给场景补值再采信这份金样本');
  process.exit(1);
}
