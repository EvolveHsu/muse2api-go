package server

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"muse2api-go/internal/pyval"
)

// update.go 对应 app.py 的「仓库实时更新检测」那一节，以及保活协程的状态壳。
//
// 按设计文档偏离清单第 4 条：Go 版不做 git 拉取与镜像重建，
// /admin/update/check 与 /admin/repo/status 仍然照抄（只是读 GitHub，不改磁盘），
// upgrade / pull / push 三条改为返回版本信息 + 手动更新提示。

const repoURL = "https://github.com/czg86389-hub/muse2api"

// 追踪推送到仓库的路径 —— Go 版不推送，留在这里是为了 /admin/repo/status 的展示口径一致。
var trackedRepoPaths = []string{
	"app.py", "engine.py", "store.py", "cdp.py", "config.py",
	"admin.html", "README.md", "version.json", "requirements.txt",
	"Dockerfile", "docker-compose.yml", ".env.example", ".gitignore",
	"LICENSE", "extension", "deploy", "tools",
}

type updateCache struct {
	ts   float64
	data pyval.Obj
}

type keepaliveState struct {
	Running    bool
	LastRun    any
	LastResult any
	NextRun    any
}

// parseVersion 对应 1.5.4 的 _parse_v：`tuple(int(x) for x in re.findall(r"\d+", v))`。
// 只有数字段参与比较，所以 "v1.5.4-beta2" 是 (1,5,4,2)，空串是 ()。
func parseVersion(v string) []int64 {
	var out []int64
	for i := 0; i < len(v); {
		if v[i] < '0' || v[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		n, err := strconv.ParseInt(v[i:j], 10, 64)
		if err != nil {
			// Python 的 int() 任意精度，Go 溢出就退回十进制逐位比大小时用不上；
			// 版本号不可能长到这个量级，按最大值处理保持比较单调。
			n = math.MaxInt64
		}
		out = append(out, n)
		i = j
	}
	return out
}

// versionGT 是 Python 的元组 `>`：逐位比，全部相等时长的算大（() < (0,) < (1,) < (1,5)）。
func versionGT(a, b []int64) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}

func (a *App) checkUpdate(c *reqCtx) (any, error) {
	force := truthy(c.R.URL.Query().Get("force"))
	a.updMu.Lock()
	defer a.updMu.Unlock()
	now := float64(a.now().Unix())
	if !force && a.updCache.data != nil && now-a.updCache.ts < 90 {
		return a.updCache.data, nil
	}

	token := a.readEnvKey("GITHUB_TOKEN")
	localVerObj := readVersionJSON(filepath.Join(a.baseDir(), "version.json"))
	localVersion := strOf(localVerObj["version"])
	if localVersion == "" {
		localVersion = "1.5.0"
	}

	hasGit := isDir(filepath.Join(a.baseDir(), ".git"))
	localSHA, localMsg, localTS := "", "", int64(0)
	if hasGit {
		if out, ok := gitRun(a.baseDir(), "rev-parse", "--short", "HEAD"); ok {
			localSHA = truncate(strings.TrimSpace(out), 7)
		}
		if out, ok := gitRun(a.baseDir(), "log", "-1", "--format=%s||%ct"); ok {
			trimmed := strings.TrimSpace(out)
			if head, tail, found := strings.Cut(trimmed, "||"); found {
				localMsg = head
				localTS, _ = strconv.ParseInt(strings.TrimSpace(tail), 10, 64)
			}
		}
	}
	shaFile := a.installedSHAFile()
	if localSHA == "" {
		if b, err := os.ReadFile(shaFile); err == nil {
			localSHA = truncate(strings.TrimSpace(string(b)), 7)
		}
	}

	remoteVersion := localVersion
	highlights := toAnyList(localVerObj["highlights"])
	if body, ok := httpGet(a, "https://raw.githubusercontent.com/czg86389-hub/muse2api/main/version.json?t="+
		strconv.FormatInt(int64(now), 10), 6, nil); ok {
		var rvj map[string]any
		if json.Unmarshal(body, &rvj) == nil {
			if v := strOf(rvj["version"]); v != "" {
				remoteVersion = v
			}
			if h := toAnyList(rvj["highlights"]); len(h) > 0 {
				highlights = h
			}
		}
	}

	remoteSHA, remoteMsg, remoteTime := "", "", ""
	recent := []any{}
	hdrs := map[string]string{"Accept": "application/vnd.github.v3+json",
		"User-Agent": "muse2api-updater"}
	if token != "" {
		hdrs["Authorization"] = "Bearer " + token
	}
	if body, ok := httpGet(a, "https://api.github.com/repos/czg86389-hub/muse2api/commits?sha=main&per_page=5",
		6, hdrs); ok {
		var commits []any
		if json.Unmarshal(body, &commits) == nil {
			for idx, raw := range commits {
				cm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				sha7 := truncate(strOf(cm["sha"]), 7)
				info, _ := cm["commit"].(map[string]any)
				firstLine := ""
				if info != nil {
					firstLine, _, _ = strings.Cut(strOf(info["message"]), "\n")
				}
				cDate := ""
				if info != nil {
					if comm, ok := info["committer"].(map[string]any); ok {
						cDate = strOf(comm["date"])
					}
				}
				cURL := strOf(cm["html_url"])
				if cURL == "" {
					cURL = repoURL + "/commit/" + sha7
				}
				if idx == 0 {
					remoteSHA, remoteMsg, remoteTime = sha7, firstLine, cDate
				}
				recent = append(recent, pyval.Obj{{"sha", sha7}, {"message", firstLine},
					{"date", cDate}, {"url", cURL}})
			}
		}
	}

	// 通过 Docker/ZIP 部署（没有 .git）且首次运行版本一致时，记录初始基准 SHA。
	if localSHA == "" && remoteSHA != "" && localVersion == remoteVersion {
		localSHA = remoteSHA
		_ = os.WriteFile(shaFile, []byte(remoteSHA), 0o600)
	}

	// 1.5.4 改过的判定：SHA 两边都在时**只看 SHA**（不再因为版本串不同而误报有更新），
	// 只有拿不到成对 SHA 时才比版本号，而且比的是「远端元组 > 本地元组」，不是 !=。
	hasUpdate := false
	if remoteSHA != "" && localSHA != "" {
		hasUpdate = remoteSHA != localSHA
	} else if remoteVersion != "" && localVersion != "" {
		hasUpdate = versionGT(parseVersion(remoteVersion), parseVersion(localVersion))
	}

	data := pyval.Obj{
		{"repo_url", repoURL}, {"has_git", hasGit},
		{"local_version", localVersion}, {"remote_version", remoteVersion},
		{"local_sha", localSHA}, {"local_msg", localMsg}, {"local_ts", localTS},
		{"remote_sha", remoteSHA}, {"remote_msg", remoteMsg}, {"remote_time", remoteTime},
		{"has_update", hasUpdate},
		{"up_to_date", !hasUpdate && (remoteSHA != "" || remoteVersion != "")},
		{"highlights", highlights}, {"recent_commits", recent},
		{"checked_at", int64(now)},
	}
	a.updCache = updateCache{ts: now, data: data}
	return data, nil
}

// upgradeManual / repoPushManual 是偏离清单第 4 条的落地点：不做在线升级、不推仓库。
func (a *App) upgradeManual(c *reqCtx) (any, error) {
	if _, err := decodeBodyWithDefault(c.R); err != nil {
		return nil, err
	}
	status, _ := a.checkUpdate(c)
	return pyval.Obj{
		{"ok", false}, {"via", "manual"},
		{"message", "Go 版不执行在线拉取与镜像重建：请在 NAS 上按 docs/DEPLOY-go.md 重新构建镜像后重启容器"},
		{"status", status},
	}, nil
}

func (a *App) repoPushManual(c *reqCtx) (any, error) {
	// Python 的签名是 `payload: dict = Body(default={})`：body 校验照做（[] / 坏 JSON /
	// 非 JSON Content-Type 都各有结局），只是这份实现用不上里面的字段。
	if _, err := decodeBodyWithDefault(c.R); err != nil {
		return nil, err
	}
	return pyval.Obj{
		{"ok", false}, {"committed", false},
		{"message", "Go 版是单二进制部署，不向 GitHub 回推代码；需要改代码请改 Python 源仓库或 Go 源码"},
	}, nil
}

func (a *App) installedSHAFile() string {
	return filepath.Join(a.CFG.DataDir(), ".installed_sha")
}

func (a *App) baseDir() string { return a.CFG.BaseDir }

// readEnvKey 先看进程环境，再看 .env（与 Python 一致）。
func (a *App) readEnvKey(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	raw, err := os.ReadFile(filepath.Join(a.CFG.BaseDir, ".env"))
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, key+"=") {
			return strings.TrimSpace(s[len(key)+1:])
		}
	}
	return ""
}

func readVersionJSON(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"version": "1.5.0", "highlights": []any{}}
	}
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return map[string]any{"version": "1.5.0", "highlights": []any{}}
	}
	_ = dec
	return obj
}

func toAnyList(v any) []any {
	if l, ok := v.([]any); ok {
		return append([]any(nil), l...)
	}
	return []any{}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

func httpGet(a *App, url string, timeoutSec int, hdrs map[string]string) ([]byte, bool) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	cli := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := readAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return body, true
}

// gitRun 跑一条只读 git 命令。对应 _git()：带 GIT_TERMINAL_PROMPT=0 与 safe.directory。
func gitRun(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", append([]string{"-c", "safe.directory=" + dir}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}
