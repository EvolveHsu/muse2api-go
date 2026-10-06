// 把「打到现役 Python 服务的一批请求 + docker logs 里 uvicorn 打出的原始行」合成 access log 金样本。
//
//   node tools/golden/ingest_accesslog.mjs <抓到的日志文本> <写出的 json>
//
// 为什么需要 sent：uvicorn 打的不是裸请求行 —— 实测 `%2F` 会被还原成 `/`，而 `%252F`、`%20`、
// `/./`、`//` 原样保留（等价于 quote(unquote(target))）。这条规则只能靠真机录，不能推断，
// 所以金样本同时存「线上真实发出去的 target」与「Python 记下的 target」，Go 用前者驱动、比对后者。
import fs from 'node:fs';

const [inPath, outPath] = process.argv.slice(2);
if (!inPath || !outPath) {
  console.error('用法: ingest_accesslog.mjs <日志文本> <输出 json>');
  process.exit(2);
}

// 探针 id → 线上实际发出的请求（method、target、带不带 Key、body）。
// 逐条对应录制命令里的 curl 参数；回放时 Go 必须发同一份请求，状态码才可能一致。
const SENT = {
  a1: ['GET', '/healthz?probe=a1', false, ''],
  a2: ['GET', '/v1/models?probe=a2', false, ''],
  a3: ['GET', '/v1/nope?probe=a3', true, ''],
  a4: ['HEAD', '/v1/models?probe=a4', false, ''],
  a5: ['GET', '/admin?probe=a5', false, ''],
  a6: ['POST', '/v1/images/generations?probe=a6', true, '{"prompt":"x","timeout":999}'],
  a7: ['GET', '/admin/nope?probe=a7', true, ''],
  a8: ['POST', '/v1/chat/completions?probe=a8', true, '[]'],
  a9: ['GET', '/openapi.json?probe=a9', false, ''],
  a10: ['GET', '/docs?probe=a10', false, ''],
  a11: ['GET', '/admin/cookie-helper?probe=a11', false, ''],
  a12: ['GET', '/v1/media/definitely-not-there.png?probe=a12', false, ''],
  a13: ['POST', '/v1/images/edits?probe=a13', true, '{bad json'],
  a14: ['GET', '/redoc?probe=a14', false, ''],
  a15: ['PATCH', '/v1/models?probe=a15', false, ''],
  a16: ['GET', '/v1/media/a%20b.png?probe=a16', false, ''],
  a17: ['GET', '/v1/media/%2F.png?probe=a17', false, ''],
  a18: ['GET', '/v1/models/?probe=a18', false, ''],
  a19: ['POST', '/v1/chat/completions?probe=a19', false, '{}'],
  a20: ['GET', '/admin/tasks?limit=abc&probe=a20', true, ''],
  w1: ['GET', '/v1/media/%2F.png?probe=w1', false, ''],
  w2: ['GET', '/v1/media/%252F.png?probe=w2', false, ''],
  w3: ['GET', '/v1/media/a%20b.png?probe=w3', false, ''],
  w4: ['GET', '/v1/./models?probe=w4', false, ''],
  w5: ['GET', '/v1//models?probe=w5', false, ''],
  w6: ['GET', '/v1/media/%E7%8C%AB.png?probe=w6', false, ''],
  w7: ['GET', '/v1/media/%2e%2e/data/accounts.json?probe=w7', false, ''],
};

const LINE = /^(\S+: *)(\S+:\d+) - "([A-Z]+) (\S+) (HTTP\/[\d.]+)" (\d{3}) (.*)$/;
// 文档保留地址（RFC 5737），用来替换录制里出现的真实 client。
const CLIENT_REDACT = '203.0.113.10:40000';
const entries = [];
const seen = new Map();
let prefix = null;
for (const raw of fs.readFileSync(inPath, 'utf8').split('\n')) {
  const line = raw.replace(/\r$/, '');
  const m = LINE.exec(line);
  if (!m) continue;
  const [, pfx, addr, method, target, proto, status, phrase] = m;
  const pm = /[?&]probe=([a-z0-9]+)$/.exec(target);
  if (!pm) continue;
  const sent = SENT[pm[1]];
  if (!sent) continue;
  if (prefix === null) prefix = pfx;
  else if (prefix !== pfx) console.error('前缀不一致：' + JSON.stringify(pfx) + ' vs ' + JSON.stringify(prefix));
  // client 地址一律换成 RFC 5737 的文档地址：金样本会随源码发布，不该带上家里的内网/网桥地址。
  // 回放只比 ` - ` 之后的部分，client 由各自的请求给出，所以这个替换不影响校验强度
  // （IPv6 那条另有 TestAccessLineIPv6 专门钉写法）。原始形态记在 client_addr_shape 里。
  const shape = /^\d+\.\d+\.\d+\.\d+:\d+$/.test(addr) ? 'ipv4:port'
    : (/^\[[0-9a-fA-F:]+\]:\d+$/.test(addr) ? 'ipv6-bracketed:port' : String(addr));
  const entry = {
    probe: pm[1],
    sent_method: sent[0],
    sent_target: sent[1],
    sent_auth: Boolean(sent[2]),
    sent_body: sent[3],
    logged_method: method,
    logged_target: target,
    logged_proto: proto,
    logged_status: Number(status),
    logged_phrase: phrase,
    client_addr_shape: shape,
    line: line.replace(addr, CLIENT_REDACT),
  };
  // 同一个探针可能被多轮抓取重复命中（docker logs --since 的窗口会重叠）：
  // 只留第一条，否则回放表里出现同名两条用例，台账也数不清。
  if (seen.has(entry.probe)) {
    const prev = seen.get(entry.probe);
    if (prev.logged_status !== entry.logged_status || prev.logged_target !== entry.logged_target) {
      console.error(`同一探针两次结果不同：${entry.probe} ${prev.line} vs ${entry.line}`);
      process.exitCode = 1;
    }
    continue;
  }
  seen.set(entry.probe, entry);
  entries.push(entry);
}

if (!entries.length) {
  console.error('一条 access log 都没解析出来，检查抓取文本');
  process.exit(1);
}
const missing = Object.keys(SENT).filter((k) => !entries.some((e) => e.probe === k));
if (missing.length) console.error('这些探针没出现在抓取里：' + missing.join(','));

const out = { prefix, entries };
fs.writeFileSync(outPath, JSON.stringify(out, null, 2) + '\n');
console.log('写出', outPath, 'prefix', JSON.stringify(prefix), 'entries', entries.length);
for (const e of entries) {
  if (e.sent_target !== e.logged_target) {
    console.log('  规范化差异', e.probe, e.sent_target, '→', e.logged_target);
  }
}
