// NAS 部署小工具：把 Go 版的构建产物送上去、出镜像、起容器、量空闲内存。
//
// 三个必填环境变量：NAS_HOST（地址）、NAS_USER（登录名）、NAS_PW（密码，只进环境）。
// 下面统一写成 `NAS_ENV`：
//   export NAS_HOST="$(awk -F': *' '/^NAS_IP/{print $2}' ~/nas.env)"
//   export NAS_USER="$(awk -F': *' '/^NAS_USERNAME/{print $2}' ~/nas.env)"
//   export NAS_PW="$(awk -F': *' '/^NAS_PWD/{print $2}' ~/nas.env)"
//
//   node tools/nas_deploy.mjs probe    # 只读体检：磁盘、docker、端口、线上 chromium 版本
//   node tools/nas_deploy.mjs push     # 上传 Dockerfile / version.json / build/muse2api-go
//   node tools/nas_deploy.mjs build    # docker build
//   node tools/nas_deploy.mjs run      # docker run -d 起在 NAS_HOST_PORT（默认 8080），独立 data-go
//   node tools/nas_deploy.mjs stats    # 采一次空闲内存（docker stats + cgroup + 进程树）
//   node tools/nas_deploy.mjs logs     # 容器日志尾部
//   node tools/nas_deploy.mjs exec '<只读命令>'
//
// 规矩：
//   - 密码只从环境变量读，绝不写文件、绝不进命令行回显。
//   - 主机与用户名同样只从环境读（NAS_HOST / NAS_USER）：这两个脚本会随源码一起发布，
//     家里内网地址与账号名不该写死在里面。用法是在仓库外放一个 nas.env（NAS_IP / NAS_USERNAME / NAS_PWD），
//     取用：export NAS_HOST="$(awk -F': *' '/^NAS_IP/{print $2}' …/nas.env)"，以此类推。
//   - 全程只在**新建**的 /opt/muse2api-go 里动作；现役 muse2api 容器只读不写。
//   - probe/exec 是只读的；push/build/run 会在 NAS 上新增目录、镜像与容器（不动既有服务）。
import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { execSync } from 'node:child_process';
import { createRequire } from 'node:module';

function needEnv(key) {
  const v = (process.env[key] || '').trim();
  if (!v) {
    console.error(`缺 ${key} 环境变量（与 NAS_PW 一样，只从环境读，不写进代码或文件）`);
    process.exit(2);
  }
  return v;
}

// 有人会把地址写成 `http://192.0.2.10/` 这种形式（家里 nas.env 就是这么存的），
// ssh 只吃裸主机名，所以这里统一剥掉协议与尾部斜杠。
function hostOnly(v) {
  return v.replace(/^[a-zA-Z]+:\/\//, '').replace(/\/+$/, '');
}

const HOST = hostOnly(needEnv('NAS_HOST')), USER = needEnv('NAS_USER'), PORT = Number(process.env.NAS_PORT || 22);
// NAS_REMOTE_DIR 是构建上下文与 data-go 的落点，默认给一个中性路径；
// 各家 NAS 的挂载点不同（例如飞牛是 /vol1/1000/docker/…），按需用自己的那个，别写进仓库。
const REMOTE_DIR = (process.env.NAS_REMOTE_DIR || '/opt/muse2api-go').trim().replace(/\/+$/, '');
const IMG = 'muse2api-go:1.6.3';
const CONTAINER = 'muse2api-go';
const HOST_PORT = process.env.NAS_HOST_PORT || '8080';

const PW = process.env.NAS_PW;
if (!PW) {
  console.error('缺 NAS_PW 环境变量（只从环境读，不写文件）');
  process.exit(2);
}

// ssh2 装在 .remote/ 下（那目录不入库），仓库根按默认规则解析不到。
async function loadSSH2() {
  const req = createRequire(path.join(process.cwd(), 'noop.js'));
  try {
    return await import('ssh2');
  } catch { /* 换绝对路径找 */ }
  for (const base of ['.remote', '.', '..']) {
    const dir = path.resolve(base, 'node_modules', 'ssh2');
    if (fs.existsSync(dir)) return req(dir);
  }
  console.error('找不到 ssh2 模块。一次性安装（用完记得卸载）：\n  mkdir -p .remote && cd .remote && npm i ssh2');
  process.exit(1);
}
// 动态载入 ssh2：静态 import 会在 loadSSH2 的兜底路径之前就解析失败。
const { Client } = await loadSSH2();

const mode = process.argv[2] || 'probe';
const inlineCmd = process.argv[3];

// 每个步骤要跑的远端命令；{read: true} 的条目在任何模式下都允许。
const steps = {
  probe: [
    'df -h /vol1 | tail -2',
    'docker --version; docker compose version 2>&1 | head -2',
    'docker ps --format "{{.Names}} {{.Image}} {{.Status}} {{.Ports}}"',
    `ss -ltnp 2>/dev/null | grep -c ":${HOST_PORT}" || echo "${HOST_PORT} 空闲"`,
    // 线上那套的 chromium 版本：Go 镜像必须拿到同一个包，DOM 行为才可比
    'docker exec muse2api chromium --version 2>/dev/null || echo "读不到"',
    'docker exec muse2api python3 -c "import fastapi,starlette,sys;print(f\'py {sys.version.split()[0]} fastapi {fastapi.__version__} starlette {starlette.__version__}\')" 2>&1 | tail -1',
    'docker exec muse2api cat /etc/debian_version 2>/dev/null || echo "读不到"',
    `ls -la ${REMOTE_DIR} 2>/dev/null || echo "目标目录还不存在（正常）"`,
    'nproc; free -m | head -2',
  ],
  push: [],   // 走下面 upload 分支：把 git 跟踪的源码打成 ctx.tar 送上去
  build: [
    // 上下文是 push 解出来的源码树，Dockerfile 在第一步里用官方 golang 镜像编译 ——
    // 与外部用户 `docker build .` 走的是同一条路径，公开的东西和我们实际跑的东西不会漂移。
    `test -f ${REMOTE_DIR}/ctx/Dockerfile || { echo "先跑 push（ctx 还没上传）"; exit 1; }`,
    `cd ${REMOTE_DIR}/ctx && docker build -t ${IMG} . 2>&1 | tail -30`,
    `docker images ${IMG} --format "{{.Repository}}:{{.Tag}} {{.Size}}"`,
  ],
  run: [
    `mkdir -p ${REMOTE_DIR}/data-go`,
    // --init：让 docker 自带的最小 init 当 PID 1。Go 进程当 PID 1 时会收养 chromium 的孤儿子孙，
    // 而我们只 Wait() 自己那个直接子进程 —— 每「起一次浏览器再释放」就攒一排僵尸进程。
    // --env-file：密钥从 ${REMOTE_DIR}/.env 读（与 Python 侧同一把），重建容器也不再换 key。
    `docker rm -f ${CONTAINER} 2>/dev/null; docker run -d --name ${CONTAINER} --restart unless-stopped --init ` +
      `-p ${HOST_PORT}:18610 -v ${REMOTE_DIR}/data-go:/app/data --shm-size=2gb ` +
      `--log-opt max-size=100m --log-opt max-file=5 ` +
      `--env-file ${REMOTE_DIR}/.env -e MUSE2API_BROWSER_IDLE_MINUTES=${process.env.BROWSER_IDLE_MINUTES || 30} ` +
      `-e MUSE2API_WARM_TAB=0 ${IMG} && sleep 3 && docker ps --filter name=${CONTAINER} --format "{{.Names}} {{.Status}}"`,
    `K=$(grep -o "^MUSE2API_KEY=.*" ${REMOTE_DIR}/.env | cut -d= -f2); curl -s -m 6 -o /dev/null -w "用 .env 里的 key 打 /admin/accounts: %{http_code}\\n" -H "Authorization: Bearer $K" http://127.0.0.1:${HOST_PORT}/admin/accounts`,
    `docker logs --tail 20 ${CONTAINER} 2>&1`,
  ],
  stats: [
    `docker stats --no-stream --format "{{.Name}} mem={{.MemUsage}} pids={{.PIDs}}" ${CONTAINER}`,
    // cgroup v1/v2 都试一次：anon 才是真正压不回去的那部分
    `cat /sys/fs/cgroup/memory/docker/${CONTAINER}/memory.stat 2>/dev/null | grep -E "^(rss|cache|inactive_file|active_file) " || ` +
      `cat /sys/fs/cgroup/system.slice/docker-${CONTAINER}.scope/stat 2>/dev/null | head -3 || echo "cgroup 路径没猜到"`,
    `docker exec ${CONTAINER} sh -c "ps -eo pid,ppid,rss,comm | head -20" 2>&1`,
    `docker exec ${CONTAINER} sh -c "pgrep -c chromium || echo 0" 2>&1`,
  ],
  logs: [`docker logs --tail 60 ${CONTAINER} 2>&1`],
};

if (mode === 'exec') {
  if (!inlineCmd) { console.error('exec 需要给一条命令'); process.exit(2); }
  steps.exec = [inlineCmd];
}

const cmds = steps[mode];
if (!cmds) { console.error('未知模式：' + mode + '（probe|push|build|run|stats|logs|exec）'); process.exit(2); }

const c = new Client();
const exec = (cmd) => new Promise((res) => {
  c.exec(cmd, (e, s) => {
    if (e) return res({ cmd, out: 'ERR ' + e.message, code: -1 });
    let o = '';
    s.on('data', (d) => { o += d; });
    s.stderr.on('data', (d) => { o += '[err] ' + d; });
    s.on('close', (code) => res({ cmd, out: o, code }));
  });
});

// 上传走 exec 的 stdin：`cat > 目标文件`。
// 不用 sftp（这台 sshd 的 sftp 子系统起不来，会话会静默卡死），也不走 base64 命令行（8.7MB 超 argv 限制）。
const upload = (local, remote) => new Promise((res, rej) => {
  c.exec('cat > ' + JSON.stringify(remote) + ' && echo WROTE_OK', (e, s) => {
    if (e) return rej(e);
    let out = '';
    s.on('data', (d) => { out += d; });
    s.stderr.on('data', (d) => { out += '[err] ' + d; });
    s.on('close', (code) => res({ code, out }));
    let sent = 0;
    const rs = fs.createReadStream(local);
    rs.on('data', (ch) => {
      sent += ch.length;
      if (sent % (2 * 1024 * 1024) < ch.length) {
        process.stdout.write(`\r  ${path.basename(local)} 已送 ${(sent / 1048576).toFixed(1)} MB   `);
      }
    });
    rs.on('error', rej);
    rs.pipe(s);
  });
});

c.on('ready', async () => {
  if (mode === 'push') {
    // 上传的是「git 跟踪的源码」打成的 tar，而不是预编译二进制：镜像改为多阶段构建后，
    // 远端要能独立 `docker build`，与外人 clone 后跑的完全同一条路径。
    // 只取 git ls-files，天然排除了 .remote/、docs/、build/ 这些本地脚手架与个人痕迹。
    console.log(`远端目录 ${REMOTE_DIR}（NAS_REMOTE_DIR 没设时用默认值；Git Bash 会把 / 开头的值改写成 D:/Program Files/Git/…，传路径务必带 MSYS_NO_PATHCONV=1）`);
    const list = execSync('git ls-files -z', { encoding: 'buffer' }).toString('latin1').split('\0').filter(Boolean);
    if (!list.length) { console.error('git ls-files 为空'); c.end(); process.exit(1); }
    const TARBALL = '.remote/ctx.tar';
    fs.mkdirSync('.remote', { recursive: true });
    fs.rmSync(TARBALL, { force: true });
    // GNU tar 的 --null 是位置性选项，必须在 -T 之前，否则"读到 NUL 字节"直接报错。
    execSync('tar --null --format=gnu -cf ' + JSON.stringify(TARBALL) + ' --no-recursion -T -',
      { input: Buffer.from(list.join('\0') + '\0', 'latin1'), stdio: ['pipe', 'ignore', 'inherit'] });
    const bytes = fs.statSync(TARBALL).size;
    const localSha = createHash('sha256').update(fs.readFileSync(TARBALL)).digest('hex');
    console.log(`打包 ${list.length} 个文件 / ${(bytes / 1048576).toFixed(2)} MB，sha256 ${localSha.slice(0, 16)}…`);

    const mk = await exec(`mkdir -p ${JSON.stringify(REMOTE_DIR)}`);
    if (mk.code !== 0) { console.error('mkdir 失败：' + mk.out); c.end(); process.exit(1); }
    const up = await upload(TARBALL, `${REMOTE_DIR}/ctx.tar`);
    if (!up.out.includes('WROTE_OK')) { console.error('写入失败：' + up.out); c.end(); process.exit(1); }

    // 先按摘要确认送达无损，再解包 —— 别解出一个半截的上下文去构建。
    const sum = await exec(`sha256sum ${JSON.stringify(`${REMOTE_DIR}/ctx.tar`)} 2>&1`);
    const remoteSha = (sum.out.match(/^[0-9a-f]{64}/) || [''])[0];
    if (remoteSha !== localSha) {
      console.error(`上下文摘要不符：本地 ${localSha.slice(0, 16)}… 远端 ${remoteSha.slice(0, 16)}…`);
      c.end();
      process.exit(1);
    }
    console.log('远端摘要一致 ✓');

    const ext = await exec(`cd ${JSON.stringify(REMOTE_DIR)} && rm -rf ctx && mkdir ctx ` +
      `&& tar -xf ctx.tar -C ctx && rm -f ctx.tar ` +
      `&& test -f ctx/Dockerfile && test -f ctx/go.mod && echo "Dockerfile 与 go.mod 就位 ✓" ` +
      `&& echo "解包 $(find ctx -type f | wc -l) 个文件"`);
    console.log(ext.out.trim());
    if (ext.code !== 0) { console.error('解包失败：' + ext.out); c.end(); process.exit(1); }
  } else {
    for (const cmd of cmds) {
      const r = await exec(cmd);
      const shown = cmd.length > 100 ? cmd.slice(0, 100) + ' …[命令回显已截断]' : cmd;
      console.log(`\n===== ${shown}\n${r.out.replace(/\s+$/, '')}${r.code ? `  (exit ${r.code})` : ''}`);
    }
  }
  c.end();
}).on('error', (e) => { console.error('CONNECT ' + e.message); process.exit(1); })
  .connect({ host: HOST, port: PORT, username: USER, password: PW, readyTimeout: 20000 });
