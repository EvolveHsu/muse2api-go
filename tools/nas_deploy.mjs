// NAS 部署小工具：把 Go 版的构建产物送上去、出镜像、起容器、量空闲内存。
//
//   NAS_PW=*** node tools/nas_deploy.mjs probe    # 只读体检：磁盘、docker、端口、线上 chromium 版本
//   NAS_PW=*** node tools/nas_deploy.mjs push     # 上传 Dockerfile / version.json / build/muse2api-go
//   NAS_PW=*** node tools/nas_deploy.mjs build    # docker build
//   NAS_PW=*** node tools/nas_deploy.mjs run      # docker run -d 起在 38467，独立 data-go
//   NAS_PW=*** node tools/nas_deploy.mjs stats    # 采一次空闲内存（docker stats + cgroup + 进程树）
//   NAS_PW=*** node tools/nas_deploy.mjs logs     # 容器日志尾部
//   NAS_PW=*** node tools/nas_deploy.mjs exec '<只读命令>'
//
// 规矩：
//   - 密码只从环境变量读，绝不写文件、绝不进命令行回显。
//   - 全程只在**新建**的 /opt/muse2api-go 里动作；现役 muse2api 容器只读不写。
//   - probe/exec 是只读的；push/build/run 会在 NAS 上新增目录、镜像与容器（不动既有服务）。
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

const HOST = '192.0.2.10', USER = 'nasuser', PORT = 22;
const REMOTE_DIR = '/opt/muse2api-go';
const IMG = 'muse2api-go:1.5.4';
const CONTAINER = 'muse2api-go';
const HOST_PORT = '38467';

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
    'ss -ltnp 2>/dev/null | grep -c ":38467" || echo "38467 空闲"',
    // 线上那套的 chromium 版本：Go 镜像必须拿到同一个包，DOM 行为才可比
    'docker exec muse2api chromium --version 2>/dev/null || echo "读不到"',
    'docker exec muse2api python3 -c "import fastapi,starlette,sys;print(f\'py {sys.version.split()[0]} fastapi {fastapi.__version__} starlette {starlette.__version__}\')" 2>&1 | tail -1',
    'docker exec muse2api cat /etc/debian_version 2>/dev/null || echo "读不到"',
    `ls -la ${REMOTE_DIR} 2>/dev/null || echo "目标目录还不存在（正常）"`,
    'nproc; free -m | head -2',
  ],
  push: [],
  build: [
    `cd ${REMOTE_DIR} && docker build -t ${IMG} . 2>&1 | tail -25`,
    `docker images ${IMG} --format "{{.Repository}}:{{.Tag}} {{.Size}}"`,
  ],
  run: [
    `mkdir -p ${REMOTE_DIR}/data-go`,
    // --init：让 docker 自带的最小 init 当 PID 1。Go 进程当 PID 1 时会收养 chromium 的孤儿子孙，
    // 而我们只 Wait() 自己那个直接子进程 —— 每「起一次浏览器再释放」就攒一排僵尸进程。
    `docker rm -f ${CONTAINER} 2>/dev/null; docker run -d --name ${CONTAINER} --restart unless-stopped --init ` +
      `-p ${HOST_PORT}:18610 -v ${REMOTE_DIR}/data-go:/app/data --shm-size=2gb ` +
      `-e MUSE2API_BROWSER_IDLE_MINUTES=${process.env.BROWSER_IDLE_MINUTES || 30} -e MUSE2API_WARM_TAB=0 ${IMG} && sleep 3 && docker ps --filter name=${CONTAINER} --format "{{.Names}} {{.Status}}"`,
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
    const files = [
      ['Dockerfile', 'Dockerfile'],
      ['deploy/version.json', 'deploy/version.json'],
      ['build/muse2api-go', 'build/muse2api-go'],
    ];
    for (const [, rel] of files) {
      if (!fs.existsSync(rel)) {
        console.error(`本地缺 ${rel} —— 先跑：GOOS=linux GOARCH=amd64 CGO_ENABLED=0 ` +
          'go build -trimpath -ldflags="-s -w" -o build/muse2api-go ./cmd/muse2api');
        c.end();
        process.exit(1);
      }
    }
    const dirs = [REMOTE_DIR, `${REMOTE_DIR}/build`, `${REMOTE_DIR}/deploy`];
    const mk = await exec('mkdir -p ' + dirs.map((d) => JSON.stringify(d)).join(' '));
    if (mk.code !== 0) { console.error('mkdir 失败：' + mk.out); c.end(); process.exit(1); }
    for (const [local, rel] of files) {
      process.stdout.write(`上传 ${rel} (${(fs.statSync(local).size / 1048576).toFixed(2)} MB)…\n`);
      const r = await upload(local, `${REMOTE_DIR}/${rel}`);
      if (!r.out.includes('WROTE_OK')) {
        console.error(`  写入失败：${r.out}`);
        c.end();
        process.exit(1);
      }
      const bytes = fs.statSync(local).size;
      const verify = await exec(`wc -c < ${JSON.stringify(`${REMOTE_DIR}/${rel}`)}`);
      const got = Number(String(verify.out).trim());
      console.log(`  本地=${bytes} 远端=${got} ${got === bytes ? 'OK' : '字节不符!'}`);
      if (got !== bytes) { c.end(); process.exit(1); }
    }
    console.log((await exec(`sha256sum ${REMOTE_DIR}/build/muse2api-go`)).out);
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
